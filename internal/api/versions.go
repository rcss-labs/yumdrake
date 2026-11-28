package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/running"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/versions"
)

// VersionLister is implemented by *versions.Service.
type VersionLister interface {
	ForJob(ctx context.Context, job spec.Job, inv spec.Inventory, envName string) (versions.JobVersions, error)
}

// RunningReader is implemented by *running.Service.
type RunningReader interface {
	Matrix(ctx context.Context, inv spec.Inventory) (running.Matrix, error)
	MatrixFresh(ctx context.Context, inv spec.Inventory) (running.Matrix, error)
	ForService(ctx context.Context, inv spec.Inventory, name string) ([]running.Cell, error)
}

// connectionChecks records how reading an AWS account went, for the System
// health page. A failure to record is logged and never fails the read.
type connectionChecks struct {
	pool store.DBTX
	log  *slog.Logger
}

func (c connectionChecks) OK(ctx context.Context, kind, ref string, at time.Time) {
	if err := store.RecordConnectionOK(ctx, c.pool, kind, ref, at); err != nil {
		c.log.Warn("could not record a connection check", "kind", kind, "ref", ref, "error", err.Error())
	}
}

func (c connectionChecks) Error(ctx context.Context, kind, ref, code string, at time.Time) {
	if err := store.RecordConnectionError(ctx, c.pool, kind, ref, code, at); err != nil {
		c.log.Warn("could not record a connection check", "kind", kind, "ref", ref, "error", err.Error())
	}
}

// getJobVersions answers GET /api/v1/jobs/{id}/versions?environment=<name>:
// the image versions each service of the job's groups can be deployed at.
func (s *server) getJobVersions(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		notFound(w, "job")
		return
	}
	envName := r.URL.Query().Get("environment")
	if envName == "" {
		invalid(w, "environment is required")
		return
	}
	source, err := store.GetJobSource(r.Context(), s.Pool, id)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, "job")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if s.Versions == nil {
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "version lists are not available")
		return
	}
	_, inventory, _, active, err := s.inventoryView(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !active || len(source.LastGood) == 0 {
		writeError(w, http.StatusConflict, codeConflict, "the job or the inventory has no valid definition yet")
		return
	}
	if _, known := inventory.Environment(envName); !known {
		invalid(w, "unknown environment")
		return
	}
	var job spec.Job
	if err := json.Unmarshal(source.LastGood, &job); err != nil {
		s.internalError(w, r, err)
		return
	}
	result, err := s.Versions.ForJob(r.Context(), job, inventory, envName)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) versionRoutes() []routeDef {
	return []routeDef{
		{"GET /api/v1/versions/running", auth.View, s.getRunningVersions},
	}
}

// getRunningVersions answers GET /api/v1/versions/running: the version each
// service runs in each environment. ?refresh=1 reads AWS again instead of the
// cache, and needs manage_connections.
func (s *server) getRunningVersions(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	if refresh {
		principal, ok := PrincipalFrom(r.Context())
		if !ok {
			unauthorized(w)
			return
		}
		if !principal.Can(auth.ManageConnections, auth.Target{}) {
			writeError(w, http.StatusForbidden, codeForbidden, "you do not have permission to do this")
			return
		}
	}
	if s.Running == nil {
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "running versions are not available")
		return
	}
	_, inventory, _, active, err := s.inventoryView(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !active {
		writeError(w, http.StatusConflict, codeConflict, "the inventory has no valid definition yet")
		return
	}
	read := s.Running.Matrix
	if refresh {
		read = s.Running.MatrixFresh
	}
	matrix, err := read(r.Context(), inventory)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, matrix)
}
