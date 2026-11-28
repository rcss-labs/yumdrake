//go:build integration

package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/testutil/testdb"
)

// now is the fixed clock of every test: with 7 days the period starts on
// 2026-11-14 00:00 UTC and the previous one on 2026-11-07 00:00 UTC.
var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func day(d int, hour, minute int) time.Time {
	return time.Date(2026, 11, d, hour, minute, 0, 0, time.UTC)
}

type fixture struct {
	t                  *testing.T
	pool               *pgxpool.Pool
	userID             int64
	folderID, sourceID int64
	snapshotID         int64
}

func newFixture(t *testing.T, environments ...spec.Environment) *fixture {
	t.Helper()
	ctx := context.Background()
	url := testdb.New(t)
	if err := db.Migrate(ctx, url); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	user, err := store.CreateLocalUser(ctx, pool, "dana@example.com", "Dana", "local", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	folder, err := store.CreateFolder(ctx, pool, "folder")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	source, err := store.CreateJobSource(ctx, pool, store.JobSource{FolderID: folder.ID, Repo: "acme/jobs", Branch: "main", Path: "deploy.yaml"})
	if err != nil {
		t.Fatalf("CreateJobSource: %v", err)
	}
	inventory, err := json.Marshal(spec.Inventory{Environments: environments})
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	snapshot, err := store.ActivateSnapshot(ctx, pool, "inv1", inventory, nil, now)
	if err != nil {
		t.Fatalf("ActivateSnapshot: %v", err)
	}
	return &fixture{t: t, pool: pool, userID: user.ID, folderID: folder.ID, sourceID: source.ID, snapshotID: snapshot.ID}
}

func (f *fixture) load(label string, days int) Overview {
	f.t.Helper()
	got, err := Load(context.Background(), f.pool, auth.Principal{}, label, days, now)
	if err != nil {
		f.t.Fatalf("Load: %v", err)
	}
	return got
}

// seed describes one run. started and finished are nil for a run that never
// started or has not finished; created defaults to started.
type seed struct {
	kind, state, job, env, label, endReason string
	created                                 time.Time
	started, finished, changed              *time.Time
	legs                                    []seedLeg
}

type seedLeg struct{ use, service, reason, state string }

func at(t time.Time) *time.Time { return &t }

func (f *fixture) add(s seed) int64 {
	f.t.Helper()
	if s.kind == "" {
		s.kind = "deploy"
	}
	if s.job == "" {
		s.job = "deploy-api"
	}
	if s.env == "" {
		s.env = "prod-eu"
	}
	if s.label == "" {
		s.label = "prod"
	}
	if s.created.IsZero() {
		s.created = *s.started
	}
	run := store.Run{
		Kind: s.kind, JobSourceID: f.sourceID, JobID: s.job, JobSnapshot: []byte(`{}`), InventorySnapshotID: f.snapshotID,
		Environment: s.env, Label: s.label, FolderID: f.folderID, Selection: []byte(`{}`),
		State: "planned", RequestedBy: f.userID, Via: "web", CreatedAt: s.created,
	}
	var steps []store.RunStep
	var legs []store.Leg
	for i, l := range s.legs {
		steps = append(steps, store.RunStep{Name: fmt.Sprintf("s%d", i), Use: l.use, Position: i, Needs: []byte(`[]`)})
		idx := int64(i)
		legs = append(legs, store.Leg{Kind: "step", StepID: &idx, Service: l.service, State: l.state, FailureShort: l.reason})
	}
	inserted, err := store.InsertRun(context.Background(), f.pool, run, steps, legs)
	if err != nil {
		f.t.Fatalf("InsertRun: %v", err)
	}
	changed := s.created
	if s.changed != nil {
		changed = *s.changed
	}
	_, err = f.pool.Exec(context.Background(),
		`UPDATE runs SET state = $2, started_at = $3, finished_at = $4, end_reason = $5, state_changed_at = $6 WHERE id = $1`,
		inserted.ID, s.state, s.started, s.finished, s.endReason, changed)
	if err != nil {
		f.t.Fatalf("set run state: %v", err)
	}
	return inserted.ID
}

// finished seeds a run that started at start and took d.
func (f *fixture) finished(state string, start time.Time, d time.Duration, change func(*seed)) int64 {
	s := seed{state: state, started: at(start), finished: at(start.Add(d))}
	if change != nil {
		change(&s)
	}
	return f.add(s)
}

var prodAndStaging = []spec.Environment{
	{Name: "prod-eu", Label: "prod"}, {Name: "prod-us", Label: "prod"}, {Name: "stg", Label: "staging"},
}

func ptr[T any](v T) *T { return &v }

func TestTotalsAndPreviousPeriod(t *testing.T) {
	f := newFixture(t, prodAndStaging...)
	// This period: 2 succeeded, 1 failed, 1 partially, 1 running, 1 restore that succeeded.
	f.finished("succeeded", day(15, 10, 0), time.Minute, nil)
	f.finished("succeeded", day(15, 11, 0), time.Minute, nil)
	f.finished("failed", day(16, 9, 0), time.Minute, nil)
	f.finished("partially_succeeded", day(16, 10, 0), time.Minute, nil)
	f.finished("succeeded", day(17, 10, 0), time.Minute, func(s *seed) { s.kind = "restore" })
	f.add(seed{state: "running", started: at(day(19, 8, 0))})
	// Not counted: still planned or resolving, and older than both periods.
	f.add(seed{state: "planned", created: day(18, 9, 0)})
	f.add(seed{state: "resolving", created: day(18, 9, 0)})
	f.finished("succeeded", time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), time.Minute, nil)
	// Previous period: 1 succeeded, 2 failed.
	f.finished("succeeded", day(10, 9, 0), time.Minute, nil)
	f.finished("failed", day(11, 9, 0), time.Minute, nil)
	f.finished("failed", day(13, 23, 59), time.Minute, nil)

	got := f.load("", 7)
	if got.Runs != 6 || got.PrevRuns != 3 {
		t.Errorf("Runs = %d, PrevRuns = %d, want 6 and 3", got.Runs, got.PrevRuns)
	}
	if got.SuccessRate == nil || *got.SuccessRate != 0.6 {
		t.Errorf("SuccessRate = %v, want 0.6 (3 of 5 finished)", got.SuccessRate)
	}
	if got.PrevSuccessRate == nil || *got.PrevSuccessRate < 0.333 || *got.PrevSuccessRate > 0.334 {
		t.Errorf("PrevSuccessRate = %v, want 1/3", got.PrevSuccessRate)
	}
	if want := day(14, 0, 0); !got.From.Equal(want) || !got.To.Equal(now) {
		t.Errorf("From, To = %v, %v, want %v, %v", got.From, got.To, want, now)
	}
}

func TestSuccessRateNilWithNoFinishedRuns(t *testing.T) {
	f := newFixture(t, prodAndStaging...)
	f.add(seed{state: "running", started: at(day(19, 8, 0))})
	f.add(seed{state: "queued", created: day(19, 9, 0)})
	f.add(seed{state: "cancelled", created: day(19, 9, 0)})

	got := f.load("", 7)
	if got.Runs != 3 {
		t.Errorf("Runs = %d, want 3", got.Runs)
	}
	if got.SuccessRate != nil || got.PrevSuccessRate != nil {
		t.Errorf("SuccessRate, PrevSuccessRate = %v, %v, want nil, nil (no finished run)", got.SuccessRate, got.PrevSuccessRate)
	}
	if got.MedianDuration != nil || got.PrevMedianDuration != nil {
		t.Errorf("MedianDuration, PrevMedianDuration = %v, %v, want nil, nil", got.MedianDuration, got.PrevMedianDuration)
	}
}

func TestMedianDurationOddAndEvenCounts(t *testing.T) {
	f := newFixture(t, prodAndStaging...)
	for i, d := range []time.Duration{300 * time.Second, 60 * time.Second, 120 * time.Second} {
		f.finished("succeeded", day(15, 8+i, 0), d, nil)
	}
	// Previous period: even count, so the mean of the middle two.
	for i, d := range []time.Duration{600 * time.Second, 60 * time.Second, 120 * time.Second, 300 * time.Second} {
		f.finished("failed", day(10, 8+i, 0), d, nil)
	}
	// A running run has no duration and must not count.
	f.add(seed{state: "running", started: at(day(19, 8, 0))})

	got := f.load("", 7)
	if got.MedianDuration == nil || *got.MedianDuration != 120*time.Second {
		t.Errorf("MedianDuration = %v, want 2m0s", got.MedianDuration)
	}
	if got.PrevMedianDuration == nil || *got.PrevMedianDuration != 210*time.Second {
		t.Errorf("PrevMedianDuration = %v, want 3m30s", got.PrevMedianDuration)
	}
}

func TestDaysIncludeZeroDays(t *testing.T) {
	f := newFixture(t, prodAndStaging...)
	f.finished("succeeded", day(15, 23, 30), time.Hour, nil) // starts on the 15th, ends on the 16th
	f.finished("failed", day(15, 1, 0), time.Minute, nil)
	f.finished("partially_succeeded", day(15, 2, 0), time.Minute, nil)
	f.add(seed{state: "cancelled", started: at(day(18, 9, 0)), finished: at(day(18, 9, 5))})
	f.add(seed{state: "invalidated", started: at(day(18, 10, 0)), finished: at(day(18, 10, 5))})
	f.add(seed{state: "running", started: at(day(20, 9, 0))}) // running is in no column

	got := f.load("", 7)
	if len(got.Days) != 7 {
		t.Fatalf("len(Days) = %d, want 7: %+v", len(got.Days), got.Days)
	}
	want := []Day{
		{Date: "2026-11-14"},
		{Date: "2026-11-15", Succeeded: 1, Failed: 2},
		{Date: "2026-11-16"},
		{Date: "2026-11-17"},
		{Date: "2026-11-18", Other: 2},
		{Date: "2026-11-19"},
		{Date: "2026-11-20"},
	}
	if !reflect.DeepEqual(got.Days, want) {
		t.Errorf("Days = %+v\nwant   %+v", got.Days, want)
	}
	if got30 := f.load("", 30); len(got30.Days) != 30 || got30.Days[0].Date != "2026-10-22" {
		t.Errorf("30 days: len %d, first %q, want 30 and 2026-10-22", len(got30.Days), got30.Days[0].Date)
	}
}

func TestLabelFilter(t *testing.T) {
	f := newFixture(t, prodAndStaging...)
	f.finished("succeeded", day(15, 9, 0), time.Minute, nil)
	f.finished("failed", day(15, 10, 0), time.Minute, nil)
	f.finished("succeeded", day(15, 11, 0), time.Minute, func(s *seed) { s.env, s.label = "stg", "staging" })
	f.add(seed{state: "waiting_approval", created: day(18, 9, 0), env: "stg", label: "staging"})
	f.add(seed{state: "waiting_approval", created: day(18, 8, 0)})

	all := f.load("", 7)
	if all.Runs != 5 || all.WaitingApproval != 2 || len(all.Environments) != 3 {
		t.Errorf("unfiltered: Runs %d, WaitingApproval %d, Environments %d, want 5, 2, 3", all.Runs, all.WaitingApproval, len(all.Environments))
	}
	staging := f.load("staging", 7)
	if staging.Runs != 2 || staging.WaitingApproval != 1 {
		t.Errorf("staging: Runs %d, WaitingApproval %d, want 2 and 1", staging.Runs, staging.WaitingApproval)
	}
	if staging.SuccessRate == nil || *staging.SuccessRate != 1 {
		t.Errorf("staging SuccessRate = %v, want 1", staging.SuccessRate)
	}
	if len(staging.Environments) != 1 || staging.Environments[0].Environment != "stg" {
		t.Errorf("staging Environments = %+v, want only stg", staging.Environments)
	}
	if staging.OldestWaiting == nil || !staging.OldestWaiting.Equal(day(18, 9, 0)) {
		t.Errorf("staging OldestWaiting = %v, want the staging run's time", staging.OldestWaiting)
	}
	for _, item := range staging.Attention {
		if item.Label != "staging" {
			t.Errorf("staging Attention has a %q item: %+v", item.Label, item)
		}
	}
}

func TestTopFailingWithCommonReason(t *testing.T) {
	f := newFixture(t, prodAndStaging...)
	run := func(hour int, legs ...seedLeg) {
		f.finished("failed", day(15, hour, 0), time.Minute, func(s *seed) { s.legs = legs })
	}
	failed := func(use, service, reason string) seedLeg { return seedLeg{use, service, reason, "failed"} }
	run(1, failed("ecs-deploy", "api", "timeout"), failed("copy-config", "api", "timeout"), failed("ecs-deploy", "web", "rollback"))
	run(2, failed("ecs-deploy", "api", "denied"), failed("ecs-deploy", "api", "timeout"))
	run(3, failed("ecs-deploy", "web", "denied"), failed("ecs-deploy", "worker", ""))
	// Ignored: a leg of another step kind, a leg that did not fail, a run outside the period.
	run(4, failed("run-task", "api", "oom"), seedLeg{"ecs-deploy", "api", "", "succeeded"})
	f.finished("failed", day(10, 1, 0), time.Minute, func(s *seed) { s.legs = []seedLeg{failed("ecs-deploy", "api", "old")} })
	// Tie-break: "alpha" and "beta" each once; and more services to reach the limit of 5.
	run(5, failed("ecs-deploy", "tie", "beta"), failed("ecs-deploy", "tie", "alpha"))
	for _, name := range []string{"s1", "s2", "s3"} {
		run(6, failed("copy-config", name, "x"))
	}

	got := f.load("", 7).TopFailing
	want := []ServiceFailures{
		{"api", 4, "timeout"},
		{"tie", 2, "alpha"},  // alpha and beta once each: alphabetical first
		{"web", 2, "denied"}, // rollback and denied once each: alphabetical first
		{"s1", 1, "x"},       // services with one failure follow in name order; the list stops at 5
		{"s2", 1, "x"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TopFailing = %+v\nwant       %+v", got, want)
	}
}

func TestAttentionExcludesFailuresFixedByALaterRun(t *testing.T) {
	f := newFixture(t, prodAndStaging...)
	// deploy-api in prod-eu failed, then a later run succeeded: not shown.
	f.finished("failed", day(15, 9, 0), time.Minute, nil)
	f.finished("succeeded", day(16, 9, 0), time.Minute, nil)
	// deploy-web in prod-eu: failed, nothing later; reason from end_reason.
	f.finished("failed", day(16, 10, 0), time.Minute, func(s *seed) { s.job, s.endReason = "deploy-web", "approval expired" })
	// deploy-web in prod-us: partially succeeded, reason from the first failed leg.
	f.finished("partially_succeeded", day(17, 10, 0), time.Minute, func(s *seed) {
		s.job, s.env = "deploy-web", "prod-us"
		s.legs = []seedLeg{{"ecs-deploy", "web", "second", "failed"}, {"ecs-deploy", "api", "first", "succeeded"}}
	})
	// deploy-web in stg: failed before the period; not in the period, not shown.
	f.finished("failed", day(10, 10, 0), time.Minute, func(s *seed) { s.job, s.env, s.label = "deploy-web", "stg", "staging" })
	// Waiting and needing reconciliation, any age.
	f.add(seed{state: "needs_reconciliation", created: day(18, 9, 0), changed: at(day(18, 9, 30)), job: "deploy-db"})
	f.add(seed{state: "waiting_approval", created: day(2, 9, 0), changed: at(day(2, 9, 0)), job: "deploy-old"})
	// A person without a name is shown by email.
	nameless, err := store.CreateLocalUser(context.Background(), f.pool, "nameless@example.com", "", "local", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	waiting := f.add(seed{state: "waiting_approval", created: day(19, 9, 0), job: "deploy-new"})
	if _, err := f.pool.Exec(context.Background(), "UPDATE runs SET requested_by = $2 WHERE id = $1", waiting, nameless.ID); err != nil {
		t.Fatalf("set requester: %v", err)
	}

	got := f.load("", 7).Attention
	var summary []string
	for _, item := range got {
		summary = append(summary, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s", item.State, item.Job, item.Environment, item.Reason, item.By, item.Label, item.At.Format("01-02T15:04")))
	}
	want := []string{
		"waiting_approval|deploy-old|prod-eu|Waiting for approval|Dana|prod|11-02T09:00",
		"needs_reconciliation|deploy-db|prod-eu|Needs reconciliation|Dana|prod|11-18T09:30",
		"waiting_approval|deploy-new|prod-eu|Waiting for approval|nameless@example.com|prod|11-19T09:00",
		"partially_succeeded|deploy-web|prod-us|second|Dana|prod|11-17T10:01",
		"failed|deploy-web|prod-eu|approval expired|Dana|prod|11-16T10:01",
	}
	if !reflect.DeepEqual(summary, want) {
		t.Errorf("Attention:\n got %q\nwant %q", summary, want)
	}
}

func TestEnvironmentsLastRun(t *testing.T) {
	f := newFixture(t, prodAndStaging...)
	older := f.finished("succeeded", day(10, 9, 0), time.Minute, nil) // before the period: still the last run
	_ = older
	newest := f.finished("failed", day(12, 9, 0), time.Minute, func(s *seed) { s.job = "deploy-web" })
	running := f.add(seed{state: "running", started: at(day(19, 8, 0)), env: "prod-us"})
	f.add(seed{state: "planned", created: day(19, 9, 0), env: "prod-us"}) // never counts

	got := f.load("", 7).Environments
	want := []EnvLast{
		{Environment: "prod-eu", Label: "prod", RunID: &newest, State: "failed", Job: "deploy-web", At: at(day(12, 9, 1))},
		{Environment: "prod-us", Label: "prod", RunID: &running, State: "running", Job: "deploy-api", At: at(day(19, 8, 0))},
		{Environment: "stg", Label: "staging"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Environments = %s\nwant          %s", dump(got), dump(want))
	}
}

func TestNeverStartedCancelledRunCountsAsOther(t *testing.T) {
	f := newFixture(t, prodAndStaging...)
	f.add(seed{state: "cancelled", created: day(17, 8, 0), finished: at(day(17, 8, 30))})

	got := f.load("", 7)
	if got.Runs != 1 {
		t.Errorf("Runs = %d, want 1", got.Runs)
	}
	if d := got.Days[3]; d.Date != "2026-11-17" || d.Other != 1 {
		t.Errorf("Days[3] = %+v, want 2026-11-17 with Other 1", d)
	}
	if got.SuccessRate != nil {
		t.Errorf("SuccessRate = %v, want nil: a cancelled run is not finished", got.SuccessRate)
	}
}

func dump(v any) string {
	out, _ := json.Marshal(v)
	return string(out)
}
