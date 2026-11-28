package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/rcss-labs/yamdrake/internal/manifest"
)

// planView is the manifest as the web application reads it. Artifact keys are
// not part of it. A restore's plan also carries restore_of (the run it
// restores), each service's target_task_definition and, for a config file it
// leaves as it is, not_restored (the reason, in words). A deploy's plan has
// none of the three.
type planView struct {
	ID              string            `json:"id"`
	RunID           int64             `json:"run_id"`
	Environment     string            `json:"environment"`
	Label           string            `json:"label"`
	AWSAccount      string            `json:"aws_account"`
	InventoryCommit string            `json:"inventory_commit"`
	JobCommit       string            `json:"job_commit"`
	RestoreOf       *int64            `json:"restore_of,omitempty"`
	Services        []planServiceView `json:"services"`
	Checks          []planCheckView   `json:"checks"`
}

type planServiceView struct {
	Name                   string           `json:"name"`
	Group                  string           `json:"group"`
	Version                string           `json:"version"`
	ImageDigest            string           `json:"image_digest"`
	ECSService             string           `json:"ecs_service"`
	ECRRepository          string           `json:"ecr_repository"`
	PreviousTaskDefinition string           `json:"previous_task_definition"`
	PreviousVersion        string           `json:"previous_version"`
	TargetTaskDefinition   string           `json:"target_task_definition,omitempty"`
	Config                 []planConfigView `json:"config"`
}

type planConfigView struct {
	Kind            string       `json:"kind"`
	Name            string       `json:"name"`
	Source          string       `json:"source"`
	Destination     string       `json:"destination"`
	Branch          string       `json:"branch,omitempty"`
	Commit          string       `json:"commit,omitempty"`
	Fallback        bool         `json:"fallback,omitempty"`
	SourceVersionID string       `json:"source_version_id,omitempty"`
	SHA256          string       `json:"sha256"`
	Size            int64        `json:"size"`
	PreviousExists  bool         `json:"previous_exists"`
	PreviousSHA256  string       `json:"previous_sha256,omitempty"`
	Change          string       `json:"change"`
	Keys            *planKeyDiff `json:"keys,omitempty"`
	NotRestored     string       `json:"not_restored,omitempty"` // why a restore leaves this item alone
}

type planKeyDiff struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
}

type planCheckView struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
}

// getRunPlan serves the run's manifest without artifact keys. The view is
// built field by field, so a field added to the manifest later is not served
// until someone adds it here. Env-file values are never in a manifest.
func (s *server) getRunPlan(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runFromPath(w, r)
	if !ok {
		return
	}
	if len(run.Manifest) == 0 {
		notFound(w, "plan")
		return
	}
	var m manifest.Manifest
	if err := json.Unmarshal(run.Manifest, &m); err != nil {
		s.internalError(w, r, fmt.Errorf("run %d has a stored manifest that does not decode: %w", run.ID, err))
		return
	}
	view := planOf(m)
	view.ID = run.ManifestID // the identity the engine stored, whatever the manifest's own id field says
	writeJSON(w, http.StatusOK, view)
}

// planOf builds the served view; every list is non-nil.
func planOf(m manifest.Manifest) planView {
	view := planView{
		ID: m.ID, RunID: m.RunID, Environment: m.Environment, Label: m.Label,
		AWSAccount: m.AWSAccount, InventoryCommit: m.InventoryCommit, JobCommit: m.JobCommit,
		Services: make([]planServiceView, 0, len(m.Services)),
		Checks:   make([]planCheckView, 0, len(m.Checks)),
	}
	if m.RestoreOf > 0 {
		view.RestoreOf = &m.RestoreOf
	}
	for _, s := range m.Services {
		sv := planServiceView{
			Name: s.Name, Group: s.Group, Version: s.Version, ImageDigest: s.ImageDigest,
			ECSService: s.ECSService, ECRRepository: s.ECRRepository,
			PreviousTaskDefinition: s.PreviousTaskDefinition, PreviousVersion: s.PreviousVersion,
			TargetTaskDefinition: s.TargetTaskDefinition,
			Config:               make([]planConfigView, 0, len(s.Config)),
		}
		for _, c := range s.Config {
			cv := planConfigView{
				Kind: c.Kind, Name: c.Name, Source: c.Source, Destination: c.Destination,
				Branch: c.Branch, Commit: c.Commit, Fallback: c.Fallback, SourceVersionID: c.SourceVersionID,
				SHA256: c.SHA256, Size: c.Size, PreviousExists: c.PreviousExists, PreviousSHA256: c.PreviousSHA256,
				Change: c.Change, NotRestored: c.NotRestored,
			}
			if c.Keys != nil {
				cv.Keys = &planKeyDiff{Added: nonNil(c.Keys.Added), Removed: nonNil(c.Keys.Removed), Changed: nonNil(c.Keys.Changed)}
			}
			sv.Config = append(sv.Config, cv)
		}
		view.Services = append(view.Services, sv)
	}
	for _, c := range m.Checks {
		view.Checks = append(view.Checks, planCheckView{Name: c.Name, Service: c.Service, Status: c.Status, Detail: c.Detail})
	}
	return view
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
