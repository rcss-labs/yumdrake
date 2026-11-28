package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/running"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// serviceHistoryLimit is how many deployments and config changes the service
// page lists.
const serviceHistoryLimit = 50

// checksumLength is how many characters of a config item's sha256 the page shows.
const checksumLength = 12

func (s *server) serviceRoutes() []routeDef {
	return []routeDef{
		{"GET /api/v1/services/{name}", auth.View, s.getServicePage},
	}
}

type serviceEnvironmentView struct {
	Environment         string       `json:"environment"`
	Label               string       `json:"label"`
	Cell                running.Cell `json:"cell"`
	RestoreFromRunID    *int64       `json:"restore_from_run_id"`
	RestoreFromFolderID *int64       `json:"restore_from_folder_id"`
}

type serviceDeploymentView struct {
	RunID       int64     `json:"run_id"`
	LegID       int64     `json:"leg_id"`
	Environment string    `json:"environment"`
	Version     string    `json:"version"`
	State       string    `json:"state"`
	At          time.Time `json:"at"`
	By          string    `json:"by"`
}

type serviceConfigItemView struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Checksum    string `json:"checksum"`
}

type serviceConfigView struct {
	RunID       int64                   `json:"run_id"`
	Environment string                  `json:"environment"`
	At          time.Time               `json:"at"`
	Items       []serviceConfigItemView `json:"items"`
}

type servicePageResponse struct {
	Service      serviceDetail            `json:"service"`
	Environments []serviceEnvironmentView `json:"environments"`
	Deployments  []serviceDeploymentView  `json:"deployments"`
	Config       []serviceConfigView      `json:"config"`
}

// getServicePage answers GET /api/v1/services/{name}: the inventory entry,
// what runs in each environment, the last deployments and config changes, and
// where a restore could start from.
func (s *server) getServicePage(w http.ResponseWriter, r *http.Request) {
	ctx, name := r.Context(), r.PathValue("name")
	_, inventory, snapshot, active, err := s.inventoryView(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	entry, found := inventory.Services[name]
	if !active || !found {
		notFound(w, "service")
		return
	}
	if s.Running == nil {
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "running versions are not available")
		return
	}
	cells, err := s.Running.ForService(ctx, inventory, name)
	if err != nil {
		if errors.Is(err, running.ErrUnknownService) {
			notFound(w, "service")
			return
		}
		s.internalError(w, r, err)
		return
	}
	restore, err := store.RestoreSources(ctx, s.Pool, name)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	deployments, err := store.ServiceDeployments(ctx, s.Pool, name, serviceHistoryLimit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	configRuns, err := store.ServiceConfigRuns(ctx, s.Pool, name, serviceHistoryLimit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	out := servicePageResponse{Service: newServiceDetail(entry, snapshot), Environments: make([]serviceEnvironmentView, 0, len(inventory.Environments)),
		Deployments: make([]serviceDeploymentView, 0, len(deployments)), Config: make([]serviceConfigView, 0, len(configRuns))}
	for i, env := range inventory.Environments {
		view := serviceEnvironmentView{Environment: env.Name, Label: env.Label}
		if i < len(cells) {
			view.Cell = cells[i]
		}
		if source, ok := restore[env.Name]; ok {
			runID, folderID := source.RunID, source.FolderID
			view.RestoreFromRunID, view.RestoreFromFolderID = &runID, &folderID
		}
		out.Environments = append(out.Environments, view)
	}
	for _, d := range deployments {
		out.Deployments = append(out.Deployments, serviceDeploymentView{RunID: d.RunID, LegID: d.LegID, Environment: d.Environment,
			Version: d.Version, State: d.State, At: d.At, By: d.By})
	}
	for _, c := range configRuns {
		var entry manifest.Service
		if err := json.Unmarshal(c.Service, &entry); err != nil {
			s.internalError(w, r, err)
			return
		}
		change := serviceConfigView{RunID: c.RunID, Environment: c.Environment, At: c.At, Items: make([]serviceConfigItemView, 0, len(entry.Config))}
		for _, item := range entry.Config {
			change.Items = append(change.Items, serviceConfigItemView{Kind: item.Kind, Name: item.Name, Destination: item.Destination,
				Checksum: item.SHA256[:min(len(item.SHA256), checksumLength)]})
		}
		out.Config = append(out.Config, change)
	}
	writeJSON(w, http.StatusOK, out)
}
