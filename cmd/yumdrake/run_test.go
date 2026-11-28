package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// ---- fixtures for the plan, run and watch flow ----

const flowRunID = 14

func manifestFixture(failedCheck bool) map[string]any {
	checks := []map[string]any{
		{"name": "service exists", "service": "billing", "status": "passed", "detail": ""},
		{"name": "image exists", "service": "billing", "status": "passed", "detail": ""},
		{"name": "write permission", "service": "billing", "status": "not_verified", "detail": "cannot be proven without writing"},
	}
	if failedCheck {
		checks = append(checks, map[string]any{"name": "image exists", "service": "orders", "status": "failed", "detail": "tag 1.4.2 not found in orders"})
	}
	return map[string]any{
		"id": "abc123", "run_id": flowRunID, "environment": "dev-01", "label": "dev", "aws_account": "111111111111",
		"inventory_commit": "i1", "job_commit": "j1",
		"services": []map[string]any{
			{"name": "billing", "group": "backends", "version": "1.4.2", "image_digest": "sha256:aaaa",
				"previous_task_definition": "arn:aws:ecs:eu-west-1:111111111111:task-definition/billing:30", "previous_version": "1.3.9",
				"config": []map[string]any{
					{"kind": "env", "name": "app.env", "branch": "dev", "fallback": true, "change": "changed",
						"keys": map[string]any{"added": []string{"NEW_FLAG"}, "removed": []string{"OLD_FLAG"}, "changed": []string{"DB_POOL"}}},
					{"kind": "toml", "name": "limits.toml", "change": "unchanged", "keys": nil},
				}},
		},
		"checks": checks,
	}
}

func stateBody(state, endReason string, legs ...map[string]any) map[string]any {
	failures := []map[string]any{}
	for _, l := range legs {
		if l["state"] == "failed" {
			failures = append(failures, map[string]any{"leg_id": l["id"], "service": l["service"], "step": l["step"], "state": "failed",
				"short": l["failure_short"], "detail": l["failure_detail"]})
		}
	}
	if legs == nil {
		legs = []map[string]any{}
	}
	return map[string]any{"id": flowRunID, "state": state, "wait_reason": "", "end_reason": endReason, "legs": legs,
		"summary": map[string]any{"counts": map[string]int{}, "failures": failures}}
}

func leg(id int, step, service, state, short string) map[string]any {
	return map[string]any{"id": id, "step": step, "kind": "step", "service": service, "version": "1.4.2", "state": state,
		"failure_short": short, "failure_detail": ""}
}

func frame(event, id string, body any) string {
	data, _ := json.Marshal(body)
	s := "event: " + event + "\n"
	if id != "" {
		s += "id: " + id + "\n"
	}
	return s + "data: " + string(data) + "\n\n"
}

// flow is the fake server for plan, run and watch.
type flow struct {
	*fakeAPI
	mu       sync.Mutex
	runState string // what GET /runs/14 answers
	polls    int
	// resolving is the number of GETs that answer "resolving" before runState.
	resolving int
	startedTo string // the state POST /start answers
}

// newFlow signs the harness in to a server that plans run 14 into planned.
func newFlow(t *testing.T) (*harness, *flow) {
	t.Helper()
	h, api := apiHarness(t)
	f := &flow{fakeAPI: api, runState: "planned", startedTo: "queued"}
	api.reply("GET /api/v1/jobs/{id}/versions", 200, versionsFixture())
	api.set("POST /api/v1/jobs/{id}/runs", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, 201, runDetailFixture(flowRunID, "resolving"))
	})
	api.set("GET /api/v1/runs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.polls++
		state := f.runState
		if f.polls <= f.resolving {
			state = "resolving"
		}
		f.mu.Unlock()
		writeTestJSON(w, 200, runDetailFixture(flowRunID, state))
	})
	api.reply("GET /api/v1/runs/{id}/plan", 200, manifestFixture(false))
	api.set("POST /api/v1/runs/{id}/start", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, 200, runDetailFixture(flowRunID, f.startedTo))
	})
	api.set("POST /api/v1/runs/{id}/cancel", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, 200, runDetailFixture(flowRunID, "cancelling"))
	})
	return h, f
}

// stream makes GET /runs/14/events send the frames and close.
func (f *flow) stream(frames ...string) {
	f.set("GET /api/v1/runs/{id}/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, fr := range frames {
			_, _ = fmt.Fprint(w, fr)
		}
	})
}

func (f *flow) started() bool { return len(f.requests("POST /api/v1/runs/14/start")) > 0 }

var planSelection = []string{"--set", "backends=1.4.2", "--set", "workers=2.0.0"}

func planArgs(command string, extra ...string) []string {
	args := append([]string{command, "roll-backends", "--env", "dev-01"}, planSelection...)
	return append(args, extra...)
}

// ---- plan ----

func TestPlanCreatesAPlannedRunAndPrintsIt(t *testing.T) {
	h, f := newFlow(t)
	code, stdout, stderr := h.run(planArgs("plan")...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	contains(t, "plan", stdout, "Run 14", "roll-backends", "dev-01", "billing", "1.3.9 -> 1.4.2", "app.env: changed",
		"added NEW_FLAG", "removed OLD_FLAG", "changed DB_POOL", `fallback branch "dev"`, "2 passed", "1 not verified", "0 failed", "yumdrake runs start 14")
	posts := f.requests("POST /api/v1/jobs/3/runs")
	if len(posts) != 1 {
		t.Fatalf("create requests = %d", len(posts))
	}
	var body struct {
		Environment string                    `json:"environment"`
		Selection   map[string]selectionGroup `json:"selection"`
		Start       bool                      `json:"start"`
	}
	if err := json.Unmarshal([]byte(posts[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Environment != "dev-01" || body.Start || body.Selection["backends"].Version != "1.4.2" || body.Selection["workers"].Version != "2.0.0" ||
		len(body.Selection["backends"].Services) != 2 {
		t.Errorf("body = %+v", body)
	}
	if f.started() {
		t.Error("plan started the run")
	}
	assertNoToken(t, stdout, stderr)
}

func TestPlanWaitsForTheResolveToFinish(t *testing.T) {
	h, f := newFlow(t)
	f.resolving = 2
	code, _, stderr := h.run(planArgs("plan")...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(h.waits) < 2 {
		t.Errorf("waits = %v, want at least 2 polls apart", h.waits)
	}
}

func TestPlanExitsOneWhenACheckFailed(t *testing.T) {
	h, f := newFlow(t)
	f.runState = "failed"
	f.reply("GET /api/v1/runs/{id}/plan", 200, manifestFixture(true))
	code, stdout, _ := h.run(planArgs("plan")...)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	contains(t, "plan", stdout, "FAILED", "image exists (orders): tag 1.4.2 not found in orders", "1 failed")
	if strings.Contains(stdout, "yumdrake runs start 14") {
		t.Error("offers to start a run that failed its checks")
	}
}

func TestPlanJSONIsOneDocument(t *testing.T) {
	h, _ := newFlow(t)
	code, stdout, _ := h.run(append(planArgs("plan"), "-o", "json")...)
	var out struct {
		Run  map[string]any `json:"run"`
		Plan map[string]any `json:"plan"`
	}
	decodeOne(t, stdout, &out)
	if code != 0 || out.Run["id"] == nil || out.Plan["id"] != "abc123" {
		t.Errorf("exit %d out %+v", code, out)
	}
}

// Review Focus 5: a typo is caught before anything is created.
func TestPlanCatchesATypoBeforeCreatingAnything(t *testing.T) {
	base := []string{"plan", "roll-backends", "--env", "dev-01"}
	tests := []struct {
		name  string
		args  []string
		wants []string
	}{
		{"group in --set", append(base, "--set", "backend=1.4.2", "--set", "workers=2.0.0"), []string{`"backend"`, "backends, workers"}},
		{"service in --set", append(base, "--set", "backends=1.4.2", "--set", "workers=2.0.0", "--set", "backends.biling=1.0.0"), []string{`"biling"`, "billing, orders"}},
		{"name in --only", append(base, "--set", "backends=1.4.2", "--only", "ordres"), []string{`"ordres"`, "backends, workers", "billing, mailer, orders"}},
		{"missing version", append(base, "--set", "backends=1.4.2"), []string{`"workers"`, "mailer", "--set workers=VERSION"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, f := newFlow(t)
			code, _, stderr := h.run(tt.args...)
			if code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			contains(t, "stderr", stderr, tt.wants...)
			if n := len(f.requests("POST /api/v1/jobs/3/runs")); n != 0 {
				t.Errorf("%d runs were created", n)
			}
		})
	}
}

func TestPlanWithLatestUsesTheNewestCommonVersion(t *testing.T) {
	h, f := newFlow(t)
	f.reply("GET /api/v1/jobs/{id}/versions", 200, map[string]any{"groups": map[string]any{
		"backends": map[string]any{"common": []string{"1.4.2", "1.4.1"}, "services": map[string]any{}, "unavailable": map[string]string{}},
		"workers":  map[string]any{"common": []string{"2.0.1"}, "services": map[string]any{}, "unavailable": map[string]string{}},
	}})
	code, _, stderr := h.run("plan", "roll-backends", "--env", "dev-01", "--latest")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	posts := f.requests("POST /api/v1/jobs/3/runs")
	if len(posts) != 1 || !strings.Contains(posts[0].Body, `"1.4.2"`) || !strings.Contains(posts[0].Body, `"2.0.1"`) {
		t.Errorf("create requests = %+v", posts)
	}
	// A group with no tag in common cannot be resolved to a version.
	f.reply("GET /api/v1/jobs/{id}/versions", 200, map[string]any{"groups": map[string]any{
		"backends": map[string]any{"common": []string{}, "services": map[string]any{}, "unavailable": map[string]string{}},
		"workers":  map[string]any{"common": []string{"2.0.1"}, "services": map[string]any{}, "unavailable": map[string]string{}},
	}})
	code, _, stderr = h.run("plan", "roll-backends", "--env", "dev-01", "--latest")
	if code != 2 || !strings.Contains(stderr, "backends") {
		t.Errorf("no common tag: exit %d stderr %q", code, stderr)
	}
}

func TestPlanShowsTheServersProblemsFor422(t *testing.T) {
	h, f := newFlow(t)
	f.reply("POST /api/v1/jobs/{id}/runs", 422, map[string]any{"error": map[string]any{"code": "invalid", "message": "the selection cannot be planned",
		"problems": []map[string]any{{"file": "jobs/roll.yaml", "line": 0, "message": `environment "dev-9" does not exist`}}}})
	code, _, stderr := h.run("plan", "roll-backends", "--env", "dev-9", "--set", "backends=1", "--set", "workers=1")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	contains(t, "stderr", stderr, `environment "dev-9" does not exist`)
}

// ---- run ----

// Review Focus 1, first half: a pipeline without --yes does not hang.
func TestRunWithoutATerminalAndWithoutYesLeavesTheRunPlanned(t *testing.T) {
	h, f := newFlow(t)
	code, stdout, stderr := h.run(planArgs("run")...)
	if code != 5 {
		t.Errorf("exit = %d, want 5", code)
	}
	contains(t, "output", stdout+stderr, "Run 14", "planned", "yumdrake runs start 14", "/runs/14")
	if f.started() {
		t.Error("the run was started without --yes")
	}
}

func TestRunAsksOnATerminal(t *testing.T) {
	h, f := newFlow(t)
	h.stdinTerminal, h.stdin = true, "y\n"
	code, stdout, stderr := h.run(planArgs("run")...)
	if code != 0 || !f.started() {
		t.Errorf("answer y: exit %d started %v stderr %q", code, f.started(), stderr)
	}
	contains(t, "prompt", stderr, "Start this run? [y/N]")
	contains(t, "stdout", stdout, "Run 14 started")

	h2, f2 := newFlow(t)
	h2.stdinTerminal, h2.stdin = true, "\n"
	code, _, _ = h2.run(planArgs("run")...)
	if code != 5 || f2.started() {
		t.Errorf("default answer: exit %d started %v, want 5 and not started", code, f2.started())
	}
}

func TestRunDoesNotStartARunThatFailedItsChecks(t *testing.T) {
	h, f := newFlow(t)
	f.runState = "failed"
	f.reply("GET /api/v1/runs/{id}/plan", 200, manifestFixture(true))
	code, _, _ := h.run(planArgs("run", "--yes")...)
	if code != 1 || f.started() {
		t.Errorf("exit %d started %v, want 1 and not started", code, f.started())
	}
}

// The final state decides the exit code of run --yes --watch.
func TestRunYesWatchExitCodePerFinalState(t *testing.T) {
	tests := []struct {
		state string
		want  int
	}{
		{"succeeded", 0}, {"failed", 1}, {"partially_succeeded", 3},
		{"cancelled", 4}, {"rejected", 4}, {"expired", 4}, {"invalidated", 4},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			h, f := newFlow(t)
			f.stream(frame("state", "", stateBody("running", "")), frame("end", "", stateBody(tt.state, "because")))
			code, stdout, stderr := h.run(planArgs("run", "--yes", "--watch")...)
			if code != tt.want {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tt.want, stderr)
			}
			contains(t, "stdout", stdout, "Run 14 finished: "+tt.state)
		})
	}
}

// Review Focus 1: approval needed.
func TestRunNeedingApprovalExitsFiveAndSaysSo(t *testing.T) {
	h, f := newFlow(t)
	waiting := stateBody("waiting_approval", "")
	waiting["wait_reason"] = "waiting for approval"
	f.startedTo = "waiting_approval"
	f.stream(frame("state", "", waiting)) // the stream stays open in reality; here it closes
	code, stdout, stderr := h.run(planArgs("run", "--yes", "--watch")...)
	if code != 5 {
		t.Errorf("exit = %d, want 5", code)
	}
	contains(t, "output", stdout+stderr, "waiting for approval", "/runs/14", "yumdrake approvals approve 14", "approve permission")
	if len(f.requests("POST /api/v1/runs/14/cancel")) != 0 {
		t.Error("the run was cancelled")
	}
}

func TestRunWithoutWatchStopsAtApprovalWithExitFive(t *testing.T) {
	h, f := newFlow(t)
	f.startedTo = "waiting_approval"
	code, stdout, stderr := h.run(planArgs("run", "--yes")...)
	if code != 5 {
		t.Errorf("exit = %d, want 5", code)
	}
	contains(t, "output", stdout+stderr, "waiting for approval", "/runs/14")
}

func TestWaitForApprovalKeepsWatchingUntilTheRunEnds(t *testing.T) {
	h, f := newFlow(t)
	waiting := stateBody("waiting_approval", "")
	waiting["wait_reason"] = "waiting for approval"
	f.stream(frame("state", "", waiting), frame("state", "", stateBody("queued", "")), frame("state", "", stateBody("running", "")),
		frame("end", "", stateBody("succeeded", "")))
	code, stdout, _ := h.run("runs", "watch", "14", "--wait-for-approval")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	contains(t, "stdout", stdout, "waiting for approval", "Run 14: queued", "Run 14: running", "Run 14 finished: succeeded")
}

func TestWatchOfARunNeedingReconciliationExitsFive(t *testing.T) {
	h, f := newFlow(t)
	f.stream(frame("state", "", stateBody("needs_reconciliation", "", leg(101, "roll-backends", "billing", "unknown", "outcome not known"))))
	code, stdout, stderr := h.run("runs", "watch", "14")
	if code != 5 {
		t.Errorf("exit = %d, want 5", code)
	}
	contains(t, "output", stdout+stderr, "billing", "outcome not known", "/runs/14", "yumdrake runs reconcile 14")
}

func TestWatchOfAPlannedRunExitsFive(t *testing.T) {
	h, f := newFlow(t)
	f.stream(frame("state", "", stateBody("planned", "")))
	code, _, stderr := h.run("runs", "watch", "14")
	if code != 5 || !strings.Contains(stderr, "yumdrake runs start 14") {
		t.Errorf("exit %d stderr %q", code, stderr)
	}
}

func TestWatchPrintsEachChangeOnceAndLogsOnRequest(t *testing.T) {
	h, f := newFlow(t)
	logs := func(text string) map[string]any {
		return map[string]any{"lines": []any{map[string]any{"leg_id": 101, "seq": 1, "at": "2026-11-27T10:01:02Z", "level": "info", "text": text}},
			"next": "c1", "gaps": []any{}}
	}
	running := leg(101, "roll-backends", "billing", "running", "")
	failed := leg(101, "roll-backends", "billing", "failed", "health checks failed")
	f.stream(
		frame("state", "", stateBody("running", "", leg(100, "roll-backends", "orders", "pending", ""), running)),
		frame("log", "c1", logs("deploying billing")),
		frame("ping", "", map[string]any{}),                                                                       // the server's keep-alive: printed as nothing
		frame("state", "", stateBody("running", "", leg(100, "roll-backends", "orders", "pending", ""), running)), // nothing changed
		frame("state", "", stateBody("running", "", leg(100, "roll-backends", "orders", "running", ""), failed)),
		frame("end", "", stateBody("failed", "1 service failed", failed)),
	)
	code, stdout, _ := h.run("runs", "watch", "14", "--logs")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	contains(t, "stdout", stdout, "roll-backends / billing: running", "roll-backends / billing: failed: health checks failed",
		"roll-backends / orders: running", "deploying billing", "Run 14 finished: failed", "1 service failed")
	if n := strings.Count(stdout, "roll-backends / billing: running"); n != 1 {
		t.Errorf("a repeated state was printed %d times:\n%s", n, stdout)
	}
	if strings.Contains(stdout, "orders: pending") {
		t.Errorf("a leg that never started is reported:\n%s", stdout)
	}

	// Without --logs no log line is printed.
	_, stdout, _ = h.run("runs", "watch", "14")
	if strings.Contains(stdout, "deploying billing") {
		t.Errorf("log shown without --logs:\n%s", stdout)
	}
}

// Review Focus 2: the connection drops; resume without repeating lines.
func TestWatchReconnectsAndDoesNotRepeatLines(t *testing.T) {
	h, f := newFlow(t)
	var mu sync.Mutex
	var connections int
	var lastIDs []string
	line := func(seq int, text string) map[string]any {
		return map[string]any{"lines": []any{map[string]any{"leg_id": 101, "seq": seq, "at": "2026-11-27T10:01:02Z", "level": "info", "text": text}},
			"next": fmt.Sprintf("c%d", seq), "gaps": []any{}}
	}
	running := leg(101, "roll-backends", "billing", "running", "")
	f.set("GET /api/v1/runs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connections++
		n := connections
		lastIDs = append(lastIDs, r.Header.Get("Last-Event-ID"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			_, _ = fmt.Fprint(w, frame("state", "", stateBody("running", "", running)), frame("log", "c1", line(1, "first line")))
		default:
			// The server sends the state again, then only what is after c1.
			_, _ = fmt.Fprint(w, frame("state", "", stateBody("running", "", running)), frame("log", "c2", line(2, "second line")),
				frame("end", "", stateBody("succeeded", "")))
		}
	})
	code, stdout, stderr := h.run("runs", "watch", "14", "--logs")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if connections != 2 || lastIDs[1] != "c1" {
		t.Errorf("connections %d, Last-Event-ID %v", connections, lastIDs)
	}
	for _, text := range []string{"first line", "second line", "roll-backends / billing: running"} {
		if n := strings.Count(stdout, text); n != 1 {
			t.Errorf("%q appears %d times:\n%s", text, n, stdout)
		}
	}
}

func TestWatchDoesNotRepeatALogLineTheServerSendsAgain(t *testing.T) {
	h, f := newFlow(t)
	line := map[string]any{"lines": []any{map[string]any{"leg_id": 101, "seq": 1, "at": "2026-11-27T10:01:02Z", "level": "info", "text": "once"}},
		"next": "c1", "gaps": []any{}}
	running := leg(101, "roll-backends", "billing", "running", "")
	f.stream(frame("state", "", stateBody("running", "", running)), frame("log", "c1", line), frame("log", "c1", line), frame("end", "", stateBody("succeeded", "")))
	_, stdout, _ := h.run("runs", "watch", "14", "--logs")
	if n := strings.Count(stdout, "once"); n != 1 {
		t.Errorf("line printed %d times", n)
	}
}

// Review Focus 2: after two minutes without a connection, exit 7.
func TestWatchGivesUpAfterTwoMinutesAndSaysTheRunContinues(t *testing.T) {
	h, f := newFlow(t)
	f.set("GET /api/v1/runs/{id}/events", func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	})
	code, _, stderr := h.run("runs", "watch", "14")
	if code != 7 {
		t.Errorf("exit = %d, want 7", code)
	}
	contains(t, "stderr", stderr, "still going", "yumdrake runs watch 14")
}

// Review Focus 4: Ctrl-C stops watching and never cancels.
func TestInterruptStopsWatchingAndNeverCancels(t *testing.T) {
	h, f := newFlow(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.ctx = ctx
	f.set("GET /api/v1/runs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, frame("state", "", stateBody("running", "")))
		w.(http.Flusher).Flush()
		cancel() // the person presses Ctrl-C
		<-r.Context().Done()
	})
	code, _, stderr := h.run("runs", "watch", "14")
	if code != 130 {
		t.Errorf("exit = %d, want 130", code)
	}
	contains(t, "stderr", stderr, "Stopped watching. The run continues: yumdrake runs watch 14. To cancel it: yumdrake runs cancel 14")
	if strings.Contains(stderr, "Sign-in") {
		t.Errorf("stderr = %q", stderr)
	}
	if n := len(f.requests("POST /api/v1/runs/14/cancel")); n != 0 {
		t.Errorf("the run was cancelled %d times", n)
	}
}

func TestWatchJSONPrintsOneDocumentAtTheEnd(t *testing.T) {
	h, f := newFlow(t)
	f.runState = "succeeded"
	f.stream(frame("state", "", stateBody("running", "")), frame("end", "", stateBody("succeeded", "")))
	code, stdout, stderr := h.run("runs", "watch", "14", "-o", "json")
	var out map[string]any
	decodeOne(t, stdout, &out)
	if code != 0 || out["state"] != "succeeded" {
		t.Errorf("exit %d out %v", code, out)
	}
	contains(t, "progress goes to stderr", stderr, "Run 14")
}

func TestWatchWithoutAccessSaysSo(t *testing.T) {
	h, f := newFlow(t)
	f.reply("GET /api/v1/runs/{id}/events", 403, errBody("forbidden", "no"))
	code, _, stderr := h.run("runs", "watch", "14")
	if code != 6 || !strings.Contains(stderr, "Not permitted") {
		t.Errorf("exit %d stderr %q", code, stderr)
	}
	assertNoToken(t, stderr)
}

// ---- start and cancel ----

func TestRunsStartStartsAPlannedRun(t *testing.T) {
	h, f := newFlow(t)
	code, stdout, stderr := h.run("runs", "start", "14")
	if code != 0 || !f.started() {
		t.Errorf("exit %d started %v: %s", code, f.started(), stderr)
	}
	contains(t, "stdout", stdout, "Run 14 started", "queued", "yumdrake runs watch 14")

	f.startedTo = "waiting_approval"
	code, stdout, stderr = h.run("runs", "start", "14")
	if code != 5 {
		t.Errorf("approval: exit %d", code)
	}
	contains(t, "output", stdout+stderr, "waiting for approval")

	f.reply("POST /api/v1/runs/{id}/start", 409, errBody("conflict", "run 14 is succeeded, not planned"))
	code, _, stderr = h.run("runs", "start", "14")
	if code != 2 || !strings.Contains(stderr, "not planned") {
		t.Errorf("conflict: exit %d stderr %q", code, stderr)
	}
}

func TestRunsStartWithWatchFollowsTheRun(t *testing.T) {
	h, f := newFlow(t)
	f.stream(frame("end", "", stateBody("failed", "")))
	code, _, _ := h.run("runs", "start", "14", "--watch")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}

func TestCancelAsksUnlessYes(t *testing.T) {
	cancelled := func(f *flow) bool { return len(f.requests("POST /api/v1/runs/14/cancel")) > 0 }

	h, f := newFlow(t)
	h.stdinTerminal, h.stdin = true, "n\n"
	code, _, stderr := h.run("runs", "cancel", "14")
	if cancelled(f) || code != 1 {
		t.Errorf("answer n: cancelled %v exit %d", cancelled(f), code)
	}
	contains(t, "prompt", stderr, "Cancel run 14? [y/N]")

	h, f = newFlow(t)
	h.stdinTerminal, h.stdin = true, "yes\n"
	if code, _, _ := h.run("runs", "cancel", "14"); code != 0 || !cancelled(f) {
		t.Errorf("answer yes: cancelled %v exit %d", cancelled(f), code)
	}

	h, f = newFlow(t)
	if code, _, _ := h.run("runs", "cancel", "14", "--yes"); code != 0 || !cancelled(f) {
		t.Errorf("--yes: cancelled %v exit %d", cancelled(f), code)
	}

	h, f = newFlow(t)
	code, _, stderr = h.run("runs", "cancel", "14")
	if code != 2 || cancelled(f) || !strings.Contains(stderr, "--yes") {
		t.Errorf("no terminal: exit %d cancelled %v stderr %q", code, cancelled(f), stderr)
	}
}

func TestPlanRunAndWatchHelpHaveAnExample(t *testing.T) {
	h, _ := newFlow(t)
	for _, args := range [][]string{{"plan"}, {"run"}, {"runs", "start"}, {"runs", "watch"}, {"runs", "cancel"}} {
		code, stdout, _ := h.run(append(args, "-h")...)
		if code != 0 || !strings.Contains(stdout, "Example:") {
			t.Errorf("%v -h: exit %d stdout %q", args, code, stdout)
		}
	}
}
