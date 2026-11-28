package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/store"
)

func (s *server) queueRoutes() []routeDef {
	return []routeDef{
		{"GET /api/v1/queue", auth.View, s.getQueue},
	}
}

type queueRunView struct {
	ID            int64     `json:"id"`
	JobID         string    `json:"job_id"`
	JobName       string    `json:"job_name"`
	Environment   string    `json:"environment"`
	Label         string    `json:"label"`
	State         string    `json:"state"`
	WaitReason    string    `json:"wait_reason"`
	BlockedByRun  *int64    `json:"blocked_by_run_id"`
	WaitingSince  time.Time `json:"waiting_since"`
	WaitedSeconds int64     `json:"waited_seconds"`
	LegsRunning   int       `json:"legs_running"`
	LegsQueued    int       `json:"legs_queued"`
}

type queueLockView struct {
	Resource   string    `json:"resource"`
	RunID      int64     `json:"run_id"`
	AcquiredAt time.Time `json:"acquired_at"`
}

type runnerView struct {
	ID          string    `json:"id"`
	Version     string    `json:"version"`
	Concurrency int       `json:"concurrency"`
	Busy        int       `json:"busy"`
	StartedAt   time.Time `json:"started_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

type queueResponse struct {
	Runs                   []queueRunView  `json:"runs"`
	Locks                  []queueLockView `json:"locks"`
	Runners                []runnerView    `json:"runners"`
	FreeSlots              int             `json:"free_slots"`
	QueuedLegs             int             `json:"queued_legs"`
	OldestQueuedAt         *time.Time      `json:"oldest_queued_at"`
	RunnerGoneAfterSeconds int64           `json:"runner_gone_after_seconds"`
}

// runnerGoneAfter is how long a runner may be silent before it stops counting.
func (s *server) runnerGoneAfter() time.Duration {
	if s.RunnerGoneAfter > 0 {
		return s.RunnerGoneAfter
	}
	return store.RunnerGoneAfter
}

// activeRunners lists the runners seen within runnerGoneAfter and how many
// legs they could still take at once (a runner over its limit counts 0).
func (s *server) activeRunners(ctx context.Context, now time.Time) ([]runnerView, int, error) {
	runners, err := store.ListRunners(ctx, s.Pool, now.Add(-s.runnerGoneAfter()))
	if err != nil {
		return nil, 0, err
	}
	views, free := make([]runnerView, 0, len(runners)), 0
	for _, r := range runners {
		views = append(views, runnerView{ID: r.ID, Version: r.Version, Concurrency: r.Concurrency, Busy: r.Busy,
			StartedAt: r.StartedAt, LastSeenAt: r.LastSeenAt})
		free += max(r.Concurrency-r.Busy, 0)
	}
	return views, free, nil
}

// blockingRun reads N from a wait reason of exactly the form "waiting for run #N"
// (what the engine writes when another run holds a lock this run needs).
func blockingRun(waitReason string) *int64 {
	digits, ok := strings.CutPrefix(waitReason, "waiting for run #")
	if !ok {
		return nil
	}
	id, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != digits {
		return nil
	}
	return &id
}

func (s *server) getQueue(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), s.Now().UTC()
	runs, err := store.ListQueueRuns(ctx, s.Pool)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	locks, err := store.ListLocks(ctx, s.Pool)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	runners, free, err := s.activeRunners(ctx, now)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	queuedLegs, oldest, err := store.QueueStats(ctx, s.Pool, now)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := queueResponse{Runs: make([]queueRunView, 0, len(runs)), Locks: make([]queueLockView, 0, len(locks)), Runners: runners,
		FreeSlots: free, QueuedLegs: queuedLegs, OldestQueuedAt: oldest, RunnerGoneAfterSeconds: int64(s.runnerGoneAfter().Seconds())}
	for _, run := range runs {
		out.Runs = append(out.Runs, queueRunView{ID: run.ID, JobID: run.JobID, JobName: run.JobName, Environment: run.Environment,
			Label: run.Label, State: run.State, WaitReason: run.WaitReason, BlockedByRun: blockingRun(run.WaitReason),
			WaitingSince: run.WaitingSince, WaitedSeconds: max(int64(now.Sub(run.WaitingSince).Seconds()), 0),
			LegsRunning: run.LegsRunning, LegsQueued: run.LegsQueued})
	}
	for _, lock := range locks {
		out.Locks = append(out.Locks, queueLockView{Resource: lock.Resource, RunID: lock.RunID, AcquiredAt: lock.AcquiredAt})
	}
	writeJSON(w, http.StatusOK, out)
}
