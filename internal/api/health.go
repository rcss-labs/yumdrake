package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/notify"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// Thresholds of the problems list on the System health page.
const (
	queuedLegProblemAfter = 5 * time.Minute
	inventoryStaleAfter   = 10 * time.Minute
	leaseExpiredLimit     = 5 // more than this many in the last hour is a problem
)

func (s *server) healthRoutes() []routeDef {
	return []routeDef{
		{"GET /api/v1/system/health", auth.ManageConnections, s.getHealth},
	}
}

type healthCounter struct {
	Hour int `json:"hour"`
	Day  int `json:"day"`
}

type healthCounters struct {
	LeaseExpired     healthCounter `json:"lease_expired"`
	LogPersistFailed healthCounter `json:"log_persist_failed"`
	SyncFailed       healthCounter `json:"sync_failed"`
}

type healthQueue struct {
	QueuedLegs     int        `json:"queued_legs"`
	OldestQueuedAt *time.Time `json:"oldest_queued_at"`
	ActiveRuns     int        `json:"active_runs"`
}

type healthSyncStatus struct {
	CheckedAt *time.Time `json:"checked_at"`
	Error     string     `json:"error"`
	ErrorAt   *time.Time `json:"error_at"`
}

type healthSyncJob struct {
	ID        int64      `json:"id"`
	JobID     string     `json:"job_id"`
	Title     string     `json:"title"`
	CheckedAt *time.Time `json:"checked_at"`
	Error     string     `json:"error"`
	ErrorAt   *time.Time `json:"error_at"`
}

type healthSync struct {
	Inventory healthSyncStatus `json:"inventory"`
	Jobs      []healthSyncJob  `json:"jobs"`
}

type healthConnection struct {
	Kind          string     `json:"kind"`
	Name          string     `json:"name"`
	LastOKAt      *time.Time `json:"last_ok_at"`
	LastErrorAt   *time.Time `json:"last_error_at"`
	LastErrorCode string     `json:"last_error_code"`
	Failing       bool       `json:"failing"`
}

type healthNotifications struct {
	Pending         int        `json:"pending"`
	FailedLastDay   int        `json:"failed_last_day"`
	OldestPendingAt *time.Time `json:"oldest_pending_at"`
}

type healthResponse struct {
	Problems               []string            `json:"problems"`
	Runners                []runnerView        `json:"runners"`
	RunnerGoneAfterSeconds int64               `json:"runner_gone_after_seconds"`
	Queue                  healthQueue         `json:"queue"`
	Counters               healthCounters      `json:"counters"`
	Sync                   healthSync          `json:"sync"`
	Connections            []healthConnection  `json:"connections"`
	Notifications          healthNotifications `json:"notifications"`
}

// durationWords says a duration in whole minutes ("2 minutes"), else in seconds.
func durationWords(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return plural(int(d/time.Minute), "minute", "minutes")
	}
	return plural(int(d/time.Second), "second", "seconds")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (s *server) getHealth(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), s.Now().UTC()
	hourAgo, dayAgo := now.Add(-time.Hour), now.Add(-24*time.Hour)

	runners, _, err := s.activeRunners(ctx, now)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := healthResponse{Runners: runners, RunnerGoneAfterSeconds: int64(s.runnerGoneAfter().Seconds())}
	if out.Queue.QueuedLegs, out.Queue.OldestQueuedAt, err = store.QueueStats(ctx, s.Pool, now); err != nil {
		s.internalError(w, r, err)
		return
	}
	active, err := store.ActiveRunIDs(ctx, s.Pool)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out.Queue.ActiveRuns = len(active)

	for _, counter := range []struct {
		kind string
		into *healthCounter
	}{{"lease_expired", &out.Counters.LeaseExpired}, {"log_persist_failed", &out.Counters.LogPersistFailed}, {"sync_failed", &out.Counters.SyncFailed}} {
		if counter.into.Hour, err = store.CountEvents(ctx, s.Pool, counter.kind, hourAgo); err != nil {
			s.internalError(w, r, err)
			return
		}
		if counter.into.Day, err = store.CountEvents(ctx, s.Pool, counter.kind, dayAgo); err != nil {
			s.internalError(w, r, err)
			return
		}
	}

	status, err := store.GetInventoryStatus(ctx, s.Pool)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out.Sync.Inventory = healthSyncStatus{CheckedAt: status.CheckedAt, Error: status.SyncError, ErrorAt: status.SyncErrorAt}
	if out.Sync.Jobs, err = s.healthSyncJobs(ctx); err != nil {
		s.internalError(w, r, err)
		return
	}
	if out.Connections, err = s.healthConnections(ctx); err != nil {
		s.internalError(w, r, err)
		return
	}

	pending, _, oldestPending, err := store.NotificationStats(ctx, s.Pool)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	failedLastDay, err := store.FailedNotificationsSince(ctx, s.Pool, dayAgo)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out.Notifications = healthNotifications{Pending: pending, FailedLastDay: failedLastDay, OldestPendingAt: oldestPending}

	out.Problems = s.healthProblems(now, out)
	writeJSON(w, http.StatusOK, out)
}

func (s *server) healthSyncJobs(ctx context.Context) ([]healthSyncJob, error) {
	sources, err := store.ListJobSources(ctx, s.Pool)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].JobID != sources[j].JobID {
			return sources[i].JobID < sources[j].JobID
		}
		return sources[i].ID < sources[j].ID
	})
	jobs := make([]healthSyncJob, 0, len(sources))
	for _, source := range sources {
		jobs = append(jobs, healthSyncJob{ID: source.ID, JobID: source.JobID, Title: source.Title, CheckedAt: source.CheckedAt,
			Error: source.SyncError, ErrorAt: source.SyncErrorAt})
	}
	return jobs, nil
}

// healthConnections lists every AWS account, GitHub connection and the Slack
// token (when one is stored) with what the connection checks say about it.
func (s *server) healthConnections(ctx context.Context) ([]healthConnection, error) {
	checks, err := store.ListConnectionChecks(ctx, s.Pool)
	if err != nil {
		return nil, err
	}
	byRef := map[string]store.ConnectionCheck{}
	for _, c := range checks {
		byRef[c.Kind+"/"+strings.ToLower(c.Ref)] = c
	}
	view := func(kind, name, ref string) healthConnection {
		c := byRef[kind+"/"+strings.ToLower(ref)]
		conn := healthConnection{Kind: kind, Name: name, LastOKAt: c.LastOKAt, LastErrorAt: c.LastErrorAt, LastErrorCode: c.LastErrorCode}
		conn.Failing = c.LastErrorAt != nil && (c.LastOKAt == nil || c.LastErrorAt.After(*c.LastOKAt))
		return conn
	}

	accounts, err := store.ListAWSAccounts(ctx, s.Pool)
	if err != nil {
		return nil, err
	}
	githubs, err := store.ListConnections(ctx, s.Pool)
	if err != nil {
		return nil, err
	}
	slack, err := notify.LoadSlackStatus(ctx, s.Pool)
	if err != nil {
		return nil, err
	}
	connections := make([]healthConnection, 0, len(accounts)+len(githubs)+1)
	for _, a := range accounts {
		connections = append(connections, view("aws", a.Name, a.AccountID))
	}
	for _, g := range githubs {
		connections = append(connections, view("github", g.Owner, g.Owner))
	}
	if slack.Configured {
		connections = append(connections, view("slack", "Slack", "slack"))
	}
	rank := map[string]int{"aws": 0, "github": 1, "slack": 2}
	sort.SliceStable(connections, func(i, j int) bool {
		if connections[i].Kind != connections[j].Kind {
			return rank[connections[i].Kind] < rank[connections[j].Kind]
		}
		return connections[i].Name < connections[j].Name
	})
	return connections, nil
}

// healthProblems states in plain words what is wrong, in a fixed order; it is
// empty when nothing is.
func (s *server) healthProblems(now time.Time, h healthResponse) []string {
	problems := []string{}
	if len(h.Runners) == 0 {
		problems = append(problems, "No runner has reported for "+durationWords(s.runnerGoneAfter())+".")
	}
	if h.Queue.OldestQueuedAt != nil && now.Sub(*h.Queue.OldestQueuedAt) > queuedLegProblemAfter {
		problems = append(problems, fmt.Sprintf("The oldest queued leg has waited %s.", plural(int(now.Sub(*h.Queue.OldestQueuedAt)/time.Minute), "minute", "minutes")))
	}
	switch checked := h.Sync.Inventory.CheckedAt; {
	case checked == nil:
		problems = append(problems, "The inventory has never been checked.")
	case now.Sub(*checked) > inventoryStaleAfter:
		problems = append(problems, fmt.Sprintf("The inventory has not been checked for %s.", plural(int(now.Sub(*checked)/time.Minute), "minute", "minutes")))
	}
	if n := h.Counters.LogPersistFailed.Hour; n > 0 {
		if n == 1 {
			problems = append(problems, "1 log batch could not be stored in the last hour.")
		} else {
			problems = append(problems, fmt.Sprintf("%d log batches could not be stored in the last hour.", n))
		}
	}
	if n := h.Counters.LeaseExpired.Hour; n > leaseExpiredLimit {
		problems = append(problems, fmt.Sprintf("%d leases expired in the last hour.", n))
	}
	if n := h.Notifications.FailedLastDay; n > 0 {
		if n == 1 {
			problems = append(problems, "1 notification failed in the last day.")
		} else {
			problems = append(problems, fmt.Sprintf("%d notifications failed in the last day.", n))
		}
	}
	for _, c := range h.Connections {
		if !c.Failing {
			continue
		}
		switch c.Kind {
		case "aws":
			problems = append(problems, fmt.Sprintf("AWS account %s failed its last attempt: %s.", c.Name, c.LastErrorCode))
		case "github":
			problems = append(problems, fmt.Sprintf("GitHub connection %s failed its last attempt: %s.", c.Name, c.LastErrorCode))
		case "slack":
			problems = append(problems, fmt.Sprintf("Slack failed its last attempt: %s.", c.LastErrorCode))
		}
	}
	return problems
}
