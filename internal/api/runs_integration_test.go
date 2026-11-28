//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/testutil/awsseed"
)

const (
	devEnvironment  = "staging"
	prodEnvironment = "prod-eu"
)

// runFixture adds to the harness an active inventory (a dev and a prod
// environment, services s1..s4 in both) and a runnable job reading it.
type runFixture struct {
	*harness
	folderID, jobID, otherJobID int64
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	ctx := context.Background()
	h := newHarness(t)
	inventory := spec.Inventory{
		Environments: []spec.Environment{
			{Name: devEnvironment, Label: "dev", AWSAccount: "111122223333"},
			{Name: prodEnvironment, Label: "prod", AWSAccount: "444455556666"},
		},
		Services: map[string]spec.Service{},
	}
	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("s%d", i)
		envs := map[string]spec.ServiceEnv{}
		for _, env := range []string{devEnvironment, prodEnvironment} {
			envs[env] = spec.ServiceEnv{
				ECSService: fmt.Sprintf("arn:aws:ecs:eu-west-1:1:service/c/%s-%s", name, env),
				Tomls:      []spec.Toml{{Name: "app", Destination: fmt.Sprintf("s3://cfg/%s/%s.toml", env, name)}},
			}
		}
		inventory.Services[name] = spec.Service{Name: name, Environments: envs}
	}
	awsseed.Connect(t, h.pool, "111122223333", "444455556666") // runs need their accounts connected
	if _, err := store.ActivateSnapshot(ctx, h.pool, "inv1", mustMarshal(t, inventory), nil, time.Now()); err != nil {
		t.Fatalf("ActivateSnapshot: %v", err)
	}
	folder, err := store.CreateFolder(ctx, h.pool, "services")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	f := &runFixture{harness: h, folderID: folder.ID}
	f.jobID = f.addJob("release", "release.yaml")
	f.otherJobID = f.addJob("hotfix", "hotfix.yaml")
	return f
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func (f *runFixture) addJob(id, path string) int64 {
	f.t.Helper()
	ctx := context.Background()
	job := spec.Job{
		ID: id, Title: id, Labels: []string{"dev", "prod"},
		Groups: []spec.Group{{ID: "g", Services: []string{"s1", "s2", "s3", "s4"}}},
		Steps: []spec.Step{
			{Name: "config", Use: "copy-config", Group: "g"},
			{Name: "deploy", Use: "ecs-deploy", Group: "g", Needs: []spec.Need{{Step: "config", Scope: "service"}}},
		},
	}
	source, err := store.CreateJobSource(ctx, f.pool, store.JobSource{FolderID: f.folderID, Repo: "acme/jobs", Branch: "main", Path: path})
	if err != nil {
		f.t.Fatalf("CreateJobSource: %v", err)
	}
	if err := store.SaveJobGood(ctx, f.pool, source.ID, mustMarshal(f.t, job), "", job.ID, job.Title, "jc1", time.Now()); err != nil {
		f.t.Fatalf("SaveJobGood: %v", err)
	}
	if err := store.SaveJobProblems(ctx, f.pool, source.ID, nil, "jc1", true, time.Now()); err != nil {
		f.t.Fatalf("SaveJobProblems: %v", err)
	}
	return source.ID
}

func selectionBody(environment string, start bool, services ...string) map[string]any {
	chosen := map[string]string{}
	for _, name := range services {
		chosen[name] = ""
	}
	return map[string]any{
		"environment": environment, "start": start,
		"selection": map[string]any{"g": map[string]any{"version": "1.0", "services": chosen}},
	}
}

// newRun creates a run through the API as the admin and returns its id.
func (f *runFixture) newRun(jobID int64, environment string, services ...string) int64 {
	f.t.Helper()
	body := f.mustDo("POST", fmt.Sprintf("/api/v1/jobs/%d/runs", jobID), selectionBody(environment, false, services...), http.StatusCreated)
	return int64(body["id"].(float64))
}

func (f *runFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// userWith makes a user holding the given grants (action, label, environment).
func (f *runFixture) userWith(name string, grants ...store.Grant) string {
	f.t.Helper()
	ctx := context.Background()
	u := f.newUser(name)
	group, err := store.CreateGroup(ctx, f.pool, "g-"+uniqueEmail(f.t, name), false)
	if err != nil {
		f.t.Fatalf("CreateGroup: %v", err)
	}
	if err := store.SetManualMembership(ctx, f.pool, u.ID, group.ID, true); err != nil {
		f.t.Fatalf("SetManualMembership: %v", err)
	}
	for _, g := range grants {
		g.GroupID = group.ID
		if _, err := store.CreateGrant(ctx, f.pool, g); err != nil {
			f.t.Fatalf("CreateGrant: %v", err)
		}
	}
	token, _ := f.newToken(u.ID)
	return token
}

func (f *runFixture) get(token, path string) (int, map[string]any) {
	f.t.Helper()
	code, raw := f.doRaw("GET", path, token, nil)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return code, decoded
}

func (f *runFixture) post(token, path string, body any) (int, map[string]any) {
	f.t.Helper()
	code, raw := f.doRaw("POST", path, token, body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return code, decoded
}

func runPath(id int64, rest string) string { return fmt.Sprintf("/api/v1/runs/%d%s", id, rest) }

func TestCreateRunValidatesSelectionWith422(t *testing.T) {
	f := newRunFixture(t)
	path := fmt.Sprintf("/api/v1/jobs/%d/runs", f.jobID)

	for name, body := range map[string]map[string]any{
		"unknown service":     selectionBody(devEnvironment, false, "nope"),
		"empty selection":     selectionBody(devEnvironment, false),
		"unknown environment": selectionBody("nowhere", false, "s1"),
	} {
		code, got := f.post(f.adminToken, path, body)
		if code != http.StatusUnprocessableEntity || errorCode(t, got) != "invalid" {
			t.Errorf("%s: %d %v, want 422 invalid", name, code, got)
			continue
		}
		problems, _ := got["error"].(map[string]any)["problems"].([]any)
		if len(problems) == 0 {
			t.Errorf("%s: the 422 lists no problems: %v", name, got)
		}
	}
	runs, err := store.ListRuns(context.Background(), f.pool, store.RunFilter{})
	if err != nil || len(runs) != 0 {
		t.Fatalf("after refused selections: %d runs (%v), want none", len(runs), err)
	}

	// Malformed bodies and unknown jobs are not selection problems.
	if code, _ := f.post(f.adminToken, path, "{not json"); code != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", code)
	}
	if code, _ := f.post(f.adminToken, path, map[string]any{"environment": devEnvironment, "bogus": 1}); code != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", code)
	}
	if code, _ := f.post(f.adminToken, "/api/v1/jobs/999999/runs", selectionBody(devEnvironment, false, "s1")); code != http.StatusNotFound {
		t.Errorf("unknown job = %d, want 404", code)
	}

	got := f.mustDo("POST", path, selectionBody(devEnvironment, false, "s1", "s2"), http.StatusCreated)
	if got["state"] != "resolving" || got["environment"] != devEnvironment || got["job_id"] != "release" || got["via"] != "cli" {
		t.Errorf("created run = %v, want resolving, staging, release, via cli (a token)", got)
	}
	steps, _ := got["steps"].([]any)
	legs, _ := got["legs"].([]any)
	if len(steps) != 2 || len(legs) != 5 { // two steps; one resolve leg and 2 services x 2 steps
		t.Errorf("created run has %d steps and %d legs, want 2 and 5", len(steps), len(legs))
	}
	// The engine audits the action in its own transaction: exactly one entry.
	n := 0
	for _, a := range f.auditActions() {
		if a == "run.create" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("run.create audit entries = %d, want exactly 1", n)
	}
}

func TestCreateRunNeedsPermission(t *testing.T) {
	f := newRunFixture(t)
	path := fmt.Sprintf("/api/v1/jobs/%d/runs", f.jobID)
	body := selectionBody(devEnvironment, false, "s1")

	if code, _ := f.post("", path, body); code != http.StatusUnauthorized {
		t.Errorf("without credentials = %d, want 401", code)
	}
	nobody := f.userWith("nobody")
	if code, got := f.post(nobody, path, body); code != http.StatusForbidden || errorCode(t, got) != "forbidden" {
		t.Errorf("without a grant = %d %v, want 403", code, got)
	}
	prodOnly := f.userWith("prodonly", store.Grant{Action: "run", Label: "prod"})
	if code, _ := f.post(prodOnly, path, body); code != http.StatusForbidden {
		t.Errorf("run scoped to prod, target dev = %d, want 403", code)
	}
	if code, _ := f.post(prodOnly, path, selectionBody(prodEnvironment, false, "s1")); code != http.StatusCreated {
		t.Errorf("run scoped to prod, target prod = %d, want 201", code)
	}
	envScoped := f.userWith("envscoped", store.Grant{Action: "run", Environment: devEnvironment})
	if code, _ := f.post(envScoped, path, body); code != http.StatusCreated {
		t.Errorf("run scoped to the environment = %d, want 201", code)
	}
	// A cookie request must come from the public address.
	user := f.newUser("cookie")
	f.grant(user.ID, "run")
	sid := f.newSession(user.ID)
	rec := f.send(sent{method: "POST", path: path, body: body, sessionID: sid, origin: "https://evil.test"})
	f.expect(rec, http.StatusForbidden, "forbidden")
	rec = f.send(sent{method: "POST", path: path, body: body, sessionID: sid, origin: publicURL})
	f.expect(rec, http.StatusCreated, "")
}

func TestGetRunIncludesFailureSummary(t *testing.T) {
	f := newRunFixture(t)
	id := f.newRun(f.jobID, devEnvironment, "s1", "s2", "s3")
	f.exec(`UPDATE legs SET state = 'succeeded' WHERE run_id = $1 AND kind = 'resolve'`, id)
	f.exec(`UPDATE legs SET state = 'succeeded' WHERE run_id = $1 AND service = 's1'`, id)
	f.exec(`UPDATE legs SET state = 'failed', attempt = 1, failure_short = 'task failed', failure_detail = 'ECS task stopped: OutOfMemory'
	        WHERE run_id = $1 AND service = 's2' AND step_id = (SELECT id FROM run_steps WHERE run_id = $1 AND name = 'config')`, id)
	f.exec(`UPDATE legs SET state = 'skipped' WHERE run_id = $1 AND service = 's2' AND state = 'pending'`, id)
	f.exec(`UPDATE legs SET state = 'unknown', failure_short = 'lease lost' WHERE run_id = $1 AND service = 's3' AND step_id = (SELECT id FROM run_steps WHERE run_id = $1 AND name = 'config')`, id)
	f.exec(`UPDATE runs SET state = 'failed', end_reason = 'one service failed' WHERE id = $1`, id)

	got := f.mustDo("GET", runPath(id, ""), nil, http.StatusOK)
	if got["state"] != "failed" || got["end_reason"] != "one service failed" || got["wait_reason"] != "" {
		t.Errorf("state %v end_reason %v wait_reason %v; want failed, the end reason, and no wait reason", got["state"], got["end_reason"], got["wait_reason"])
	}
	summary := got["summary"].(map[string]any)
	counts := summary["counts"].(map[string]any)
	if counts["succeeded"] != float64(3) || counts["failed"] != float64(1) || counts["unknown"] != float64(1) || counts["skipped"] != float64(1) {
		t.Errorf("counts = %v, want succeeded 3 (resolve and both s1 legs), failed 1, unknown 1, skipped 1", counts)
	}
	for _, state := range []string{"pending", "queued", "claimed", "running", "cancelled"} {
		if _, ok := counts[state]; !ok {
			t.Errorf("counts lacks %s, so a zero count would be missing for the web application", state)
		}
	}
	failures := summary["failures"].([]any)
	if len(failures) != 2 {
		t.Fatalf("failures = %v, want the failed and the unknown leg", failures)
	}
	first := failures[0].(map[string]any)
	if first["service"] != "s2" || first["step"] != "config" || first["short"] != "task failed" || first["detail"] != "ECS task stopped: OutOfMemory" || first["state"] != "failed" {
		t.Errorf("first failure = %v", first)
	}
	if got["steps"] == nil || got["legs"] == nil || got["locks"] == nil {
		t.Errorf("steps, legs and locks must be lists, got %v %v %v", got["steps"], got["legs"], got["locks"])
	}
	if leg := got["legs"].([]any)[1].(map[string]any); leg["step"] == "" || leg["version"] != "1.0" {
		t.Errorf("a step leg shows %v, want its step name and version", leg)
	}

	// A waiting run shows its wait reason, never as an end reason; held locks are listed.
	waiting := f.newRun(f.jobID, devEnvironment, "s1")
	f.exec(`UPDATE runs SET state = 'queued', wait_reason = 'waiting for run #3' WHERE id = $1`, waiting)
	f.exec(`INSERT INTO run_locks (resource, run_id, acquired_at) VALUES ('arn:one', $1, now()), ('arn:two', $1, now())`, waiting)
	got = f.mustDo("GET", runPath(waiting, ""), nil, http.StatusOK)
	if got["wait_reason"] != "waiting for run #3" || got["end_reason"] != "" {
		t.Errorf("wait_reason %v end_reason %v", got["wait_reason"], got["end_reason"])
	}
	if locks := got["locks"].([]any); len(locks) != 2 || locks[0] != "arn:one" {
		t.Errorf("locks = %v, want the two held resources", locks)
	}
	// Lists are [] and never null, also on a run with no failures and no locks.
	quiet := f.newRun(f.jobID, devEnvironment, "s1")
	_, raw := f.doRaw("GET", runPath(quiet, ""), f.adminToken, nil)
	if found := nullSlices(reflect.TypeFor[runDetailView](), raw, ""); len(found) > 0 {
		t.Errorf("run detail has null where a list belongs: %v", found)
	}
	if code, _ := f.get(f.adminToken, runPath(999999, "")); code != http.StatusNotFound {
		t.Errorf("unknown run = %d, want 404", code)
	}
	if code, _ := f.get(f.adminToken, "/api/v1/runs/abc"); code != http.StatusNotFound {
		t.Errorf("non-numeric run = %d, want 404", code)
	}
}

func TestListRunsFilters(t *testing.T) {
	f := newRunFixture(t)
	a := f.newRun(f.jobID, devEnvironment, "s1")
	b := f.newRun(f.jobID, prodEnvironment, "s1")
	c := f.newRun(f.otherJobID, devEnvironment, "s2")
	d := f.newRun(f.otherJobID, devEnvironment, "s2")
	f.exec(`UPDATE runs SET state = 'planned' WHERE id IN ($1, $2)`, a, b)
	f.exec(`UPDATE runs SET state = 'succeeded' WHERE id = $1`, c)

	ids := func(query string) []int64 {
		t.Helper()
		code, got := f.get(f.adminToken, "/api/v1/runs"+query)
		if code != http.StatusOK {
			t.Fatalf("GET /runs%s = %d %v", query, code, got)
		}
		out := []int64{}
		for _, r := range got["runs"].([]any) {
			out = append(out, int64(r.(map[string]any)["id"].(float64)))
		}
		return out
	}
	same := func(what string, got []int64, want ...int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}
	same("all, newest first", ids(""), d, c, b, a)
	same("job", ids(fmt.Sprintf("?job=%d", f.jobID)), b, a)
	same("environment", ids("?environment="+prodEnvironment), b)
	same("one state", ids("?state=planned"), b, a)
	same("repeated state", ids("?state=planned&state=succeeded"), c, b, a)
	same("q on the job id", ids("?q=hotfix"), d, c)
	same("q on the run id", ids("?q="+url.QueryEscape(fmt.Sprint(a))), a)
	same("limit", ids("?limit=2"), d, c)
	same("before", ids(fmt.Sprintf("?before=%d", c)), b, a)
	if got := ids("?limit=1000"); len(got) != 4 {
		t.Errorf("limit above the maximum = %v, want it clamped, not refused", got)
	}
	same("nothing matches", ids("?q=zzz"))
	_, rawList := f.doRaw("GET", "/api/v1/runs", f.adminToken, nil)
	if found := nullSlices(reflect.TypeFor[struct {
		Runs []runRowView `json:"runs"`
	}](), rawList, ""); len(found) > 0 {
		t.Errorf("run list has null where a list belongs: %v", found)
	}

	_, got := f.get(f.adminToken, "/api/v1/runs?limit=2")
	if next, _ := got["next_before"].(float64); int64(next) != c {
		t.Errorf("next_before = %v, want %d (the last run of a full page)", got["next_before"], c)
	}
	_, got = f.get(f.adminToken, "/api/v1/runs?limit=10")
	if got["next_before"] != nil {
		t.Errorf("next_before = %v on the last page, want null", got["next_before"])
	}
	_, empty := f.get(f.adminToken, "/api/v1/runs?q=zzz")
	if list, ok := empty["runs"].([]any); !ok || len(list) != 0 {
		t.Errorf("runs = %v, want []", empty["runs"])
	}
	for _, bad := range []string{"?limit=abc", "?limit=-1", "?before=x", "?job=x", "?state=nonsense"} {
		if code, _ := f.get(f.adminToken, "/api/v1/runs"+bad); code != http.StatusBadRequest {
			t.Errorf("GET /runs%s = %d, want 400", bad, code)
		}
	}
	// View is open to everyone who is signed in.
	if code, _ := f.get(f.userWith("viewer"), "/api/v1/runs"); code != http.StatusOK {
		t.Errorf("a plain user listing runs = %d, want 200", code)
	}
}

func (f *runFixture) addLines(runID int64, lines ...store.LogLine) {
	f.t.Helper()
	for i := range lines {
		lines[i].RunID = runID
		if lines[i].At.IsZero() {
			lines[i].At = time.Now()
		}
		if lines[i].Level == "" {
			lines[i].Level = "info"
		}
	}
	if err := store.InsertLogLines(context.Background(), f.pool, lines); err != nil {
		f.t.Fatalf("InsertLogLines: %v", err)
	}
}

func (f *runFixture) legIDs(runID int64) map[string]int64 {
	f.t.Helper()
	legs, err := store.LegsOfRun(context.Background(), f.pool, runID)
	if err != nil {
		f.t.Fatalf("LegsOfRun: %v", err)
	}
	out := map[string]int64{}
	for _, l := range legs {
		out[fmt.Sprintf("%s/%d", l.Service, l.ID)] = l.ID
		if l.Kind != "step" {
			out[l.Kind] = l.ID
		}
	}
	return out
}

func TestLogEndpointFiltersPagesAndNeedsViewLogs(t *testing.T) {
	f := newRunFixture(t)
	id := f.newRun(f.jobID, devEnvironment, "s1", "s2")
	legs, err := store.LegsOfRun(context.Background(), f.pool, id)
	if err != nil {
		t.Fatal(err)
	}
	var s1Config, s2Deploy int64
	steps, _ := store.StepsOfRun(context.Background(), f.pool, id)
	stepName := map[int64]string{}
	for _, s := range steps {
		stepName[s.ID] = s.Name
	}
	for _, l := range legs {
		if l.Service == "s1" && stepName[*l.StepID] == "config" {
			s1Config = l.ID
		}
		if l.Service == "s2" && stepName[*l.StepID] == "deploy" {
			s2Deploy = l.ID
		}
	}
	base := time.Now().Add(time.Minute)
	f.addLines(id,
		store.LogLine{LegID: s1Config, Seq: 1, At: base.Add(1 * time.Second), Text: "copying app.toml"},
		store.LogLine{LegID: s1Config, Seq: 2, At: base.Add(2 * time.Second), Level: "ok", Text: "copied"},
		store.LogLine{LegID: s2Deploy, Seq: 1, At: base.Add(3 * time.Second), Text: "registering task definition"},
		store.LogLine{LegID: s2Deploy, Seq: 3, At: base.Add(4 * time.Second), Level: "error", Text: "task FAILED"}, // seq 2 is missing
	)

	read := func(token, query string) (int, map[string]any) {
		return f.get(token, runPath(id, "/log"+query))
	}
	texts := func(got map[string]any) []string {
		out := []string{}
		for _, l := range got["lines"].([]any) {
			out = append(out, l.(map[string]any)["text"].(string))
		}
		return out
	}
	code, got := read(f.adminToken, "")
	if code != http.StatusOK || len(texts(got)) != 5 { // the run's own creation line plus four
		t.Fatalf("all lines = %d %v", code, got)
	}
	first := got["lines"].([]any)[0].(map[string]any)
	for _, key := range []string{"leg_id", "seq", "at", "level", "text"} {
		if _, ok := first[key]; !ok {
			t.Errorf("a line lacks %q: %v", key, first)
		}
	}
	gaps := got["gaps"].([]any)
	if len(gaps) != 1 || gaps[0].(map[string]any)["leg_id"] != float64(s2Deploy) || gaps[0].(map[string]any)["from_seq"] != float64(2) {
		t.Errorf("gaps = %v, want seq 2 of the s2 deploy leg", gaps)
	}

	if _, got := read(f.adminToken, "?service=s2"); fmt.Sprint(texts(got)) != "[registering task definition task FAILED]" {
		t.Errorf("service filter = %v", texts(got))
	}
	if _, got := read(f.adminToken, "?step=config"); fmt.Sprint(texts(got)) != "[copying app.toml copied]" {
		t.Errorf("step filter = %v", texts(got))
	}
	if _, got := read(f.adminToken, "?level=error"); fmt.Sprint(texts(got)) != "[task FAILED]" {
		t.Errorf("level filter = %v", texts(got))
	}
	if _, got := read(f.adminToken, "?q=TASK"); len(texts(got)) != 2 {
		t.Errorf("q filter = %v, want two case-insensitive matches", texts(got))
	}

	// Paging: two lines at a time, following next, covers everything once.
	var all []string
	cursor := ""
	for range 10 {
		_, page := read(f.adminToken, "?limit=2&after="+url.QueryEscape(cursor))
		lines := texts(page)
		if len(lines) == 0 {
			break
		}
		all = append(all, lines...)
		cursor, _ = page["next"].(string)
	}
	if len(all) != 5 {
		t.Errorf("paged lines = %v, want all 5 exactly once", all)
	}
	if _, page := read(f.adminToken, "?after="+url.QueryEscape(cursor)); len(texts(page)) != 0 || page["next"] != cursor {
		t.Errorf("after the last line: %v, want no lines and the same cursor", page)
	}
	for _, bad := range []string{"?after=garbage!", "?limit=0", "?limit=x", "?level=loud", "?step=nonexistent"} {
		if code, _ := read(f.adminToken, bad); code != http.StatusBadRequest {
			t.Errorf("log%s = %d, want 400", bad, code)
		}
	}
	_, rawPage := f.doRaw("GET", runPath(id, "/log?q=nothing-matches"), f.adminToken, nil)
	if found := nullSlices(reflect.TypeFor[logPageView](), rawPage, ""); len(found) > 0 {
		t.Errorf("an empty log page has null where a list belongs: %v", found)
	}
	if code, got := read(f.adminToken, "?limit=5000"); code != http.StatusOK || len(texts(got)) != 5 {
		t.Errorf("limit above the maximum = %d, want it clamped to 1000", code)
	}

	// view_logs is needed on the run's target.
	if code, _ := read(f.userWith("noviewlogs", store.Grant{Action: "run"}), ""); code != http.StatusForbidden {
		t.Errorf("without view_logs = %d, want 403", code)
	}
	if code, _ := read(f.userWith("otherlabel", store.Grant{Action: "view_logs", Label: "prod"}), ""); code != http.StatusForbidden {
		t.Errorf("view_logs on prod, run is dev = %d, want 403", code)
	}
	if code, _ := read(f.userWith("rightlabel", store.Grant{Action: "view_logs", Label: "dev"}), ""); code != http.StatusOK {
		t.Errorf("view_logs on dev = %d, want 200", code)
	}
	if code, _ := f.get(f.adminToken, runPath(999999, "/log")); code != http.StatusNotFound {
		t.Errorf("unknown run = %d, want 404", code)
	}
}

func TestCancelReconcileCheckAgainReleaseLocksPermissionsAndStates(t *testing.T) {
	f := newRunFixture(t)
	runner := f.userWith("runner", store.Grant{Action: "run"})
	canceller := f.userWith("canceller", store.Grant{Action: "cancel"})
	restorer := f.userWith("restorer", store.Grant{Action: "restore"})
	prodRestorer := f.userWith("prodrestorer", store.Grant{Action: "restore", Label: "prod"})
	plain := f.userWith("plain")
	accessManager := f.userWith("accessmanager", store.Grant{Action: "manage_access"})

	// settle puts a fresh run into needs_reconciliation with one unknown leg
	// (the s1 config leg) and every other leg settled.
	settle := func() (runID, unknownLeg int64) {
		runID = f.newRun(f.jobID, devEnvironment, "s1")
		f.exec(`UPDATE legs SET state = 'skipped' WHERE run_id = $1`, runID)
		f.exec(`UPDATE legs SET state = 'succeeded' WHERE run_id = $1 AND kind = 'resolve'`, runID)
		f.exec(`UPDATE legs SET state = 'unknown' WHERE run_id = $1 AND kind = 'step' AND step_id = (SELECT id FROM run_steps WHERE run_id = $1 AND name = 'config')`, runID)
		f.exec(`UPDATE runs SET state = 'needs_reconciliation' WHERE id = $1`, runID)
		f.exec(`INSERT INTO run_locks (resource, run_id, acquired_at) VALUES ($1, $2, now())`, fmt.Sprintf("lock-%d", runID), runID)
		legs, _ := store.LegsOfRun(context.Background(), f.pool, runID)
		for _, l := range legs {
			if l.State == "unknown" {
				unknownLeg = l.ID
			}
		}
		return runID, unknownLeg
	}
	// otherRunsLeg settles two runs and returns the first with the unknown leg of the second.
	otherRunsLeg := func() (int64, int64) {
		first, _ := settle()
		_, foreign := settle()
		return first, foreign
	}
	planned := func() (int64, int64) {
		id := f.newRun(f.jobID, devEnvironment, "s1")
		f.exec(`UPDATE runs SET state = 'planned' WHERE id = $1`, id)
		return id, 0
	}
	fresh := func() (int64, int64) { return f.newRun(f.jobID, devEnvironment, "s1"), 0 }
	final := func() (int64, int64) {
		id, _ := fresh()
		f.exec(`UPDATE runs SET state = 'failed', end_reason = 'done' WHERE id = $1`, id)
		return id, 0
	}

	cases := []struct {
		name  string
		token string
		setup func() (int64, int64)
		path  func(run, leg int64) string
		body  any
		want  int
		check func(t *testing.T, run, leg int64)
		audit string // the audit action the engine writes
	}{
		{"start planned as runner", runner, planned, func(r, _ int64) string { return runPath(r, "/start") }, nil, 200, func(t *testing.T, r, _ int64) { f.wantRunState(t, r, "running") }, "run.start"},
		{"start without run", canceller, planned, func(r, _ int64) string { return runPath(r, "/start") }, nil, 403, func(t *testing.T, r, _ int64) { f.wantRunState(t, r, "planned") }, ""},
		{"start a run that is not planned", runner, fresh, func(r, _ int64) string { return runPath(r, "/start") }, nil, 409, nil, ""},
		{"start unknown run", runner, func() (int64, int64) { return 999999, 0 }, func(r, _ int64) string { return runPath(r, "/start") }, nil, 404, nil, ""},
		{"cancel as canceller", canceller, fresh, func(r, _ int64) string { return runPath(r, "/cancel") }, nil, 200, func(t *testing.T, r, _ int64) { f.wantRunState(t, r, "cancelled") }, "run.cancel"},
		{"cancel without cancel", runner, fresh, func(r, _ int64) string { return runPath(r, "/cancel") }, nil, 403, func(t *testing.T, r, _ int64) { f.wantRunState(t, r, "resolving") }, ""},
		{"cancel a final run", canceller, final, func(r, _ int64) string { return runPath(r, "/cancel") }, nil, 409, nil, ""},
		{"reconcile as restorer", restorer, settle, func(r, l int64) string { return runPath(r, fmt.Sprintf("/legs/%d/reconcile", l)) }, map[string]string{"outcome": "succeeded"}, 200,
			func(t *testing.T, r, l int64) { f.wantLegState(t, l, "succeeded") }, "run.reconcile"},
		{"reconcile as failed", restorer, settle, func(r, l int64) string { return runPath(r, fmt.Sprintf("/legs/%d/reconcile", l)) }, map[string]string{"outcome": "failed"}, 200,
			func(t *testing.T, r, l int64) { f.wantLegState(t, l, "failed") }, "run.reconcile"},
		{"reconcile with a bad outcome", restorer, settle, func(r, l int64) string { return runPath(r, fmt.Sprintf("/legs/%d/reconcile", l)) }, map[string]string{"outcome": "maybe"}, 400, nil, ""},
		{"reconcile without a body", restorer, settle, func(r, l int64) string { return runPath(r, fmt.Sprintf("/legs/%d/reconcile", l)) }, nil, 400, nil, ""},
		{"reconcile without restore", runner, settle, func(r, l int64) string { return runPath(r, fmt.Sprintf("/legs/%d/reconcile", l)) }, map[string]string{"outcome": "succeeded"}, 403,
			func(t *testing.T, r, l int64) { f.wantLegState(t, l, "unknown") }, ""},
		{"reconcile with restore on another label", prodRestorer, settle, func(r, l int64) string { return runPath(r, fmt.Sprintf("/legs/%d/reconcile", l)) }, map[string]string{"outcome": "succeeded"}, 403, nil, ""},
		{"reconcile a run not in reconciliation", restorer, fresh, func(r, _ int64) string { return runPath(r, "/legs/1/reconcile") }, map[string]string{"outcome": "succeeded"}, 409, nil, ""},
		{"reconcile a leg of another run", restorer, settle, func(r, _ int64) string { return runPath(r, "/legs/999999/reconcile") }, map[string]string{"outcome": "succeeded"}, 404, nil, ""},
		{"reconcile a real leg of another run", restorer, otherRunsLeg, func(r, l int64) string { return runPath(r, fmt.Sprintf("/legs/%d/reconcile", l)) }, map[string]string{"outcome": "succeeded"}, 404,
			func(t *testing.T, _, l int64) { f.wantLegState(t, l, "unknown") }, ""},
		{"check again as restorer", restorer, settle, func(r, l int64) string { return runPath(r, fmt.Sprintf("/legs/%d/check-again", l)) }, nil, 200,
			func(t *testing.T, r, l int64) { f.wantLegState(t, l, "queued") }, "run.check_again"},
		{"check again without restore", plain, settle, func(r, l int64) string { return runPath(r, fmt.Sprintf("/legs/%d/check-again", l)) }, nil, 403, nil, ""},
		{"check again a non-numeric leg", restorer, settle, func(r, _ int64) string { return runPath(r, "/legs/abc/check-again") }, nil, 404, nil, ""},
		{"release locks as admin", f.adminToken, settle, func(r, _ int64) string { return fmt.Sprintf("/api/v1/admin/runs/%d/release-locks", r) }, nil, 200,
			func(t *testing.T, r, _ int64) {
				f.wantRunState(t, r, "failed")
				if locks, _ := store.LocksOfRun(context.Background(), f.pool, r); len(locks) != 0 {
					t.Errorf("locks still held: %v", locks)
				}
			}, "run.release_locks"},
		{"release locks as a non-admin", restorer, settle, func(r, _ int64) string { return fmt.Sprintf("/api/v1/admin/runs/%d/release-locks", r) }, nil, 403, nil, ""},
		{"release locks as a manage_access non-admin", accessManager, settle, func(r, _ int64) string { return fmt.Sprintf("/api/v1/admin/runs/%d/release-locks", r) }, nil, 403,
			func(t *testing.T, r, _ int64) { f.wantRunState(t, r, "needs_reconciliation") }, ""},
		{"release locks of a run not in reconciliation", f.adminToken, fresh, func(r, _ int64) string { return fmt.Sprintf("/api/v1/admin/runs/%d/release-locks", r) }, nil, 409, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run, leg := c.setup()
			before := len(f.auditEntries())
			code, got := f.post(c.token, c.path(run, leg), c.body)
			if code != c.want {
				t.Fatalf("status = %d %v, want %d", code, got, c.want)
			}
			if c.want == 200 && (got["id"] != float64(run) || got["state"] == nil) {
				t.Errorf("a successful action answers with the run, got %v", got)
			}
			if c.check != nil {
				c.check(t, run, leg)
			}
			entries := f.auditEntries()
			added := len(entries) - before
			switch {
			case c.audit != "" && (added != 1 || entries[0].Action != c.audit):
				t.Errorf("audit entries added = %d (latest %q), want exactly one %q (the engine's own, not doubled)", added, entries[0].Action, c.audit)
			case c.audit == "" && added != 0:
				t.Errorf("a refused action wrote %d audit entries", added)
			}
		})
	}
}

func (f *runFixture) wantRunState(t *testing.T, id int64, want string) {
	t.Helper()
	run, err := store.GetRun(context.Background(), f.pool, id)
	if err != nil || run.State != want {
		t.Errorf("run %d state = %q (%v), want %q", id, run.State, err, want)
	}
}

func (f *runFixture) wantLegState(t *testing.T, id int64, want string) {
	t.Helper()
	leg, err := store.GetLeg(context.Background(), f.pool, id)
	if err != nil || leg.State != want {
		t.Errorf("leg %d state = %q (%v), want %q", id, leg.State, err, want)
	}
}

func TestListRunsCarriesServicesSummaryAndNames(t *testing.T) {
	f := newRunFixture(t)
	f.exec(`UPDATE job_sources SET title = 'Release backends' WHERE id = $1`, f.jobID)
	busy := f.newRun(f.jobID, devEnvironment, "s1", "s2", "s3")
	bare := f.newRun(f.otherJobID, devEnvironment, "s4")
	f.exec(`DELETE FROM legs WHERE run_id = $1 AND kind = 'step'`, bare)
	f.exec(`UPDATE users SET name = 'Ada Admin' WHERE id = (SELECT requested_by FROM runs WHERE id = $1)`, busy)
	f.exec(`UPDATE legs SET state = 'succeeded' WHERE run_id = $1 AND service = 's1'`, busy)
	f.exec(`UPDATE legs SET state = 'failed', failure_short = 'task failed', failure_detail = 'OutOfMemory'
	        WHERE run_id = $1 AND service = 's2' AND step_id = (SELECT id FROM run_steps WHERE run_id = $1 AND name = 'config')`, busy)
	f.exec(`UPDATE legs SET state = 'skipped' WHERE run_id = $1 AND service = 's2' AND state = 'pending'`, busy)
	f.exec(`UPDATE legs SET state = 'succeeded' WHERE run_id = $1 AND service = 's3' AND step_id = (SELECT id FROM run_steps WHERE run_id = $1 AND name = 'config')`, busy)

	got := f.mustDo("GET", "/api/v1/runs", nil, http.StatusOK)
	rows := map[int64]map[string]any{}
	for _, r := range got["runs"].([]any) {
		row := r.(map[string]any)
		rows[int64(row["id"].(float64))] = row
	}
	row := rows[busy]
	services := row["services"].([]any)
	want := []struct{ service, version, state string }{{"s1", "1.0", "succeeded"}, {"s2", "1.0", "failed"}, {"s3", "1.0", "running"}}
	if len(services) != len(want) {
		t.Fatalf("services = %v, want %v (step legs only, in order of first appearance)", services, want)
	}
	for i, w := range want {
		s := services[i].(map[string]any)
		if s["service"] != w.service || s["version"] != w.version || s["state"] != w.state {
			t.Errorf("services[%d] = %v, want %v", i, s, w)
		}
	}
	summary := row["summary"].(map[string]any)
	counts := summary["counts"].(map[string]any)
	if counts["failed"] != float64(1) || counts["succeeded"] != float64(3) || counts["skipped"] != float64(1) || counts["cancelled"] != float64(0) {
		t.Errorf("counts = %v, want failed 1, succeeded 3, skipped 1 and zeros for the rest", counts)
	}
	if failures := summary["failures"].([]any); len(failures) != 1 || failures[0].(map[string]any)["service"] != "s2" || failures[0].(map[string]any)["step"] != "config" {
		t.Errorf("failures = %v, want the one failed s2 config leg", summary["failures"])
	}
	if row["job_name"] != "Release backends" || row["requested_by_name"] != "Ada Admin" {
		t.Errorf("job_name %v requested_by_name %v", row["job_name"], row["requested_by_name"])
	}

	none := rows[bare]
	if list, ok := none["services"].([]any); !ok || len(list) != 0 {
		t.Errorf("a run with no step legs has services %v, want []", none["services"])
	}
	if none["summary"].(map[string]any)["failures"] == nil {
		t.Errorf("failures must be [] not null")
	}
	if name := none["requested_by_name"]; name == "" {
		t.Errorf("requested_by_name is empty, want the name or else the email")
	}
}

func TestRunDetailCarriesJobAndRequesterNames(t *testing.T) {
	f := newRunFixture(t)
	f.exec(`UPDATE job_sources SET title = 'Release backends' WHERE id = $1`, f.jobID)
	id := f.newRun(f.jobID, devEnvironment, "s1")
	f.exec(`UPDATE users SET name = '' WHERE id = (SELECT requested_by FROM runs WHERE id = $1)`, id)
	var email string
	if err := f.pool.QueryRow(context.Background(), `SELECT u.email FROM users u JOIN runs r ON r.requested_by = u.id WHERE r.id = $1`, id).Scan(&email); err != nil {
		t.Fatal(err)
	}
	got := f.mustDo("GET", runPath(id, ""), nil, http.StatusOK)
	if got["job_name"] != "Release backends" || got["requested_by_name"] != email {
		t.Errorf("job_name %v requested_by_name %v, want the job title and, with no name, the email %q", got["job_name"], got["requested_by_name"], email)
	}
	f.exec(`UPDATE users SET name = 'Ada Admin' WHERE email = $1`, email)
	if started := f.mustDo("POST", runPath(id, "/cancel"), nil, http.StatusOK); started["requested_by_name"] != "Ada Admin" || started["job_name"] != "Release backends" {
		t.Errorf("a run action answers job_name %v requested_by_name %v, want the same names", started["job_name"], started["requested_by_name"])
	}
}
