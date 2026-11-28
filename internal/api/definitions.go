package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rcss-labs/yamdrake/internal/audit"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
	yumsync "github.com/rcss-labs/yamdrake/internal/sync"
)

type inventoryResponse struct {
	// Commit and ActivatedAt describe the active snapshot (empty and null
	// before the first activation), so the age of the last good sync is visible.
	Commit       string               `json:"commit"`
	ActivatedAt  *time.Time           `json:"activated_at"`
	CheckedAt    *time.Time           `json:"checked_at"`
	SyncError    string               `json:"sync_error"`
	SyncErrorAt  *time.Time           `json:"sync_error_at"`
	Rejected     *rejected            `json:"rejected"`
	Environments []environmentSummary `json:"environments"`
	Services     []serviceSummary     `json:"services"`
}

// rejected is a newer commit that failed validation; the active one stays.
type rejected struct {
	Commit   string        `json:"commit"`
	Problems spec.Problems `json:"problems"`
}

type environmentSummary struct {
	Name       string `json:"name"`
	Label      string `json:"label"`
	AWSAccount string `json:"aws_account"`
}

type serviceSummary struct {
	Name         string   `json:"name"`
	Environments []string `json:"environments"`
}

// inventoryView builds the response from the stored snapshot and status.
func (s *server) inventoryView(ctx context.Context) (inventoryResponse, spec.Inventory, store.Snapshot, bool, error) {
	status, err := store.GetInventoryStatus(ctx, s.Pool)
	if err != nil {
		return inventoryResponse{}, spec.Inventory{}, store.Snapshot{}, false, err
	}
	out := inventoryResponse{
		CheckedAt: status.CheckedAt, SyncError: status.SyncError, SyncErrorAt: status.SyncErrorAt,
		Environments: []environmentSummary{}, Services: []serviceSummary{},
	}
	if status.Problems != nil && status.ProblemsCommit != "" {
		var problems spec.Problems
		if err := json.Unmarshal(status.Problems, &problems); err != nil {
			return inventoryResponse{}, spec.Inventory{}, store.Snapshot{}, false, fmt.Errorf("decode inventory problems: %w", err)
		}
		if problems == nil {
			problems = spec.Problems{}
		}
		out.Rejected = &rejected{Commit: status.ProblemsCommit, Problems: problems}
	}
	snapshot, active, err := store.ActiveSnapshot(ctx, s.Pool)
	if err != nil || !active {
		return out, spec.Inventory{}, store.Snapshot{}, false, err
	}
	var inventory spec.Inventory
	if err := json.Unmarshal(snapshot.Inventory, &inventory); err != nil {
		return inventoryResponse{}, spec.Inventory{}, store.Snapshot{}, false, fmt.Errorf("decode active inventory: %w", err)
	}
	out.Commit, out.ActivatedAt = snapshot.Commit, &snapshot.ActivatedAt
	for _, environment := range inventory.Environments {
		out.Environments = append(out.Environments, environmentSummary{
			Name: environment.Name, Label: environment.Label, AWSAccount: environment.AWSAccount})
	}
	for _, name := range inventory.ServiceNames() {
		summary := serviceSummary{Name: name, Environments: []string{}}
		for _, environment := range inventory.Environments {
			if _, ok := inventory.Services[name].Environments[environment.Name]; ok {
				summary.Environments = append(summary.Environments, environment.Name)
			}
		}
		out.Services = append(out.Services, summary)
	}
	return out, inventory, snapshot, true, nil
}

func (s *server) getInventory(w http.ResponseWriter, r *http.Request) {
	view, _, _, _, err := s.inventoryView(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *server) getService(w http.ResponseWriter, r *http.Request) {
	_, inventory, snapshot, active, err := s.inventoryView(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	service, found := inventory.Services[r.PathValue("name")]
	if !active || !found {
		notFound(w, "service")
		return
	}
	writeJSON(w, http.StatusOK, newServiceDetail(service, snapshot))
}

// newServiceDetail is the view of one inventory entry: the same answer the
// inventory route and the service page give.
func newServiceDetail(service spec.Service, snapshot store.Snapshot) serviceDetail {
	withEmptyLists(reflect.ValueOf(&service))
	// The text comes from the same snapshot as the parsed service.
	return serviceDetail{Service: service, Source: snapshot.Sources[service.File]}
}

// serviceDetail is a service with the text of its file at the active commit
// ("" for a snapshot stored before the text was kept).
type serviceDetail struct {
	spec.Service
	Source string `json:"source"`
}

func (s *server) getInventorySource(w http.ResponseWriter, r *http.Request) {
	source, _, err := store.GetInventorySource(r.Context(), s.Pool)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	// Nothing stored reads as empty strings, so the form shows empty fields.
	writeJSON(w, http.StatusOK, sourceRequest{Repo: source.Repo, Branch: source.Branch, Path: source.Path})
}

// syncFailed handles the error of an on-demand sync and reports whether it
// wrote a response. A source error (yumsync.ErrSource) is already stored as
// sync_error and logged by the engine, and the response that follows shows it;
// anything else, including an ended request, is an internal failure.
func (s *server) syncFailed(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil || (errors.Is(err, yumsync.ErrSource) && r.Context().Err() == nil) {
		return false
	}
	s.internalError(w, r, fmt.Errorf("on-demand sync: %w", err))
	return true
}

func (s *server) syncInventory(w http.ResponseWriter, r *http.Request) {
	if err := s.recordAudit(r.Context(), s.Pool, r, audit.Event{Action: "inventory.sync", TargetType: "inventory_source", TargetID: "inventory"}); err != nil {
		s.internalError(w, r, err)
		return
	}
	if s.syncFailed(w, r, s.Engine.SyncInventory(r.Context())) {
		return
	}
	s.getInventory(w, r)
}

// validRepo accepts "owner/name" with no spaces.
func validRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	return ok && owner != "" && name != "" && !strings.ContainsAny(repo, " \t\r\n") && strings.Count(repo, "/") == 1
}

type sourceRequest struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Path   string `json:"path"`
}

func (q sourceRequest) check() string {
	switch {
	case !validRepo(q.Repo):
		return fmt.Sprintf("repo %q must look like owner/name", q.Repo)
	case strings.TrimSpace(q.Branch) == "":
		return "branch is required"
	}
	return ""
}

func (s *server) putInventorySource(w http.ResponseWriter, r *http.Request) {
	var req sourceRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if problem := req.check(); problem != "" {
		invalid(w, problem)
		return
	}
	// Stored and synced under the sync lock, so a poll in flight cannot write a
	// status for the old source after the change. The audit entry is written in
	// the transaction that stores the source.
	source := store.InventorySource{Repo: req.Repo, Branch: req.Branch, Path: req.Path}
	record := func(ctx context.Context, tx store.DBTX) error {
		return s.recordAudit(ctx, tx, r, audit.Event{Action: "inventory.source_set", TargetType: "inventory_source", TargetID: "inventory",
			Detail: map[string]any{"repo": source.Repo, "branch": source.Branch, "path": source.Path}})
	}
	if s.syncFailed(w, r, s.Engine.SetInventorySource(r.Context(), source, record)) {
		return
	}
	s.getInventory(w, r)
}

type folderResponse struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (s *server) listFolders(w http.ResponseWriter, r *http.Request) {
	folders, err := store.ListFolders(r.Context(), s.Pool)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]folderResponse, 0, len(folders))
	for _, f := range folders {
		out = append(out, folderResponse{ID: f.ID, Name: f.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": out})
}

const maxFolderNameLength = 100

func (s *server) createFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > maxFolderNameLength {
		invalid(w, fmt.Sprintf("name is required and at most %d characters", maxFolderNameLength))
		return
	}
	var folder store.Folder
	err := s.auditedWrite(r, func(ctx context.Context, tx pgx.Tx) (audit.Event, error) {
		var err error
		folder, err = store.CreateFolder(ctx, tx, name)
		return audit.Event{Action: "folder.create", TargetType: "folder", TargetID: strconv.FormatInt(folder.ID, 10),
			Detail: map[string]any{"name": name}}, err
	})
	switch {
	case errors.Is(err, store.ErrDuplicate):
		writeError(w, http.StatusConflict, codeConflict, fmt.Sprintf("a folder named %q already exists", name))
	case err != nil:
		s.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, folderResponse{ID: folder.ID, Name: folder.Name})
	}
}

type jobSummary struct {
	ID           int64      `json:"id"`
	JobID        string     `json:"job_id"`
	Title        string     `json:"title"`
	FolderID     int64      `json:"folder_id"`
	Folder       string     `json:"folder"`
	Repo         string     `json:"repo"`
	Branch       string     `json:"branch"`
	Path         string     `json:"path"`
	Runnable     bool       `json:"runnable"`
	Commit       string     `json:"commit"`
	SyncedAt     *time.Time `json:"synced_at"`
	CheckedAt    *time.Time `json:"checked_at"`
	ProblemCount int        `json:"problem_count"`
	SyncError    string     `json:"sync_error"`
	SyncErrorAt  *time.Time `json:"sync_error_at"`
	// Services counts the distinct services the last good definition deploys
	// across its groups, and ApprovalLabels lists the environment labels that
	// need approval. Both are empty (0 and []) until the first good read.
	Services       int      `json:"services"`
	ApprovalLabels []string `json:"approval_labels"`
}

type jobDetail struct {
	jobSummary
	// Definition is the last good definition (null until the first good read).
	Definition json.RawMessage `json:"definition"`
	// Source is the YAML text of the last good commit ("" until the first good read).
	Source         string        `json:"source"`
	Problems       spec.Problems `json:"problems"`
	ProblemsCommit string        `json:"problems_commit"`
}

func summarize(job store.JobSource, folderNames map[int64]string) (jobSummary, spec.Problems, error) {
	var problems spec.Problems
	if job.Problems != nil {
		if err := json.Unmarshal(job.Problems, &problems); err != nil {
			return jobSummary{}, nil, fmt.Errorf("decode problems of job source %d: %w", job.ID, err)
		}
	}
	services, approvalLabels := 0, []string{}
	if job.LastGood != nil {
		var definition spec.Job
		if err := json.Unmarshal(job.LastGood, &definition); err != nil {
			return jobSummary{}, nil, fmt.Errorf("decode last good of job source %d: %w", job.ID, err)
		}
		services = distinctServices(definition)
		if definition.Approval != nil && definition.Approval.Labels != nil {
			approvalLabels = definition.Approval.Labels
		}
	}
	return jobSummary{
		Services: services, ApprovalLabels: approvalLabels,
		ID: job.ID, JobID: job.JobID, Title: job.Title, FolderID: job.FolderID, Folder: folderNames[job.FolderID],
		Repo: job.Repo, Branch: job.Branch, Path: job.Path, Runnable: job.Runnable, Commit: job.LastGoodCommit,
		SyncedAt: job.SyncedAt, CheckedAt: job.CheckedAt, ProblemCount: len(problems),
		SyncError: job.SyncError, SyncErrorAt: job.SyncErrorAt,
	}, problems, nil
}

// distinctServices counts the services named by any group once.
func distinctServices(job spec.Job) int {
	seen := map[string]bool{}
	for _, group := range job.Groups {
		for _, name := range group.Services {
			seen[name] = true
		}
	}
	return len(seen)
}

func (s *server) folderNames(ctx context.Context) (map[int64]string, error) {
	folders, err := store.ListFolders(ctx, s.Pool)
	if err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(folders))
	for _, f := range folders {
		names[f.ID] = f.Name
	}
	return names, nil
}

// detail loads one job source with its folder name.
func (s *server) detail(ctx context.Context, id int64) (jobDetail, error) {
	job, err := store.GetJobSource(ctx, s.Pool, id)
	if err != nil {
		return jobDetail{}, err
	}
	names, err := s.folderNames(ctx)
	if err != nil {
		return jobDetail{}, err
	}
	summary, problems, err := summarize(job, names)
	if err != nil {
		return jobDetail{}, err
	}
	if problems == nil {
		problems = spec.Problems{}
	}
	definition, err := emptyLists[spec.Job](job.LastGood)
	if err != nil {
		return jobDetail{}, fmt.Errorf("job source %d: %w", id, err)
	}
	return jobDetail{jobSummary: summary, Definition: definition, Source: job.LastGoodSource, Problems: problems, ProblemsCommit: job.ProblemsCommit}, nil
}

// respondDetail writes the job source id, or 404 when it does not exist.
func (s *server) respondDetail(w http.ResponseWriter, r *http.Request, id int64, status int) {
	detail, err := s.detail(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w, "job")
	case err != nil:
		s.internalError(w, r, err)
	default:
		writeJSON(w, status, detail)
	}
}

func (s *server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := store.ListJobSources(r.Context(), s.Pool)
	if err == nil {
		var names map[int64]string
		if names, err = s.folderNames(r.Context()); err == nil {
			out := make([]jobSummary, 0, len(jobs))
			for _, job := range jobs {
				var summary jobSummary
				if summary, _, err = summarize(job, names); err != nil {
					break
				}
				out = append(out, summary)
			}
			if err == nil {
				writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
				return
			}
		}
	}
	s.internalError(w, r, err)
}

func (s *server) getJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		notFound(w, "job")
		return
	}
	s.respondDetail(w, r, id, http.StatusOK)
}

func unknownFolder(w http.ResponseWriter, folderID int64) {
	invalid(w, fmt.Sprintf("folder %d does not exist", folderID))
}

func (s *server) registerJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FolderID int64 `json:"folder_id"`
		sourceRequest
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.FolderID <= 0 {
		invalid(w, "folder_id is required")
		return
	}
	if problem := req.check(); problem != "" {
		invalid(w, problem)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		invalid(w, "path is required")
		return
	}
	var created store.JobSource
	err := s.auditedWrite(r, func(ctx context.Context, tx pgx.Tx) (audit.Event, error) {
		var err error
		created, err = store.CreateJobSource(ctx, tx, store.JobSource{
			FolderID: req.FolderID, Repo: req.Repo, Branch: req.Branch, Path: req.Path})
		return audit.Event{Action: "job.register", TargetType: "job_source", TargetID: strconv.FormatInt(created.ID, 10),
			Detail: map[string]any{"folder_id": req.FolderID, "repo": req.Repo, "branch": req.Branch, "path": req.Path}}, err
	})
	switch {
	case errors.Is(err, store.ErrDuplicate):
		writeError(w, http.StatusConflict, codeConflict,
			fmt.Sprintf("%s on branch %s at %s is already registered", req.Repo, req.Branch, req.Path))
		return
	case errors.Is(err, store.ErrInvalidReference):
		unknownFolder(w, req.FolderID)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	if s.syncFailed(w, r, s.Engine.SyncJob(r.Context(), created.ID)) {
		return
	}
	s.respondDetail(w, r, created.ID, http.StatusCreated)
}

func (s *server) moveJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		notFound(w, "job")
		return
	}
	var req struct {
		FolderID int64 `json:"folder_id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.FolderID <= 0 {
		invalid(w, "folder_id is required")
		return
	}
	err := s.auditedWrite(r, func(ctx context.Context, tx pgx.Tx) (audit.Event, error) {
		return audit.Event{Action: "job.move", TargetType: "job_source", TargetID: strconv.FormatInt(id, 10),
			Detail: map[string]any{"folder_id": req.FolderID}}, store.MoveJobSource(ctx, tx, id, req.FolderID)
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w, "job")
	case errors.Is(err, store.ErrInvalidReference):
		unknownFolder(w, req.FolderID)
	case err != nil:
		s.internalError(w, r, err)
	default:
		s.respondDetail(w, r, id, http.StatusOK)
	}
}

func (s *server) syncJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		notFound(w, "job")
		return
	}
	if _, err := store.GetJobSource(r.Context(), s.Pool, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound(w, "job")
		} else {
			s.internalError(w, r, err)
		}
		return
	}
	if err := s.recordAudit(r.Context(), s.Pool, r, audit.Event{Action: "job.sync", TargetType: "job_source", TargetID: strconv.FormatInt(id, 10)}); err != nil {
		s.internalError(w, r, err)
		return
	}
	if s.syncFailed(w, r, s.Engine.SyncJob(r.Context(), id)) {
		return
	}
	s.respondDetail(w, r, id, http.StatusOK)
}
