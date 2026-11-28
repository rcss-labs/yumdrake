//go:build e2e && floci

package e2e

// End-to-end proof of the visibility screens' data against Floci: two real runs
// through the API and the engine (one clean, one with a failing service), what
// the overview, the running-versions matrix, the queue and System health say
// about them, and what they say once the runner is stopped.
//
// ecs-deploy is a stand-in here (Plan 6's real step is not built yet). After a
// succeeded leg the test does what Plan 6 will do: it registers a task
// definition revision tagged yumdrake:version, yumdrake:run, yumdrake:leg and
// yumdrake:manifest and points the ECS service at it, so the matrix reads real
// ECS state.

import (
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/rcss-labs/yamdrake/internal/queue"
)

const (
	// deployFailure is the short reason the failing service's leg reports.
	deployFailure = "health checks failed"
	// failingService fails in the second run.
	failingService = "api"
	// handChanged is changed in the console after the first run.
	handChanged = "worker"
	// runnerGoneAfter is how long the orchestrator waits before it calls a
	// silent runner gone, shortened from 2 minutes for the test.
	runnerGoneAfter = 3 * time.Second
)

type visibilityOverview struct {
	Runs        int      `json:"runs"`
	SuccessRate *float64 `json:"success_rate"`
	TopFailing  []struct {
		Service      string `json:"service"`
		Failures     int    `json:"failures"`
		CommonReason string `json:"common_reason"`
	} `json:"top_failing"`
	Attention []struct {
		RunID       int64  `json:"run_id"`
		State       string `json:"state"`
		Environment string `json:"environment"`
		Reason      string `json:"reason"`
	} `json:"attention"`
	Environments []struct {
		Environment string `json:"environment"`
		RunID       *int64 `json:"run_id"`
		State       string `json:"state"`
	} `json:"environments"`
}

type visibilityCell struct {
	State            string `json:"state"`
	Version          string `json:"version"`
	TaskDefinition   string `json:"task_definition"`
	ByYumdrake       bool   `json:"by_yumdrake"`
	RunID            int64  `json:"run_id"`
	LastDeployFailed bool   `json:"last_deploy_failed"`
	Error            string `json:"error"`
}

type visibilityMatrix struct {
	Environments []struct {
		Name string `json:"name"`
	} `json:"environments"`
	Rows []struct {
		Service string           `json:"service"`
		Cells   []visibilityCell `json:"cells"`
	} `json:"rows"`
}

// cell returns the cell of a service in an environment.
func (m visibilityMatrix) cell(t *testing.T, service, environment string) visibilityCell {
	t.Helper()
	col := slices.IndexFunc(m.Environments, func(e struct {
		Name string `json:"name"`
	}) bool {
		return e.Name == environment
	})
	if col < 0 {
		t.Fatalf("the matrix has no environment %q", environment)
	}
	for _, row := range m.Rows {
		if row.Service == service {
			if col >= len(row.Cells) {
				t.Fatalf("row %s has %d cells, want one for %s", service, len(row.Cells), environment)
			}
			return row.Cells[col]
		}
	}
	t.Fatalf("the matrix has no row for %q", service)
	return visibilityCell{}
}

type visibilityQueue struct {
	Runners []struct {
		ID string `json:"id"`
	} `json:"runners"`
	FreeSlots int `json:"free_slots"`
}

type visibilityHealth struct {
	Problems []string `json:"problems"`
}

// deployLikePlanSix is the ecs-deploy stand-in's outcome: the failing service's
// leg of the second run fails; every other leg succeeds and leaves the tagged
// task definition a real deploy would.
func (s *flociStack) deployLikePlanSix(task queue.Claimed, call deployCall) queue.Outcome {
	if call.service.Name == failingService && task.Run.Environment == prodEnv {
		return queue.Outcome{State: "failed", FailureShort: deployFailure, FailureDetail: "the new tasks did not become healthy"}
	}
	e := s.envs[task.Run.Environment]
	svc := e.services[call.service.Name]
	e.acct.Redeploy(flociRegion, svc.cluster, call.service.Name, svc.repoURI+":"+call.service.Version, map[string]string{
		"yumdrake:version":  call.service.Version,
		"yumdrake:run":      strconv.FormatInt(task.Run.ID, 10),
		"yumdrake:leg":      strconv.FormatInt(task.Leg.ID, 10),
		"yumdrake:manifest": task.Run.ManifestID,
	})
	return queue.Outcome{State: "succeeded"}
}

func TestVisibilityAfterTwoRunsAndAStoppedRunner(t *testing.T) {
	s := startFloci(t, flociOpts{
		seed:            []string{devEnv, prodEnv},
		orchestratorEnv: map[string]string{"YUMDRAKE_RUNNER_GONE_AFTER": runnerGoneAfter.String()},
	})
	s.rec.setAfterDeploy(s.deployLikePlanSix)

	cleanRun := s.createRun(devEnv, planVersion, true)
	if got := s.waitRun(cleanRun, "succeeded", "failed", "partially_succeeded"); got.State != "succeeded" {
		t.Fatalf("the clean run ended %q (%s), want succeeded", got.State, got.EndReason)
	}
	failedRun := s.createRun(prodEnv, planVersion, true)
	if got := s.waitRun(failedRun, "succeeded", "failed", "partially_succeeded"); got.State != "partially_succeeded" {
		t.Fatalf("the run with a failing service ended %q (%s), want partially_succeeded", got.State, got.EndReason)
	}
	// Someone changes one service in the console: an untagged revision.
	s.deployByHand(s.envs[devEnv], handChanged)

	t.Run("overview", func(t *testing.T) {
		var o visibilityOverview
		s.api.call(http.MethodGet, "/api/v1/analytics/overview?days=14", nil, &o)
		if o.Runs != 2 {
			t.Errorf("runs = %d, want 2", o.Runs)
		}
		if o.SuccessRate == nil || *o.SuccessRate != 0.5 {
			t.Errorf("success rate = %v, want 0.5 (one of two finished runs succeeded)", o.SuccessRate)
		}
		if len(o.TopFailing) != 1 || o.TopFailing[0].Service != failingService || o.TopFailing[0].Failures != 1 || o.TopFailing[0].CommonReason != deployFailure {
			t.Errorf("top failing = %+v, want %s once with %q", o.TopFailing, failingService, deployFailure)
		}
		if len(o.Attention) != 1 || o.Attention[0].RunID != failedRun || o.Attention[0].Environment != prodEnv || o.Attention[0].State != "partially_succeeded" {
			t.Errorf("attention = %+v, want only run %d on %s", o.Attention, failedRun, prodEnv)
		}
		last := map[string]int64{}
		for _, e := range o.Environments {
			if e.RunID != nil {
				last[e.Environment] = *e.RunID
			}
		}
		if last[devEnv] != cleanRun || last[prodEnv] != failedRun {
			t.Errorf("last runs by environment = %v, want %s %d and %s %d", last, devEnv, cleanRun, prodEnv, failedRun)
		}
	})

	t.Run("matrix", func(t *testing.T) {
		var m visibilityMatrix
		s.api.call(http.MethodGet, "/api/v1/versions/running", nil, &m)
		for _, name := range []string{"web", "api"} {
			c := m.cell(t, name, devEnv)
			if c.State != "ok" || c.Version != planVersion || !c.ByYumdrake || c.RunID != cleanRun || c.LastDeployFailed {
				t.Errorf("%s in %s = %+v, want ok %s by Yumdrake from run %d, no failed deploy", name, devEnv, c, planVersion, cleanRun)
			}
		}
		if c := m.cell(t, handChanged, devEnv); c.State != "ok" || c.ByYumdrake || c.RunID != 0 {
			t.Errorf("%s in %s = %+v, want ok and not deployed by Yumdrake (changed by hand)", handChanged, devEnv, c)
		}
		for _, name := range []string{"web", "worker"} {
			c := m.cell(t, name, prodEnv)
			if c.State != "ok" || c.Version != planVersion || !c.ByYumdrake || c.RunID != failedRun || c.LastDeployFailed {
				t.Errorf("%s in %s = %+v, want ok %s by Yumdrake from run %d", name, prodEnv, c, planVersion, failedRun)
			}
		}
		if c := m.cell(t, failingService, prodEnv); c.State != "ok" || c.Version != runningVersion || c.ByYumdrake || !c.LastDeployFailed {
			t.Errorf("%s in %s = %+v, want ok, still %s, not by Yumdrake, last deploy failed", failingService, prodEnv, c, runningVersion)
		}
	})

	t.Run("a stopped runner shows in the queue and System health", func(t *testing.T) {
		var before visibilityQueue
		s.api.call(http.MethodGet, "/api/v1/queue", nil, &before)
		if len(before.Runners) != 1 {
			t.Fatalf("runners before the stop = %+v, want the one test runner", before.Runners)
		}
		s.stopRunner()

		want := "No runner has reported for " + strconv.Itoa(int(runnerGoneAfter/time.Second)) + " seconds."
		var q visibilityQueue
		var h visibilityHealth
		eventually(t, 15*time.Second, "the queue and System health to report the stopped runner", func() bool {
			s.api.call(http.MethodGet, "/api/v1/queue", nil, &q)
			s.api.call(http.MethodGet, "/api/v1/system/health", nil, &h)
			return len(q.Runners) == 0 && slices.Contains(h.Problems, want)
		})
		if q.FreeSlots != 0 {
			t.Errorf("free slots = %d with no runner, want 0", q.FreeSlots)
		}
	})
}
