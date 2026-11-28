//go:build integration

package running

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/rcss-labs/yamdrake/internal/cache"
	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/testutil/testdb"
)

type dbFixture struct {
	t                                      *testing.T
	pool                                   *pgxpool.Pool
	userID, folderID, sourceID, snapshotID int64
}

func newDBFixture(t *testing.T) *dbFixture {
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
	user, err := store.CreateLocalUser(ctx, pool, "running@example.com", "Running", "local", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	folder, err := store.CreateFolder(ctx, pool, "running-folder")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	source, err := store.CreateJobSource(ctx, pool, store.JobSource{FolderID: folder.ID, Repo: "acme/jobs", Branch: "main", Path: "deploy.yaml"})
	if err != nil {
		t.Fatalf("CreateJobSource: %v", err)
	}
	snapshot, err := store.ActivateSnapshot(ctx, pool, "running-inventory", []byte(`{}`), nil, time.Now())
	if err != nil {
		t.Fatalf("ActivateSnapshot: %v", err)
	}
	return &dbFixture{t: t, pool: pool, userID: user.ID, folderID: folder.ID, sourceID: source.ID, snapshotID: snapshot.ID}
}

// run inserts a run of environment env with an ecs-deploy step (position 0)
// and a config-sync step (position 1), and returns the run and the two step ids.
func (f *dbFixture) run(env string) (runID, deployStep, otherStep int64) {
	f.t.Helper()
	ctx := context.Background()
	run, err := store.InsertRun(ctx, f.pool, store.Run{
		Kind: "deploy", InventorySnapshotID: f.snapshotID, JobSourceID: f.sourceID, JobID: "deploy-api", JobSnapshot: []byte(`{"id":"deploy-api"}`),
		Environment: env, FolderID: f.folderID, Selection: []byte(`{}`), State: "running", RequestedBy: f.userID, Via: "web",
	}, []store.RunStep{
		{Name: "deploy", Use: "ecs-deploy", Position: 0, Needs: []byte(`[]`)},
		{Name: "config", Use: "config-sync", Position: 1, Needs: []byte(`[]`)},
	}, nil)
	if err != nil {
		f.t.Fatalf("InsertRun: %v", err)
	}
	steps, err := store.StepsOfRun(ctx, f.pool, run.ID)
	if err != nil || len(steps) != 2 {
		f.t.Fatalf("StepsOfRun: %v", err)
	}
	return run.ID, steps[0].ID, steps[1].ID
}

// leg inserts a leg; a final state (succeeded, failed) is finished, others are not.
func (f *dbFixture) leg(runID int64, stepID *int64, kind, service, state string) int64 {
	f.t.Helper()
	leg := store.Leg{RunID: runID, StepID: stepID, Kind: kind, Service: service, State: state}
	if state == "succeeded" || state == "failed" {
		finished := time.Now()
		leg.FinishedAt = &finished
	}
	got, err := store.InsertLeg(context.Background(), f.pool, leg)
	if err != nil {
		f.t.Fatalf("InsertLeg: %v", err)
	}
	return got.ID
}

func tagsFor(run, leg int64) map[string]string {
	return map[string]string{"yumdrake:run": strconv.FormatInt(run, 10), "yumdrake:leg": strconv.FormatInt(leg, 10)}
}

func TestDeployedOutsideYumdrakeIsMarked(t *testing.T) {
	f := newDBFixture(t)
	w := newWorld()
	envs := []spec.Environment{envEU, envUS}

	euRun, euDeploy, euOther := f.run("prod-eu")
	usRun, usDeploy, _ := f.run("prod-us")
	okLeg := f.leg(euRun, &euDeploy, "step", "ok", "succeeded")
	f.leg(euRun, &euDeploy, "step", "other-service", "succeeded")
	usLeg := f.leg(usRun, &usDeploy, "step", "otherenv", "succeeded")
	notDeployLeg := f.leg(euRun, &euOther, "step", "notdeploy", "succeeded")
	resolveLeg := f.leg(euRun, nil, "resolve", "resolveleg", "succeeded")
	secondRun, secondDeploy, _ := f.run("prod-eu")
	secondLeg := f.leg(secondRun, &secondDeploy, "step", "wrongrun", "succeeded")

	cases := []struct {
		service string
		tags    map[string]string
		by      bool
		runID   int64
	}{
		{"ok", tagsFor(euRun, okLeg), true, euRun},
		{"wrongsvc", tagsFor(euRun, okLeg), false, 0},         // the leg deployed another service
		{"otherenv", tagsFor(usRun, usLeg), false, 0},         // the run is of another environment
		{"notdeploy", tagsFor(euRun, notDeployLeg), false, 0}, // the leg is not an ecs-deploy step
		{"resolveleg", tagsFor(euRun, resolveLeg), false, 0},  // a resolve leg deploys nothing
		{"noleg", tagsFor(euRun, 999999), false, 0},           // no such leg
		{"wrongrun", tagsFor(euRun, secondLeg), false, 0},     // the leg belongs to another run
		{"outside", nil, false, 0},                            // nobody tagged it
		{"garbage", map[string]string{"yumdrake:run": "x", "yumdrake:leg": "y"}, false, 0},
		{"halftagged", map[string]string{"yumdrake:run": strconv.FormatInt(euRun, 10)}, false, 0},
	}
	var services []spec.Service
	for _, tc := range cases {
		w.run(acctEU, "main", tc.service, repoURI(acctEU, tc.service)+":1.0.0", tc.tags)
		services = append(services, serviceSpec(tc.service, "semver", envEU))
	}
	// The tagged "otherenv" fixture lives in prod-eu on purpose: its tags name a prod-us run.
	svc := newService(t, w, Config{DB: f.pool})
	m, err := svc.Matrix(context.Background(), inventoryOf(envs, services...))
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}
	for _, tc := range cases {
		c := cellOf(t, m, tc.service, "prod-eu")
		if c.ByYumdrake != tc.by || c.RunID != tc.runID {
			t.Errorf("%s: by_yumdrake %v run_id %d, want %v %d", tc.service, c.ByYumdrake, c.RunID, tc.by, tc.runID)
		}
	}
}

func TestLastDeployFailedFromNewestLeg(t *testing.T) {
	f := newDBFixture(t)
	w := newWorld()
	envs := []spec.Environment{envEU, envUS}
	euRun, euDeploy, euOther := f.run("prod-eu")
	usRun, usDeploy, _ := f.run("prod-us")

	// failed then succeeded: the newest finished leg decides, so not failed.
	f.leg(euRun, &euDeploy, "step", "recovered", "failed")
	f.leg(euRun, &euDeploy, "step", "recovered", "succeeded")
	// succeeded then failed: failed.
	f.leg(euRun, &euDeploy, "step", "broken", "succeeded")
	f.leg(euRun, &euDeploy, "step", "broken", "failed")
	// failed, then a newer leg still running: the newest finished one counts.
	f.leg(euRun, &euDeploy, "step", "inflight", "failed")
	f.leg(euRun, &euDeploy, "step", "inflight", "running")
	// a newer failed leg of another environment or of a non-deploy step is not this one's.
	f.leg(euRun, &euDeploy, "step", "elsewhere", "succeeded")
	f.leg(usRun, &usDeploy, "step", "elsewhere", "failed")
	f.leg(euRun, &euDeploy, "step", "otherstep", "succeeded")
	f.leg(euRun, &euOther, "step", "otherstep", "failed")
	// no legs at all.
	var services []spec.Service
	for _, name := range []string{"recovered", "broken", "inflight", "elsewhere", "otherstep", "never"} {
		w.run(acctEU, "main", name, repoURI(acctEU, name)+":1.0.0", nil)
		services = append(services, serviceSpec(name, "semver", envEU))
	}
	m, err := newService(t, w, Config{DB: f.pool}).Matrix(context.Background(), inventoryOf(envs, services...))
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}
	for service, want := range map[string]bool{"recovered": false, "broken": true, "inflight": true, "elsewhere": false, "otherstep": false, "never": false} {
		if got := cellOf(t, m, service, "prod-eu").LastDeployFailed; got != want {
			t.Errorf("%s: last_deploy_failed = %v, want %v", service, got, want)
		}
	}
}

func openTestValkey(t *testing.T) valkey.Client {
	t.Helper()
	addr := os.Getenv("YUMDRAKE_TEST_VALKEY_ADDR")
	if addr == "" {
		t.Fatal("YUMDRAKE_TEST_VALKEY_ADDR must be set when running integration tests")
	}
	client, err := cache.Open(cache.Options{Addr: addr})
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

func ttlMillis(t *testing.T, client valkey.Client, key string) int64 {
	t.Helper()
	ms, err := client.Do(context.Background(), client.B().Pttl().Key(key).Build()).AsInt64()
	if err != nil {
		t.Fatalf("PTTL %s: %v", key, err)
	}
	return ms
}

func TestCacheTwoMinutesAndWorksWithoutValkey(t *testing.T) {
	ctx := context.Background()
	envs := []spec.Environment{envEU, envUS}
	build := func() (*world, spec.Inventory) {
		w := newWorld()
		w.run(acctEU, "main", "api", repoURI(acctEU, "api")+":1.0.0", nil)
		w.run(acctUS, "main", "api", repoURI(acctUS, "api")+":1.0.0", nil)
		return w, inventoryOf(envs, serviceSpec("api", "semver", envs...))
	}

	// Without Valkey every call reads AWS.
	w, inv := build()
	svc := newService(t, w, Config{})
	for range 2 {
		if _, err := svc.Matrix(ctx, inv); err != nil {
			t.Fatalf("Matrix without Valkey: %v", err)
		}
	}
	if got := w.ecsCalls.Load(); got != 8 {
		t.Errorf("ECS calls after two reads without Valkey = %d, want 8 (2 environments x 2 calls x 2 reads)", got)
	}

	client := openTestValkey(t)
	prefix := fmt.Sprintf("running-test-%d:", time.Now().UnixNano())
	t.Cleanup(func() {
		keys, _ := client.Do(ctx, client.B().Keys().Pattern(prefix+"*").Build()).AsStrSlice()
		for _, key := range keys {
			client.Do(ctx, client.B().Del().Key(key).Build())
		}
	})
	keyOf := func(env string) string {
		t.Helper()
		keys, err := client.Do(ctx, client.B().Keys().Pattern(prefix+"running:"+env+":*").Build()).AsStrSlice()
		if err != nil || len(keys) != 1 {
			t.Fatalf("keys for %s = %v, %v; want exactly one under the prefix", env, keys, err)
		}
		return keys[0]
	}

	w, inv = build()
	w.failCluster[acctUS+"|main"] = &smithy.GenericAPIError{Code: "AccessDeniedException"}
	svc = newService(t, w, Config{Valkey: client, KeyPrefix: prefix})
	first, err := svc.Matrix(ctx, inv)
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}
	calls := w.ecsCalls.Load()
	second, err := svc.Matrix(ctx, inv)
	if err != nil {
		t.Fatalf("second Matrix: %v", err)
	}
	if got := w.ecsCalls.Load(); got != calls {
		t.Errorf("ECS calls grew from %d to %d on a cached read", calls, got)
	}
	if cellOf(t, second, "api", "prod-eu").Version != "1.0.0" || cellOf(t, second, "api", "prod-us").Error != "AccessDeniedException" ||
		!cellOf(t, second, "api", "prod-eu").ReadAt.Equal(cellOf(t, first, "api", "prod-eu").ReadAt) {
		t.Errorf("cached matrix differs from the first: %+v vs %+v", second.Rows, first.Rows)
	}

	// 2 minutes when every cell is fine, 20 seconds when one failed.
	if ttl := ttlMillis(t, client, keyOf("prod-eu")); ttl <= 100_000 || ttl > 120_000 {
		t.Errorf("TTL of a healthy environment = %d ms, want just under 120000", ttl)
	}
	if ttl := ttlMillis(t, client, keyOf("prod-us")); ttl <= 0 || ttl > 20_000 {
		t.Errorf("TTL of a failed environment = %d ms, want at most 20000", ttl)
	}

	// A fresh read goes to AWS again and writes the cache again.
	before := w.ecsCalls.Load()
	client.Do(ctx, client.B().Expire().Key(keyOf("prod-eu")).Seconds(30).Build())
	if _, err := svc.MatrixFresh(ctx, inv); err != nil {
		t.Fatalf("MatrixFresh: %v", err)
	}
	// prod-eu: DescribeServices and DescribeTaskDefinition; prod-us fails at DescribeServices.
	if got := w.ecsCalls.Load(); got != before+3 {
		t.Errorf("ECS calls after a fresh read = %d, want %d", got, before+3)
	}
	if ttl := ttlMillis(t, client, keyOf("prod-eu")); ttl <= 100_000 {
		t.Errorf("TTL after a fresh read = %d ms, want it written again at 120000", ttl)
	}
}

func TestDatabaseFactsAreNotCached(t *testing.T) {
	ctx := context.Background()
	f := newDBFixture(t)
	client := openTestValkey(t)
	prefix := fmt.Sprintf("running-test-%d:", time.Now().UnixNano())
	t.Cleanup(func() {
		keys, _ := client.Do(ctx, client.B().Keys().Pattern(prefix+"*").Build()).AsStrSlice()
		for _, key := range keys {
			client.Do(ctx, client.B().Del().Key(key).Build())
		}
	})
	w := newWorld()
	w.run(acctEU, "main", "api", repoURI(acctEU, "api")+":1.0.0", nil)
	inv := inventoryOf([]spec.Environment{envEU}, serviceSpec("api", "semver", envEU))
	svc := newService(t, w, Config{DB: f.pool, Valkey: client, KeyPrefix: prefix})

	m, err := svc.Matrix(ctx, inv)
	if err != nil || cellOf(t, m, "api", "prod-eu").LastDeployFailed {
		t.Fatalf("first Matrix = %+v, %v; want no failed deploy", m.Rows, err)
	}
	runID, deploy, _ := f.run("prod-eu")
	f.leg(runID, &deploy, "step", "api", "failed")
	calls := w.ecsCalls.Load()
	m, err = svc.Matrix(ctx, inv)
	if err != nil {
		t.Fatalf("second Matrix: %v", err)
	}
	if w.ecsCalls.Load() != calls {
		t.Fatalf("the second read went to AWS; this test needs it served from the cache")
	}
	if !cellOf(t, m, "api", "prod-eu").LastDeployFailed {
		t.Errorf("a cached read hid a deploy that failed after it was cached")
	}
}
