package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/engine"
	"github.com/rcss-labs/yamdrake/internal/logs"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
)

const (
	maxRunListLimit = 100
	maxLogLimit     = 1000
)

var (
	runStates = []string{"resolving", "planned", "waiting_approval", "queued", "running", "cancelling", "needs_reconciliation",
		"succeeded", "partially_succeeded", "failed", "cancelled", "rejected", "expired", "invalidated"}
	legStates  = []string{"pending", "queued", "claimed", "running", "succeeded", "failed", "skipped", "cancelled", "unknown"}
	logLevels  = []string{"info", "ok", "warn", "error"}
	finalRuns  = map[string]bool{"succeeded": true, "partially_succeeded": true, "failed": true, "cancelled": true, "rejected": true, "expired": true, "invalidated": true}
	knownState = func(list []string) map[string]bool {
		m := map[string]bool{}
		for _, s := range list {
			m[s] = true
		}
		return m
	}
)

// runRoutes are the run endpoints. Permissions that depend on the run's target
// (run, cancel, restore, view_logs) are checked after the run is found: a
// grant scoped to a label, environment or folder cannot be judged before the
// run is known, so those routes need only view at the route level. Releasing
// locks is for administrators: manage_access at the route, admin in the engine.
func (s *server) runRoutes() []routeDef {
	return []routeDef{
		{"POST /api/v1/jobs/{id}/runs", auth.View, s.createRun},
		{"GET /api/v1/runs", auth.View, s.listRuns},
		{"GET /api/v1/runs/{id}", auth.View, s.getRun},
		{"GET /api/v1/runs/{id}/log", auth.View, s.getRunLog},
		{"GET /api/v1/runs/{id}/plan", auth.View, s.getRunPlan},
		{"GET /api/v1/runs/{id}/events", auth.View, s.streamEvents},
		{"POST /api/v1/runs/{id}/start", auth.View, s.startRun},
		{"POST /api/v1/runs/{id}/cancel", auth.View, s.cancelRun},
		{"GET /api/v1/approvals", auth.View, s.listApprovals},
		{"POST /api/v1/runs/{id}/approve", auth.View, s.approveRun},
		{"POST /api/v1/runs/{id}/reject", auth.View, s.rejectRun},
		{"POST /api/v1/runs/{id}/legs/{leg}/reconcile", auth.View, s.reconcileLeg},
		{"POST /api/v1/runs/{id}/legs/{leg}/check-again", auth.View, s.checkLegAgain},
		{"POST /api/v1/admin/runs/{id}/release-locks", auth.ManageAccess, s.releaseLocks},
	}
}

// ---- response shapes ----

type stepView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Use         string `json:"use"`
	Group       string `json:"group"`
	Position    int    `json:"position"`
	MaxParallel int    `json:"max_parallel"`
}

type legView struct {
	ID              int64      `json:"id"`
	StepID          *int64     `json:"step_id"`
	Step            string     `json:"step"`
	Kind            string     `json:"kind"`
	Service         string     `json:"service"`
	Version         string     `json:"version"`
	State           string     `json:"state"`
	Attempt         int        `json:"attempt"`
	CancelRequested bool       `json:"cancel_requested"`
	FailureShort    string     `json:"failure_short"`
	FailureDetail   string     `json:"failure_detail"`
	QueuedAt        *time.Time `json:"queued_at"`
	StartedAt       *time.Time `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
}

type failureView struct {
	LegID   int64  `json:"leg_id"`
	Service string `json:"service"`
	Step    string `json:"step"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Short   string `json:"short"`
	Detail  string `json:"detail"`
}

type summaryView struct {
	// Counts has an entry for every leg state, zero included.
	Counts   map[string]int `json:"counts"`
	Failures []failureView  `json:"failures"`
}

// runProgress is how far a run's legs have come.
type runProgress struct {
	CancelRequested bool        `json:"cancel_requested"`
	Legs            []legView   `json:"legs"`
	Locks           []string    `json:"locks"`
	Summary         summaryView `json:"summary"`
}

// runStateView is what changes while a run goes: it is the body of the stream's
// state event, and its fields are the same as the run detail's. A final run's
// reason is end_reason; a waiting run's is wait_reason (never both).
type runStateView struct {
	ID             int64      `json:"id"`
	State          string     `json:"state"`
	WaitReason     string     `json:"wait_reason"`
	EndReason      string     `json:"end_reason"`
	StateChangedAt time.Time  `json:"state_changed_at"`
	StartedAt      *time.Time `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	runProgress
}

type runSummaryView struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	JobSourceID int64  `json:"job_source_id"`
	JobID       string `json:"job_id"`
	JobName     string `json:"job_name"` // the job source's title, "" when unset
	Environment string `json:"environment"`
	Label       string `json:"label"`
	State       string `json:"state"`
	WaitReason  string `json:"wait_reason"`
	EndReason   string `json:"end_reason"`
	ManifestID  string `json:"manifest_id"`
	RequestedBy int64  `json:"requested_by"`
	// RequestedByName is the requester's name, else their email.
	RequestedByName string     `json:"requested_by_name"`
	Via             string     `json:"via"`
	CreatedAt       time.Time  `json:"created_at"`
	StateChangedAt  time.Time  `json:"state_changed_at"`
	StartedAt       *time.Time `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
}

type runDetailView struct {
	runSummaryView
	AWSAccount       string `json:"aws_account"`
	FolderID         int64  `json:"folder_id"`
	InventoryCommit  string `json:"inventory_commit"`
	JobCommit        string `json:"job_commit"`
	ApprovalRequired bool   `json:"approval_required"`
	AutoStart        bool   `json:"auto_start"`
	// Approval is the run's open approval, else the latest one; null when it never asked.
	Approval *approvalView `json:"approval"`
	Steps    []stepView    `json:"steps"`
	runProgress
}

func summarizeRun(r store.Run, names store.RunNames) runSummaryView {
	return runSummaryView{
		ID: r.ID, Kind: r.Kind, JobSourceID: r.JobSourceID, JobID: r.JobID, JobName: names.JobName, Environment: r.Environment, Label: r.Label,
		State: r.State, WaitReason: r.WaitReason, EndReason: r.EndReason, ManifestID: r.ManifestID,
		RequestedBy: r.RequestedBy, RequestedByName: names.RequestedByName, Via: r.Via, CreatedAt: r.CreatedAt, StateChangedAt: r.StateChangedAt,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
	}
}

// runRowView is a run's row in the list: the run, where each of its services stands and how its legs add up.
type runRowView struct {
	runSummaryView
	Services []serviceStrip `json:"services"`
	Summary  summaryView    `json:"summary"`
}

// buildRows shapes listed runs with one query for all their legs, one for their step names and one for the names.
func (s *server) buildRows(ctx context.Context, runs []store.Run) ([]runRowView, error) {
	ids := make([]int64, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	names, err := store.NamesOfRuns(ctx, s.Pool, ids)
	if err != nil {
		return nil, err
	}
	stepNames, err := store.StepNamesOfRuns(ctx, s.Pool, ids)
	if err != nil {
		return nil, err
	}
	legs, err := store.LegsOfRuns(ctx, s.Pool, ids)
	if err != nil {
		return nil, err
	}
	legsOf := make(map[int64][]store.Leg, len(runs))
	for _, l := range legs {
		legsOf[l.RunID] = append(legsOf[l.RunID], l)
	}
	rows := make([]runRowView, 0, len(runs))
	for _, run := range runs {
		rows = append(rows, runRowView{
			runSummaryView: summarizeRun(run, names[run.ID]),
			Services:       serviceStrips(legsOf[run.ID]),
			Summary:        summarizeLegs(legsOf[run.ID], stepNames),
		})
	}
	return rows, nil
}

// buildProgress reads a run's legs and locks and shapes them. steps names the
// steps of the legs (they never change after the run is created).
func (s *server) buildProgress(ctx context.Context, run store.Run, steps []store.RunStep) (runProgress, error) {
	legs, err := store.LegsOfRun(ctx, s.Pool, run.ID)
	if err != nil {
		return runProgress{}, err
	}
	locks, err := store.LocksOfRun(ctx, s.Pool, run.ID)
	if err != nil {
		return runProgress{}, err
	}
	stepName := make(map[int64]string, len(steps))
	for _, st := range steps {
		stepName[st.ID] = st.Name
	}
	view := runProgress{
		CancelRequested: run.CancelRequestedAt != nil || run.CancelRequestedBy != nil,
		Legs:            make([]legView, 0, len(legs)), Locks: locks,
		Summary: summarizeLegs(legs, stepName),
	}
	for _, l := range legs {
		name := ""
		if l.StepID != nil {
			name = stepName[*l.StepID]
		}
		view.Legs = append(view.Legs, legView{
			ID: l.ID, StepID: l.StepID, Step: name, Kind: l.Kind, Service: l.Service, Version: l.Version, State: l.State,
			Attempt: l.Attempt, CancelRequested: l.CancelRequested, FailureShort: l.FailureShort, FailureDetail: l.FailureDetail,
			QueuedAt: l.QueuedAt, StartedAt: l.StartedAt, FinishedAt: l.FinishedAt,
		})
	}
	return view, nil
}

// summarizeLegs counts a run's legs by state (every state is present, zero included) and lists the failed and unknown ones.
func summarizeLegs(legs []store.Leg, stepName map[int64]string) summaryView {
	summary := summaryView{Counts: make(map[string]int, len(legStates)), Failures: []failureView{}}
	for _, state := range legStates {
		summary.Counts[state] = 0
	}
	for _, l := range legs {
		summary.Counts[l.State]++
		if l.State == "failed" || l.State == "unknown" {
			name := ""
			if l.StepID != nil {
				name = stepName[*l.StepID]
			}
			summary.Failures = append(summary.Failures, failureView{
				LegID: l.ID, Service: l.Service, Step: name, Kind: l.Kind, State: l.State, Short: l.FailureShort, Detail: l.FailureDetail})
		}
	}
	return summary
}

// serviceStrip is one service of a run in the list: its version and where it stands.
type serviceStrip struct {
	Service string `json:"service"`
	Version string `json:"version"`
	State   string `json:"state"`
}

// serviceStrips lists the services of a run's step legs (resolve and revalidate legs have none), in the order the
// run first mentions them. It is the Go side of serviceProgress in web/src/features/runs/runModel.ts.
func serviceStrips(legs []store.Leg) []serviceStrip {
	strips := []serviceStrip{}
	index := map[string]int{}
	states := map[string][]string{}
	for _, l := range legs {
		if l.Kind != "step" || l.Service == "" {
			continue
		}
		i, seen := index[l.Service]
		if !seen {
			i = len(strips)
			index[l.Service] = i
			strips = append(strips, serviceStrip{Service: l.Service})
		}
		if strips[i].Version == "" {
			strips[i].Version = l.Version
		}
		states[l.Service] = append(states[l.Service], l.State)
	}
	for i := range strips {
		strips[i].State = overallServiceState(states[strips[i].Service])
	}
	return strips
}

// overallServiceState is the one state that stands for all of a service's step legs: the worst problem first, then
// anything still live, then done, cancelled or skipped; a mix of finished and waiting legs reads as running.
func overallServiceState(states []string) string {
	has := func(want string) bool { return slices.Contains(states, want) }
	for _, worst := range []string{"failed", "unknown", "running", "claimed", "queued"} {
		if has(worst) {
			return worst
		}
	}
	switch {
	case len(states) > 0 && !slices.ContainsFunc(states, func(s string) bool { return s != "succeeded" }):
		return "succeeded"
	case has("cancelled"):
		return "cancelled"
	case has("skipped"):
		return "skipped"
	case has("succeeded"):
		return "running"
	}
	return "pending"
}

// buildState is the state event's body.
func (s *server) buildState(ctx context.Context, run store.Run, steps []store.RunStep) (runStateView, error) {
	progress, err := s.buildProgress(ctx, run, steps)
	if err != nil {
		return runStateView{}, err
	}
	return runStateView{
		ID: run.ID, State: run.State, WaitReason: run.WaitReason, EndReason: run.EndReason,
		StateChangedAt: run.StateChangedAt, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, runProgress: progress,
	}, nil
}

func (s *server) buildDetail(ctx context.Context, run store.Run) (runDetailView, error) {
	steps, err := store.StepsOfRun(ctx, s.Pool, run.ID)
	if err != nil {
		return runDetailView{}, err
	}
	progress, err := s.buildProgress(ctx, run, steps)
	if err != nil {
		return runDetailView{}, err
	}
	names, err := store.NamesOfRuns(ctx, s.Pool, []int64{run.ID})
	if err != nil {
		return runDetailView{}, err
	}
	out := runDetailView{
		runSummaryView: summarizeRun(run, names[run.ID]), AWSAccount: run.AWSAccount, FolderID: run.FolderID,
		InventoryCommit: run.InventoryCommit, JobCommit: run.JobCommit,
		ApprovalRequired: run.ApprovalRequired, AutoStart: run.AutoStart,
		Steps: make([]stepView, 0, len(steps)), runProgress: progress,
	}
	for _, st := range steps {
		out.Steps = append(out.Steps, stepView{ID: st.ID, Name: st.Name, Use: st.Use, Group: st.GroupID, Position: st.Position, MaxParallel: st.MaxParallel})
	}
	approval, asked, err := store.LatestApproval(ctx, s.Pool, run.ID)
	if err != nil {
		return runDetailView{}, err
	}
	if asked {
		out.Approval, err = s.shapeApproval(ctx, map[int64]string{}, approval)
		if err != nil {
			return runDetailView{}, err
		}
	}
	return out, nil
}

// ---- helpers ----

func targetOfRun(r store.Run) auth.Target {
	return auth.Target{Label: r.Label, Environment: r.Environment, FolderID: r.FolderID}
}

// runFromPath loads the run of the {id} path value; it answers 404 itself.
func (s *server) runFromPath(w http.ResponseWriter, r *http.Request) (store.Run, bool) {
	id, ok := pathID(r)
	if !ok {
		notFound(w, "run")
		return store.Run{}, false
	}
	run, err := store.GetRun(r.Context(), s.Pool, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w, "run")
		return store.Run{}, false
	case err != nil:
		s.internalError(w, r, err)
		return store.Run{}, false
	}
	return run, true
}

func (s *server) principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		unauthorized(w)
	}
	return p, ok
}

func forbidden(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, codeForbidden, "you do not have permission to do this")
}

// problemsBody is the 422 answer for a selection that cannot be planned.
type problemsBody struct {
	Error struct {
		Code     string        `json:"code"`
		Message  string        `json:"message"`
		Problems spec.Problems `json:"problems"`
	} `json:"error"`
}

// engineError maps what the engine's actions return to an HTTP answer. The
// engine has already audited what it did, so nothing is recorded here.
func (s *server) engineError(w http.ResponseWriter, r *http.Request, err error) {
	var selection *engine.SelectionError
	switch {
	case errors.As(err, &selection):
		var body problemsBody
		body.Error.Code, body.Error.Message = codeInvalid, "the selection cannot be planned"
		body.Error.Problems = selection.Problems
		if body.Error.Problems == nil {
			body.Error.Problems = spec.Problems{}
		}
		writeJSON(w, http.StatusUnprocessableEntity, body)
	case errors.Is(err, engine.ErrForbidden):
		forbidden(w)
	case errors.Is(err, engine.ErrManifestChanged):
		writeError(w, http.StatusConflict, codeConflict, "The plan changed. Reload and review it again.")
	case errors.Is(err, engine.ErrApprovalExpired):
		writeError(w, http.StatusConflict, codeConflict, "The approval expired before the decision. The run has been expired.")
	case errors.Is(err, store.ErrAlreadyDecided):
		writeError(w, http.StatusConflict, codeConflict, "This run was already decided.")
	case errors.Is(err, engine.ErrWrongState), errors.Is(err, engine.ErrNotRunnable):
		writeError(w, http.StatusConflict, codeConflict, errorMessageOf(err))
	case errors.Is(err, store.ErrNotFound):
		notFound(w, "run or leg")
	case errors.Is(err, store.ErrInvalid):
		invalid(w, errorMessageOf(err))
	default:
		s.internalError(w, r, err)
	}
}

// errorMessageOf is the engine's explanation, which names no secret: it is
// built from states and names the person already sees.
func errorMessageOf(err error) string { return err.Error() }

// respondRun answers with the run's current detail.
func (s *server) respondRun(w http.ResponseWriter, r *http.Request, id int64, status int) {
	run, err := store.GetRun(r.Context(), s.Pool, id)
	if err == nil {
		var detail runDetailView
		if detail, err = s.buildDetail(r.Context(), run); err == nil {
			writeJSON(w, status, detail)
			return
		}
	}
	s.internalError(w, r, err)
}

// ---- handlers ----

type selectionGroup struct {
	Version  string            `json:"version"`
	Services map[string]string `json:"services"`
}

type createRunRequest struct {
	Environment string                    `json:"environment"`
	Selection   map[string]selectionGroup `json:"selection"`
	Start       bool                      `json:"start"`
}

func (s *server) createRun(w http.ResponseWriter, r *http.Request) {
	jobID, ok := pathID(r)
	if !ok {
		notFound(w, "job")
		return
	}
	if _, err := store.GetJobSource(r.Context(), s.Pool, jobID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound(w, "job")
		} else {
			s.internalError(w, r, err)
		}
		return
	}
	principal, ok := s.principal(w, r)
	if !ok {
		return
	}
	var req createRunRequest
	if !decodeBody(w, r, &req) {
		return
	}
	selection := engine.Selection{}
	for group, choice := range req.Selection {
		selection[group] = engine.GroupSelection{Version: choice.Version, Services: choice.Services}
	}
	via := "web"
	if principal.Via == viaToken {
		via = "cli"
	}
	run, err := s.Runs.Create(r.Context(), principal, jobID, req.Environment, selection, via, req.Start)
	if err != nil {
		s.engineError(w, r, err)
		return
	}
	s.respondRun(w, r, run.ID, http.StatusCreated)
}

// doRunAction runs an engine action on the run of the path and answers with
// the run.
func (s *server) doRunAction(w http.ResponseWriter, r *http.Request, act func(ctx context.Context, p auth.Principal, runID int64) error) {
	id, ok := pathID(r)
	if !ok {
		notFound(w, "run")
		return
	}
	principal, ok := s.principal(w, r)
	if !ok {
		return
	}
	if err := act(r.Context(), principal, id); err != nil {
		s.engineError(w, r, err)
		return
	}
	s.respondRun(w, r, id, http.StatusOK)
}

func (s *server) startRun(w http.ResponseWriter, r *http.Request) { s.doRunAction(w, r, s.Runs.Start) }
func (s *server) cancelRun(w http.ResponseWriter, r *http.Request) {
	s.doRunAction(w, r, s.Runs.Cancel)
}

// releaseLocks looks the run up first so that an unknown run is a 404 for
// everyone; the engine then refuses anyone who is not an administrator.
func (s *server) releaseLocks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.runFromPath(w, r); !ok {
		return
	}
	s.doRunAction(w, r, s.Runs.ReleaseLocks)
}

// doLegAction is doRunAction for an action on one leg of the run.
func (s *server) doLegAction(w http.ResponseWriter, r *http.Request, act func(ctx context.Context, p auth.Principal, runID, legID int64) error) {
	legID, err := strconv.ParseInt(r.PathValue("leg"), 10, 64)
	if err != nil || legID <= 0 {
		notFound(w, "leg")
		return
	}
	s.doRunAction(w, r, func(ctx context.Context, p auth.Principal, runID int64) error { return act(ctx, p, runID, legID) })
}

func (s *server) reconcileLeg(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Outcome string `json:"outcome"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	s.doLegAction(w, r, func(ctx context.Context, p auth.Principal, runID, legID int64) error {
		return s.Runs.Reconcile(ctx, p, runID, legID, req.Outcome)
	})
}

func (s *server) checkLegAgain(w http.ResponseWriter, r *http.Request) {
	s.doLegAction(w, r, s.Runs.CheckAgain)
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runFromPath(w, r)
	if !ok {
		return
	}
	s.respondRun(w, r, run.ID, http.StatusOK)
}

// intParam reads an optional integer query parameter; ok is false (and the
// answer written) when it is present but not an integer in [min, ...).
func intParam(w http.ResponseWriter, r *http.Request, name string, min int64) (value int64, present, ok bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, false, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < min {
		invalid(w, fmt.Sprintf("%s must be an integer of at least %d", name, min))
		return 0, true, false
	}
	return n, true, true
}

func (s *server) listRuns(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := store.RunFilter{Environment: query.Get("environment"), Query: query.Get("q")}
	var ok bool
	if filter.JobSourceID, _, ok = intParam(w, r, "job", 1); !ok {
		return
	}
	if filter.BeforeID, _, ok = intParam(w, r, "before", 1); !ok {
		return
	}
	limit, present, ok := intParam(w, r, "limit", 1)
	if !ok {
		return
	}
	if !present {
		limit = 50
	}
	limit = min(limit, maxRunListLimit)
	filter.Limit = int(limit)
	known := knownState(runStates)
	for _, state := range query["state"] {
		if !known[state] {
			invalid(w, fmt.Sprintf("unknown state %q", state))
			return
		}
		filter.States = append(filter.States, state)
	}
	runs, err := store.ListRuns(r.Context(), s.Pool, filter)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	rows, err := s.buildRows(r.Context(), runs)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := struct {
		Runs       []runRowView `json:"runs"`
		NextBefore *int64       `json:"next_before"`
	}{Runs: rows}
	if len(runs) == int(limit) {
		last := runs[len(runs)-1].ID
		out.NextBefore = &last
	}
	writeJSON(w, http.StatusOK, out)
}

type logLineView struct {
	LegID int64     `json:"leg_id"`
	Seq   int64     `json:"seq"`
	At    time.Time `json:"at"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
}

type gapView struct {
	LegID   int64 `json:"leg_id"`
	FromSeq int64 `json:"from_seq"`
	ToSeq   int64 `json:"to_seq"`
}

type logPageView struct {
	Lines []logLineView `json:"lines"`
	Next  string        `json:"next"`
	Gaps  []gapView     `json:"gaps"`
}

func shapeLogPage(page logs.Page) logPageView {
	out := logPageView{Lines: make([]logLineView, 0, len(page.Lines)), Next: string(page.Next), Gaps: make([]gapView, 0, len(page.Gaps))}
	for _, l := range page.Lines {
		out.Lines = append(out.Lines, logLineView{LegID: l.LegID, Seq: l.Seq, At: l.At, Level: l.Level, Text: l.Text})
	}
	for _, g := range page.Gaps {
		out.Gaps = append(out.Gaps, gapView{LegID: g.LegID, FromSeq: g.FromSeq, ToSeq: g.ToSeq})
	}
	return out
}

func (s *server) getRunLog(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runFromPath(w, r)
	if !ok {
		return
	}
	principal, ok := s.principal(w, r)
	if !ok {
		return
	}
	if !principal.Can(auth.ViewLogs, targetOfRun(run)) {
		forbidden(w)
		return
	}
	query := r.URL.Query()
	filter := logs.Filter{Service: query.Get("service"), Level: query.Get("level"), Query: query.Get("q")}
	if filter.Level != "" && !knownState(logLevels)[filter.Level] {
		invalid(w, fmt.Sprintf("unknown level %q", filter.Level))
		return
	}
	if name := query.Get("step"); name != "" {
		steps, err := store.StepsOfRun(r.Context(), s.Pool, run.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		for _, st := range steps {
			if st.Name == name {
				id := st.ID
				filter.StepID = &id
			}
		}
		if filter.StepID == nil {
			invalid(w, fmt.Sprintf("the run has no step %q", name))
			return
		}
	}
	limit, present, ok := intParam(w, r, "limit", 1)
	if !ok {
		return
	}
	if !present {
		limit = 200
	}
	limit = min(limit, maxLogLimit)
	page, err := s.Logs.Read(r.Context(), run.ID, filter, logs.Cursor(query.Get("after")), int(limit))
	switch {
	case errors.Is(err, logs.ErrBadCursor):
		invalid(w, "the after cursor is not one this log produced")
	case err != nil:
		s.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, shapeLogPage(page))
	}
}
