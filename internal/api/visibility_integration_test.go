//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/notify"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// seededLeg and seededRun let a test write exactly the rows a page reads,
// without running the engine.
type seededLeg struct {
	kind, service, version, state string
	queued, started, finished     *time.Time
}

type seededRun struct {
	kind, environment, label, state, waitReason string
	created, stateChanged                       time.Time
	requestedBy                                 int64
	manifest                                    *manifest.Manifest
	legs                                        []seededLeg
}

func at(t time.Time) *time.Time { return &t }

// insertRun stores a run with one ecs-deploy step; its "step" legs belong to that step.
func (f *runFixture) insertRun(s seededRun) int64 {
	f.t.Helper()
	ctx := context.Background()
	snapshot, _, err := store.ActiveSnapshot(ctx, f.pool)
	if err != nil {
		f.t.Fatalf("ActiveSnapshot: %v", err)
	}
	if s.kind == "" {
		s.kind = "deploy"
	}
	if s.environment == "" {
		s.environment, s.label = devEnvironment, "dev"
	}
	if s.created.IsZero() {
		s.created = time.Now().UTC()
	}
	if s.requestedBy == 0 {
		s.requestedBy = f.admin.ID
	}
	run := store.Run{
		Kind: s.kind, JobSourceID: f.jobID, JobID: "release", JobSnapshot: []byte(`{}`), InventorySnapshotID: snapshot.ID,
		Environment: s.environment, Label: s.label, FolderID: f.folderID, Selection: []byte(`{}`),
		State: s.state, WaitReason: s.waitReason, RequestedBy: s.requestedBy, Via: "web", CreatedAt: s.created,
	}
	if s.manifest != nil {
		run.Manifest = mustMarshal(f.t, s.manifest)
	}
	var legs []store.Leg
	for _, l := range s.legs {
		leg := store.Leg{Kind: l.kind, Service: l.service, Version: l.version, State: l.state,
			QueuedAt: l.queued, StartedAt: l.started, FinishedAt: l.finished}
		if leg.Kind == "" {
			leg.Kind = "step"
			zero := int64(0)
			leg.StepID = &zero
		}
		legs = append(legs, leg)
	}
	stored, err := store.InsertRun(ctx, f.pool, run, []store.RunStep{{Name: "deploy", Use: "ecs-deploy", Position: 0}}, legs)
	if err != nil {
		f.t.Fatalf("InsertRun: %v", err)
	}
	if !s.stateChanged.IsZero() {
		f.exec(`UPDATE runs SET state_changed_at = $2 WHERE id = $1`, stored.ID, s.stateChanged)
	}
	return stored.ID
}

func (f *runFixture) getJSON(token, path string, want int) map[string]any {
	f.t.Helper()
	code, raw := f.doRaw("GET", path, token, nil)
	if code != want {
		f.t.Fatalf("GET %s = %d, want %d: %s", path, code, want, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		f.t.Fatalf("GET %s: decode: %v: %s", path, err, raw)
	}
	return body
}

func asList(t *testing.T, v any, what string) []any {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("%s = %#v, want a list (never null)", what, v)
	}
	return list
}

func asMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want an object", what, v)
	}
	return m
}

func within(t *testing.T, what string, got any, want time.Time, tolerance time.Duration) {
	t.Helper()
	text, ok := got.(string)
	if !ok {
		t.Errorf("%s = %#v, want a time near %s", what, got, want.Format(time.RFC3339))
		return
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Errorf("%s = %q: %v", what, text, err)
		return
	}
	if d := parsed.Sub(want); d > tolerance || d < -tolerance {
		t.Errorf("%s = %s, want within %s of %s", what, text, tolerance, want.Format(time.RFC3339))
	}
}

func TestQueueListsWaitingRunsLocksAndRunnerCapacity(t *testing.T) {
	f := newRunFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r1 := f.insertRun(seededRun{state: "running", created: now.Add(-10 * time.Minute), legs: []seededLeg{
		{service: "s1", state: "claimed"}, {service: "s2", state: "running"}, {service: "s3", state: "queued", queued: at(now.Add(-2 * time.Minute))},
		{service: "s4", state: "succeeded"}}})
	r2 := f.insertRun(seededRun{state: "queued", created: now.Add(-9 * time.Minute), stateChanged: now.Add(-90 * time.Second),
		waitReason: fmt.Sprintf("waiting for run #%d", r1), environment: prodEnvironment, label: "prod",
		legs: []seededLeg{{service: "s1", state: "queued", queued: at(now.Add(-8 * time.Minute))}, {service: "s2", state: "queued", queued: at(now.Add(-4 * time.Minute))}}})
	r3 := f.insertRun(seededRun{state: "waiting_approval", created: now.Add(-8 * time.Minute), stateChanged: now.Add(-3 * time.Minute)})
	r4 := f.insertRun(seededRun{state: "needs_reconciliation", created: now.Add(-7 * time.Minute), waitReason: "a leg is unknown"})
	r5 := f.insertRun(seededRun{state: "cancelling", created: now.Add(-6 * time.Minute)})
	f.insertRun(seededRun{state: "planned", created: now.Add(-5 * time.Minute)})
	f.insertRun(seededRun{state: "succeeded", created: now.Add(-4 * time.Minute)})
	if _, err := store.AcquireLocks(ctx, f.pool, r1, []string{"arn:service/b", "arn:service/a"}, now.Add(-9*time.Minute)); err != nil {
		t.Fatalf("AcquireLocks: %v", err)
	}
	for _, runner := range []store.RunnerInfo{
		{ID: "runner-b", Version: "1.2.0", Concurrency: 2, Busy: 3, StartedAt: now.Add(-time.Hour), LastSeenAt: now.Add(-30 * time.Second)},
		{ID: "runner-a", Version: "1.2.0", Concurrency: 4, Busy: 1, StartedAt: now.Add(-time.Hour), LastSeenAt: now},
		{ID: "runner-old", Version: "1.1.0", Concurrency: 8, Busy: 0, StartedAt: now.Add(-3 * time.Hour), LastSeenAt: now.Add(-5 * time.Minute)},
	} {
		if err := store.UpsertRunner(ctx, f.pool, runner); err != nil {
			t.Fatalf("UpsertRunner: %v", err)
		}
	}

	body := f.getJSON(f.adminToken, "/api/v1/queue", http.StatusOK)

	runs := asList(t, body["runs"], "runs")
	var ids []int64
	for _, r := range runs {
		ids = append(ids, int64(asMap(t, r, "run")["id"].(float64)))
	}
	if want := []int64{r1, r2, r3, r4, r5}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("run ids = %v, want %v (waiting states only, oldest first)", ids, want)
	}
	first, second, third := asMap(t, runs[0], "r1"), asMap(t, runs[1], "r2"), asMap(t, runs[2], "r3")
	if first["state"] != "running" || first["legs_running"] != float64(2) || first["legs_queued"] != float64(1) ||
		first["blocked_by_run_id"] != nil || first["job_name"] != "release" || first["job_id"] != "release" || first["environment"] != devEnvironment || first["label"] != "dev" {
		t.Errorf("running run = %v", first)
	}
	if second["state"] != "queued" || second["wait_reason"] != fmt.Sprintf("waiting for run #%d", r1) || second["blocked_by_run_id"] != float64(r1) ||
		second["legs_running"] != float64(0) || second["legs_queued"] != float64(2) || second["environment"] != prodEnvironment || second["label"] != "prod" {
		t.Errorf("queued run = %v", second)
	}
	if w, ok := second["waited_seconds"].(float64); !ok || w < 89 || w > 120 {
		t.Errorf("waited_seconds = %v, want about 90 (the time since the state changed)", second["waited_seconds"])
	}
	within(t, "waiting_since", second["waiting_since"], now.Add(-90*time.Second), 2*time.Second)
	if w, ok := third["waited_seconds"].(float64); !ok || w < 179 || w > 210 || third["blocked_by_run_id"] != nil {
		t.Errorf("approval run = %v, want waited about 180 seconds and nobody blocking", third)
	}
	if reconcile := asMap(t, runs[3], "r4"); reconcile["wait_reason"] != "a leg is unknown" || reconcile["blocked_by_run_id"] != nil {
		t.Errorf("reconcile run = %v", reconcile)
	}

	locks := asList(t, body["locks"], "locks")
	if len(locks) != 2 || asMap(t, locks[0], "lock")["resource"] != "arn:service/a" || asMap(t, locks[1], "lock")["resource"] != "arn:service/b" ||
		asMap(t, locks[0], "lock")["run_id"] != float64(r1) {
		t.Errorf("locks = %v, want both of run %d ordered by resource", locks, r1)
	}
	within(t, "lock acquired_at", asMap(t, locks[0], "lock")["acquired_at"], now.Add(-9*time.Minute), 2*time.Second)

	runners := asList(t, body["runners"], "runners")
	if len(runners) != 2 || asMap(t, runners[0], "runner")["id"] != "runner-a" || asMap(t, runners[1], "runner")["id"] != "runner-b" {
		t.Fatalf("runners = %v, want the two seen in the last 2 minutes ordered by id", runners)
	}
	if a := asMap(t, runners[0], "runner-a"); a["concurrency"] != float64(4) || a["busy"] != float64(1) || a["version"] != "1.2.0" {
		t.Errorf("runner-a = %v", a)
	}
	if body["free_slots"] != float64(3) {
		t.Errorf("free_slots = %v, want 3 (a runner over its concurrency counts 0, not -1)", body["free_slots"])
	}
	if body["queued_legs"] != float64(3) {
		t.Errorf("queued_legs = %v, want 3", body["queued_legs"])
	}
	within(t, "oldest_queued_at", body["oldest_queued_at"], now.Add(-8*time.Minute), 2*time.Second)
	if body["runner_gone_after_seconds"] != float64(120) {
		t.Errorf("runner_gone_after_seconds = %v, want 120", body["runner_gone_after_seconds"])
	}
}

func TestQueueSaysWhenNoRunnerHasReported(t *testing.T) {
	f := newRunFixture(t)
	now := time.Now().UTC()
	f.insertRun(seededRun{state: "queued", created: now.Add(-3 * time.Minute), stateChanged: now.Add(-2 * time.Minute),
		legs: []seededLeg{{service: "s1", state: "queued", queued: at(now.Add(-2 * time.Minute))}}})
	// A runner that went quiet long ago is not a runner.
	if err := store.UpsertRunner(context.Background(), f.pool, store.RunnerInfo{ID: "gone", Version: "1", Concurrency: 8, StartedAt: now.Add(-time.Hour), LastSeenAt: now.Add(-10 * time.Minute)}); err != nil {
		t.Fatalf("UpsertRunner: %v", err)
	}

	body := f.getJSON(f.adminToken, "/api/v1/queue", http.StatusOK)

	if runners := asList(t, body["runners"], "runners"); len(runners) != 0 {
		t.Errorf("runners = %v, want []", runners)
	}
	if body["free_slots"] != float64(0) {
		t.Errorf("free_slots = %v, want 0", body["free_slots"])
	}
	runs := asList(t, body["runs"], "runs")
	if len(runs) != 1 {
		t.Fatalf("runs = %v, want the queued run", runs)
	}
	if w, ok := asMap(t, runs[0], "run")["waited_seconds"].(float64); !ok || w < 119 || w > 150 {
		t.Errorf("waited_seconds = %v, want about 120", asMap(t, runs[0], "run")["waited_seconds"])
	}
	if body["queued_legs"] != float64(1) {
		t.Errorf("queued_legs = %v, want 1", body["queued_legs"])
	}
}

func TestQueueAndHealthListsAreEmptyListsInAnEmptySystem(t *testing.T) {
	f := &runFixture{harness: newHarness(t)} // no inventory, no jobs, no accounts
	queue := f.getJSON(f.adminToken, "/api/v1/queue", http.StatusOK)
	for _, name := range []string{"runs", "locks", "runners"} {
		if got := asList(t, queue[name], "queue."+name); len(got) != 0 {
			t.Errorf("queue.%s = %v, want []", name, got)
		}
	}
	if queue["oldest_queued_at"] != nil || queue["queued_legs"] != float64(0) || queue["free_slots"] != float64(0) {
		t.Errorf("queue = %v, want no oldest leg and nothing queued", queue)
	}

	health := f.getJSON(f.adminToken, "/api/v1/system/health", http.StatusOK)
	if got := asList(t, health["runners"], "runners"); len(got) != 0 {
		t.Errorf("health.runners = %v, want []", got)
	}
	if got := asList(t, health["connections"], "connections"); len(got) != 0 {
		t.Errorf("health.connections = %v, want []", got)
	}
	sync := asMap(t, health["sync"], "sync")
	if got := asList(t, sync["jobs"], "sync.jobs"); len(got) != 0 {
		t.Errorf("sync.jobs = %v, want []", got)
	}
	problems := asList(t, health["problems"], "problems")
	if len(problems) != 2 || problems[0] != "No runner has reported for 2 minutes." || problems[1] != "The inventory has never been checked." {
		t.Errorf("problems = %v, want the two an empty system has", problems)
	}
}

func TestQueueAndServicePageAreOpenToEveryoneAndHealthIsNot(t *testing.T) {
	f := newRunFixture(t)
	f.rebuild(func(d *Deps) { d.Running = &fakeRunning{} })
	plain := f.newUser("visibilityplain")
	plainToken, _ := f.newToken(plain.ID)
	for _, path := range []string{"/api/v1/queue", "/api/v1/services/s1"} {
		if code, raw := f.doRaw("GET", path, plainToken, nil); code != http.StatusOK {
			t.Errorf("GET %s as a viewer = %d: %s", path, code, raw)
		}
		if code, _ := f.doRaw("GET", path, "", nil); code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, code)
		}
	}
	if code, _ := f.doRaw("GET", "/api/v1/system/health", plainToken, nil); code != http.StatusForbidden {
		t.Errorf("health as a viewer = %d, want 403", code)
	}
	manager := f.newUser("visibilitymanager")
	f.grant(manager.ID, "manage_connections")
	managerToken, _ := f.newToken(manager.ID)
	for name, token := range map[string]string{"manage_connections": managerToken, "admin": f.adminToken} {
		if code, raw := f.doRaw("GET", "/api/v1/system/health", token, nil); code != http.StatusOK {
			t.Errorf("health as %s = %d: %s", name, code, raw)
		}
	}
}

func deployManifest(previousTaskDefinition string, config ...manifest.ConfigItem) *manifest.Manifest {
	return &manifest.Manifest{Services: []manifest.Service{
		{Name: "s1", PreviousTaskDefinition: previousTaskDefinition, Config: config},
		{Name: "s2", PreviousTaskDefinition: "arn:aws:ecs:eu-west-1:1:task-definition/s2:9"},
	}}
}

func TestServicePageShowsRunningHistoryAndRestoreSource(t *testing.T) {
	f := newRunFixture(t)
	f.rebuild(func(d *Deps) { d.Running = &fakeRunning{} })
	now := time.Now().UTC()
	nameless, err := store.CreateLocalUser(context.Background(), f.pool, "nameless@example.test", "", "local", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	const previous = "arn:aws:ecs:eu-west-1:1:task-definition/s1:3"
	toml := manifest.ConfigItem{Kind: "toml", Name: "app", Destination: "s3://cfg/staging/s1.toml", SHA256: "0123456789abcdef0123456789abcdef"}
	env := manifest.ConfigItem{Kind: "env", Name: "secrets", Destination: "s3://cfg/staging/s1.env", SHA256: "fedcba9876543210"}

	// Oldest first, so ids and legs grow with time.
	a := f.insertRun(seededRun{kind: "restore", state: "partially_succeeded", created: now.Add(-6 * time.Hour), manifest: deployManifest(previous, toml),
		legs: []seededLeg{{service: "s1", version: "1.0.0", state: "succeeded", queued: at(now.Add(-6 * time.Hour)), started: at(now.Add(-6 * time.Hour)), finished: at(now.Add(-5*time.Hour - 50*time.Minute))}}})
	c := f.insertRun(seededRun{state: "succeeded", created: now.Add(-5 * time.Hour), requestedBy: nameless.ID, manifest: deployManifest(""),
		legs: []seededLeg{{service: "s1", version: "1.1.0", state: "succeeded", finished: at(now.Add(-4*time.Hour - 50*time.Minute))}}})
	b := f.insertRun(seededRun{state: "failed", environment: prodEnvironment, label: "prod", created: now.Add(-4 * time.Hour), manifest: deployManifest(previous, env),
		legs: []seededLeg{{service: "s1", version: "1.2.0", state: "failed", started: at(now.Add(-4 * time.Hour)), finished: at(now.Add(-3*time.Hour - 55*time.Minute))}}})
	f.insertRun(seededRun{state: "succeeded", created: now.Add(-3 * time.Hour), manifest: deployManifest(previous),
		legs: []seededLeg{{service: "s2", version: "9.9.9", state: "succeeded", finished: at(now.Add(-3 * time.Hour))}}})
	e := f.insertRun(seededRun{state: "running", created: now.Add(-2 * time.Hour), manifest: deployManifest(previous, toml, env),
		legs: []seededLeg{{service: "s1", version: "1.3.0", state: "running", started: at(now.Add(-2 * time.Hour))}, {kind: "revalidate", state: "succeeded"}}})
	i := f.insertRun(seededRun{state: "queued", environment: prodEnvironment, label: "prod", created: now.Add(-time.Hour),
		legs: []seededLeg{{service: "s1", version: "1.4.0", state: "pending"}}})

	body := f.getJSON(f.adminToken, "/api/v1/services/s1", http.StatusOK)

	if service := asMap(t, body["service"], "service"); service["name"] != "s1" {
		t.Errorf("service = %v, want the inventory entry of s1", service)
	}
	envs := asList(t, body["environments"], "environments")
	if len(envs) != 2 {
		t.Fatalf("environments = %v, want the two inventory environments in order", envs)
	}
	dev, prod := asMap(t, envs[0], "dev"), asMap(t, envs[1], "prod")
	if dev["environment"] != devEnvironment || dev["label"] != "dev" || prod["environment"] != prodEnvironment || prod["label"] != "prod" {
		t.Errorf("environments = %v %v, want staging then prod-eu", dev, prod)
	}
	if cell := asMap(t, dev["cell"], "cell"); cell["state"] != "ok" || cell["version"] != "1.4.2" {
		t.Errorf("staging cell = %v, want the running version from the reader", cell)
	}
	if dev["restore_from_run_id"] != float64(a) || dev["restore_from_folder_id"] != float64(f.folderID) {
		t.Errorf("staging restore source = %v / %v, want run %d (the newest run with a succeeded deploy and a previous task definition; run %d has none) in folder %d",
			dev["restore_from_run_id"], dev["restore_from_folder_id"], a, c, f.folderID)
	}
	if prod["restore_from_run_id"] != nil || prod["restore_from_folder_id"] != nil {
		t.Errorf("prod-eu restore source = %v / %v, want none (its only deploy failed)", prod["restore_from_run_id"], prod["restore_from_folder_id"])
	}

	deployments := asList(t, body["deployments"], "deployments")
	var runIDs []int64
	for _, d := range deployments {
		runIDs = append(runIDs, int64(asMap(t, d, "deployment")["run_id"].(float64)))
	}
	if want := []int64{i, e, b, c, a}; !reflect.DeepEqual(runIDs, want) {
		t.Fatalf("deployment runs = %v, want %v (newest leg first, this service only, step legs only)", runIDs, want)
	}
	last := asMap(t, deployments[4], "oldest deployment")
	if last["environment"] != devEnvironment || last["version"] != "1.0.0" || last["state"] != "succeeded" || last["by"] != "admin" || last["leg_id"] == nil {
		t.Errorf("oldest deployment = %v", last)
	}
	within(t, "finished deployment at", last["at"], now.Add(-5*time.Hour-50*time.Minute), 2*time.Second)
	if by := asMap(t, deployments[3], "deployment c")["by"]; by != "nameless@example.test" {
		t.Errorf("by = %v, want the email when the requester has no name", by)
	}
	within(t, "unfinished deployment at (the start)", asMap(t, deployments[1], "deployment e")["at"], now.Add(-2*time.Hour), 2*time.Second)
	within(t, "deployment with no times at (the run's creation)", asMap(t, deployments[0], "deployment i")["at"], now.Add(-time.Hour), 2*time.Second)

	config := asList(t, body["config"], "config")
	runIDs = nil
	for _, c := range config {
		runIDs = append(runIDs, int64(asMap(t, c, "config change")["run_id"].(float64)))
	}
	if want := []int64{e, b, a}; !reflect.DeepEqual(runIDs, want) {
		t.Fatalf("config runs = %v, want %v (newest first; only runs whose manifest has config for s1)", runIDs, want)
	}
	newest := asMap(t, config[0], "config e")
	items := asList(t, newest["items"], "items")
	if newest["environment"] != devEnvironment || len(items) != 2 {
		t.Fatalf("config of run %d = %v", e, newest)
	}
	if item := asMap(t, items[0], "item"); item["kind"] != "toml" || item["name"] != "app" || item["destination"] != "s3://cfg/staging/s1.toml" || item["checksum"] != "0123456789ab" {
		t.Errorf("first item = %v, want the toml with a 12 character checksum", item)
	}
	if item := asMap(t, items[1], "item"); item["checksum"] != "fedcba987654" {
		t.Errorf("second item = %v", item)
	}
	within(t, "config at", newest["at"], now.Add(-2*time.Hour), 2*time.Second)
}

func TestServicePageKeepsOnlyTheLast50DeploymentsAndConfigChanges(t *testing.T) {
	f := newRunFixture(t)
	f.rebuild(func(d *Deps) { d.Running = &fakeRunning{} })
	now := time.Now().UTC()
	item := manifest.ConfigItem{Kind: "toml", Name: "app", Destination: "d", SHA256: "0123456789abcdef"}
	for n := 0; n < 55; n++ {
		f.insertRun(seededRun{state: "succeeded", created: now.Add(time.Duration(n-100) * time.Minute), manifest: deployManifest("td", item),
			legs: []seededLeg{{service: "s1", version: fmt.Sprintf("1.0.%d", n), state: "succeeded"}}})
	}
	body := f.getJSON(f.adminToken, "/api/v1/services/s1", http.StatusOK)
	deployments, config := asList(t, body["deployments"], "deployments"), asList(t, body["config"], "config")
	if len(deployments) != 50 || len(config) != 50 {
		t.Fatalf("deployments = %d, config = %d, want 50 each", len(deployments), len(config))
	}
	if v := asMap(t, deployments[0], "newest")["version"]; v != "1.0.54" {
		t.Errorf("newest deployment version = %v, want 1.0.54", v)
	}
}

func TestServicePageOfAServiceWithNoHistoryHasEmptyLists(t *testing.T) {
	f := newRunFixture(t)
	f.rebuild(func(d *Deps) { d.Running = &fakeRunning{} })
	body := f.getJSON(f.adminToken, "/api/v1/services/s3", http.StatusOK)
	for _, name := range []string{"deployments", "config"} {
		if got := asList(t, body[name], name); len(got) != 0 {
			t.Errorf("%s = %v, want []", name, got)
		}
	}
	for _, e := range asList(t, body["environments"], "environments") {
		if env := asMap(t, e, "environment"); env["restore_from_run_id"] != nil || env["restore_from_folder_id"] != nil {
			t.Errorf("environment = %v, want no restore source", env)
		}
	}
	code, raw := f.doRaw("GET", "/api/v1/services/nope", f.adminToken, nil)
	if code != http.StatusNotFound || !strings.Contains(string(raw), `"not_found"`) {
		t.Errorf("unknown service = %d %s, want 404 not_found", code, raw)
	}
}

// healthRow starts from a healthy system (a runner, the inventory just
// checked) and changes one thing.
type healthRow struct {
	name       string
	runnerGone time.Duration // Deps.RunnerGoneAfter; zero is the default
	setup      func(f *runFixture)
	want       []string
}

func (f *runFixture) makeHealthy() {
	f.t.Helper()
	now := time.Now().UTC()
	if err := store.UpsertRunner(context.Background(), f.pool, store.RunnerInfo{ID: "r1", Version: "1", Concurrency: 4, StartedAt: now.Add(-time.Hour), LastSeenAt: now}); err != nil {
		f.t.Fatalf("UpsertRunner: %v", err)
	}
	f.exec(`UPDATE inventory_status SET checked_at = $1`, now)
}

func (f *runFixture) events(kind string, count int, age time.Duration) {
	f.t.Helper()
	for range count {
		if err := store.RecordEvent(context.Background(), f.pool, kind, time.Now().Add(-age)); err != nil {
			f.t.Fatalf("RecordEvent: %v", err)
		}
	}
}

func (f *runFixture) failedNotifications(count int, age time.Duration) {
	f.t.Helper()
	run := f.insertRun(seededRun{state: "failed"})
	for n := range count {
		f.exec(`INSERT INTO notifications (run_id, kind, channel, state, attempts, next_attempt_at, failed_at) VALUES ($1, $2, '#deploys', 'failed', 5, now(), $3)`,
			run, fmt.Sprintf("kind-%d", n), time.Now().Add(-age))
	}
}

func (f *runFixture) connectionFailed(kind, ref, code string) {
	f.t.Helper()
	if err := store.RecordConnectionError(context.Background(), f.pool, kind, ref, code, time.Now()); err != nil {
		f.t.Fatalf("RecordConnectionError: %v", err)
	}
}

func (f *runFixture) githubConnection(owner string) {
	f.t.Helper()
	if _, err := store.CreateConnection(context.Background(), f.pool, store.Connection{Owner: owner, Method: "token"}, []byte("x")); err != nil {
		f.t.Fatalf("CreateConnection: %v", err)
	}
}

func (f *runFixture) slackToken() {
	f.t.Helper()
	if err := notify.SaveSlackToken(context.Background(), f.pool, f.cipher, "xoxb-test-token", f.admin.ID, time.Now()); err != nil {
		f.t.Fatalf("SaveSlackToken: %v", err)
	}
}

func TestHealthProblems(t *testing.T) {
	const awsName = "test-111122223333" // the account the fixture connects
	rows := []healthRow{
		{name: "healthy", want: []string{}},
		{name: "no runner for the default 2 minutes", setup: func(f *runFixture) { f.exec(`DELETE FROM runners`) },
			want: []string{"No runner has reported for 2 minutes."}},
		{name: "no runner, a shorter configured window in seconds", runnerGone: 30 * time.Second,
			setup: func(f *runFixture) { f.exec(`UPDATE runners SET last_seen_at = now() - interval '1 minute'`) },
			want:  []string{"No runner has reported for 30 seconds."}},
		{name: "no runner, a window of one minute", runnerGone: time.Minute,
			setup: func(f *runFixture) { f.exec(`UPDATE runners SET last_seen_at = now() - interval '5 minutes'`) },
			want:  []string{"No runner has reported for 1 minute."}},
		{name: "oldest queued leg older than 5 minutes", setup: func(f *runFixture) {
			f.insertRun(seededRun{state: "queued", legs: []seededLeg{{service: "s1", state: "queued", queued: at(time.Now().Add(-7 * time.Minute))}}})
		}, want: []string{"The oldest queued leg has waited 7 minutes."}},
		{name: "a queued leg of 2 minutes is not a problem", setup: func(f *runFixture) {
			f.insertRun(seededRun{state: "queued", legs: []seededLeg{{service: "s1", state: "queued", queued: at(time.Now().Add(-2 * time.Minute))}}})
		}, want: []string{}},
		{name: "inventory never checked", setup: func(f *runFixture) { f.exec(`UPDATE inventory_status SET checked_at = NULL`) },
			want: []string{"The inventory has never been checked."}},
		{name: "inventory not checked for 15 minutes", setup: func(f *runFixture) {
			f.exec(`UPDATE inventory_status SET checked_at = now() - interval '15 minutes'`)
		}, want: []string{"The inventory has not been checked for 15 minutes."}},
		{name: "inventory checked 9 minutes ago is fine", setup: func(f *runFixture) {
			f.exec(`UPDATE inventory_status SET checked_at = now() - interval '9 minutes'`)
		}, want: []string{}},
		{name: "one log batch could not be stored", setup: func(f *runFixture) { f.events("log_persist_failed", 1, time.Minute) },
			want: []string{"1 log batch could not be stored in the last hour."}},
		{name: "three log batches could not be stored", setup: func(f *runFixture) { f.events("log_persist_failed", 3, 10*time.Minute) },
			want: []string{"3 log batches could not be stored in the last hour."}},
		{name: "a log failure two hours ago is not in the last hour", setup: func(f *runFixture) { f.events("log_persist_failed", 3, 2*time.Hour) },
			want: []string{}},
		{name: "six leases expired", setup: func(f *runFixture) { f.events("lease_expired", 6, 5*time.Minute) },
			want: []string{"6 leases expired in the last hour."}},
		{name: "five leases expired is not more than 5", setup: func(f *runFixture) { f.events("lease_expired", 5, 5*time.Minute) },
			want: []string{}},
		{name: "one failed notification", setup: func(f *runFixture) { f.failedNotifications(1, time.Hour) },
			want: []string{"1 notification failed in the last day."}},
		{name: "two failed notifications", setup: func(f *runFixture) { f.failedNotifications(2, 23*time.Hour) },
			want: []string{"2 notifications failed in the last day."}},
		{name: "a notification that failed two days ago", setup: func(f *runFixture) { f.failedNotifications(1, 48*time.Hour) },
			want: []string{}},
		{name: "an AWS account whose last attempt failed", setup: func(f *runFixture) { f.connectionFailed("aws", "111122223333", "AccessDenied") },
			want: []string{"AWS account " + awsName + " failed its last attempt: AccessDenied."}},
		{name: "a GitHub connection whose last attempt failed", setup: func(f *runFixture) {
			f.githubConnection("acme")
			f.connectionFailed("github", "acme", "RateLimited")
		}, want: []string{"GitHub connection acme failed its last attempt: RateLimited."}},
		{name: "Slack whose last attempt failed", setup: func(f *runFixture) {
			f.slackToken()
			f.connectionFailed("slack", "slack", "invalid_auth")
		}, want: []string{"Slack failed its last attempt: invalid_auth."}},
		{name: "a connection that failed and then worked", setup: func(f *runFixture) {
			f.connectionFailed("aws", "111122223333", "AccessDenied")
			if err := store.RecordConnectionOK(context.Background(), f.pool, "aws", "111122223333", time.Now().Add(time.Second)); err != nil {
				f.t.Fatalf("RecordConnectionOK: %v", err)
			}
		}, want: []string{}},
		{name: "a failed check of an account that is no longer connected", setup: func(f *runFixture) { f.connectionFailed("aws", "999999999999", "AccessDenied") },
			want: []string{}},
		{name: "everything at once, in the documented order", setup: func(f *runFixture) {
			f.exec(`DELETE FROM runners`)
			f.insertRun(seededRun{state: "queued", legs: []seededLeg{{service: "s1", state: "queued", queued: at(time.Now().Add(-9 * time.Minute))}}})
			f.exec(`UPDATE inventory_status SET checked_at = now() - interval '20 minutes'`)
			f.events("log_persist_failed", 2, time.Minute)
			f.events("lease_expired", 7, time.Minute)
			f.failedNotifications(1, time.Hour)
			f.githubConnection("acme")
			f.slackToken()
			f.connectionFailed("slack", "slack", "invalid_auth")
			f.connectionFailed("github", "acme", "RateLimited")
			f.connectionFailed("aws", "444455556666", "ExpiredToken")
		}, want: []string{
			"No runner has reported for 2 minutes.",
			"The oldest queued leg has waited 9 minutes.",
			"The inventory has not been checked for 20 minutes.",
			"2 log batches could not be stored in the last hour.",
			"7 leases expired in the last hour.",
			"1 notification failed in the last day.",
			"AWS account test-444455556666 failed its last attempt: ExpiredToken.",
			"GitHub connection acme failed its last attempt: RateLimited.",
			"Slack failed its last attempt: invalid_auth.",
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newRunFixture(t)
			if row.runnerGone != 0 {
				f.rebuild(func(d *Deps) { d.RunnerGoneAfter = row.runnerGone })
			}
			f.makeHealthy()
			if row.setup != nil {
				row.setup(f)
			}
			body := f.getJSON(f.adminToken, "/api/v1/system/health", http.StatusOK)
			got := asList(t, body["problems"], "problems")
			gotText := make([]string, len(got))
			for i, p := range got {
				gotText[i], _ = p.(string)
			}
			if !reflect.DeepEqual(gotText, row.want) {
				t.Errorf("problems = %q, want %q", gotText, row.want)
			}
		})
	}
}

func TestHealthReportsRunnersCountersSyncConnectionsAndNotifications(t *testing.T) {
	f := newRunFixture(t)
	ctx := context.Background()
	f.makeHealthy()
	now := time.Now().UTC()
	f.events("lease_expired", 2, 10*time.Minute)
	f.events("lease_expired", 1, 5*time.Hour)
	f.events("sync_failed", 1, 30*time.Hour)
	f.events("log_persist_failed", 1, 20*time.Minute)
	f.exec(`UPDATE inventory_status SET sync_error = 'GitHub did not answer', sync_error_at = $1`, now.Add(-time.Minute))
	f.exec(`UPDATE job_sources SET checked_at = $2, sync_error = 'rate limited', sync_error_at = $3 WHERE id = $1`, f.otherJobID, now.Add(-2*time.Minute), now.Add(-time.Minute))
	f.githubConnection("acme")
	f.slackToken()
	f.connectionFailed("github", "acme", "RateLimited")
	if err := store.RecordConnectionOK(ctx, f.pool, "aws", "111122223333", now.Add(-time.Minute)); err != nil {
		t.Fatalf("RecordConnectionOK: %v", err)
	}
	// Two pending messages (the older one first) and one failed an hour ago.
	run := f.insertRun(seededRun{state: "failed"})
	f.exec(`INSERT INTO notifications (run_id, kind, channel, state, next_attempt_at, created_at) VALUES ($1, 'a', '#c', 'pending', now(), $2), ($1, 'b', '#c', 'pending', now(), $3)`,
		run, now.Add(-30*time.Minute), now.Add(-10*time.Minute))
	f.failedNotifications(1, time.Hour)

	body := f.getJSON(f.adminToken, "/api/v1/system/health", http.StatusOK)

	runners := asList(t, body["runners"], "runners")
	if len(runners) != 1 || asMap(t, runners[0], "runner")["id"] != "r1" || body["runner_gone_after_seconds"] != float64(120) {
		t.Errorf("runners = %v, gone after %v", runners, body["runner_gone_after_seconds"])
	}
	queue := asMap(t, body["queue"], "queue")
	if queue["queued_legs"] != float64(0) || queue["oldest_queued_at"] != nil || queue["active_runs"] != float64(0) {
		t.Errorf("queue = %v, want nothing queued and no active runs", queue)
	}
	counters := asMap(t, body["counters"], "counters")
	want := map[string][2]float64{"lease_expired": {2, 3}, "log_persist_failed": {1, 1}, "sync_failed": {0, 0}}
	for kind, hourDay := range want {
		counter := asMap(t, counters[kind], kind)
		if counter["hour"] != hourDay[0] || counter["day"] != hourDay[1] {
			t.Errorf("counters.%s = %v, want hour %v and day %v (sync_failed is 30 hours old, outside the day)", kind, counter, hourDay[0], hourDay[1])
		}
	}
	syncStatus := asMap(t, body["sync"], "sync")
	inventory := asMap(t, syncStatus["inventory"], "inventory")
	if inventory["error"] != "GitHub did not answer" || inventory["checked_at"] == nil || inventory["error_at"] == nil {
		t.Errorf("sync.inventory = %v", inventory)
	}
	jobs := asList(t, syncStatus["jobs"], "jobs")
	if len(jobs) != 2 {
		t.Fatalf("sync.jobs = %v, want both job sources", jobs)
	}
	var withError map[string]any
	for _, j := range jobs {
		job := asMap(t, j, "job")
		if job["id"] == float64(f.otherJobID) {
			withError = job
		}
	}
	if withError == nil || withError["job_id"] != "hotfix" || withError["error"] != "rate limited" || withError["checked_at"] == nil {
		t.Errorf("sync job = %v, want the hotfix source with its error text", withError)
	}
	if first, second := asMap(t, jobs[0], "job")["job_id"], asMap(t, jobs[1], "job")["job_id"]; first != "hotfix" || second != "release" {
		t.Errorf("job order = %v, %v, want ordered by job_id", first, second)
	}

	connections := asList(t, body["connections"], "connections")
	var order []string
	for _, c := range connections {
		conn := asMap(t, c, "connection")
		order = append(order, fmt.Sprintf("%v/%v", conn["kind"], conn["name"]))
	}
	if wantOrder := []string{"aws/test-111122223333", "aws/test-444455556666", "github/acme", "slack/Slack"}; !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("connections = %v, want %v (aws, github, slack, then name)", order, wantOrder)
	}
	aws0, never, github, slack := asMap(t, connections[0], "aws0"), asMap(t, connections[1], "aws1"), asMap(t, connections[2], "github"), asMap(t, connections[3], "slack")
	if aws0["failing"] != false || aws0["last_ok_at"] == nil || aws0["last_error_at"] != nil || aws0["last_error_code"] != "" {
		t.Errorf("a working account = %v", aws0)
	}
	if never["failing"] != false || never["last_ok_at"] != nil || never["last_error_at"] != nil {
		t.Errorf("an account never checked = %v, want nulls and not failing", never)
	}
	if github["failing"] != true || github["last_error_code"] != "RateLimited" || github["last_error_at"] == nil {
		t.Errorf("github = %v", github)
	}
	if slack["failing"] != false {
		t.Errorf("slack = %v", slack)
	}

	notifications := asMap(t, body["notifications"], "notifications")
	if notifications["pending"] != float64(2) || notifications["failed_last_day"] != float64(1) {
		t.Errorf("notifications = %v, want 2 pending and 1 failed in the last day", notifications)
	}
	within(t, "oldest_pending_at", notifications["oldest_pending_at"], now.Add(-30*time.Minute), 2*time.Second)
}
