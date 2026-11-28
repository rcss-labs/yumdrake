//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// finishedRun is a dev run of s1 and s2 that ended in state: s1 was deployed
// (a done update-service operation on its deploy leg), s2's deploy leg failed.
func (f *runFixture) finishedRun(state string) int64 {
	f.t.Helper()
	id := f.newRun(f.jobID, devEnvironment, "s1", "s2")
	service := func(name string) manifest.Service {
		return manifest.Service{
			Name: name, Group: "g", Version: "1.0", ImageDigest: "sha256:aaaa",
			ECSService:             fmt.Sprintf("arn:aws:ecs:eu-west-1:1:service/c/%s-%s", name, devEnvironment),
			PreviousTaskDefinition: "arn:aws:ecs:eu-west-1:1:task-definition/" + name + ":4", PreviousVersion: "0.9",
			Config: []manifest.ConfigItem{{Kind: "toml", Name: "app", Destination: fmt.Sprintf("s3://cfg/%s/%s.toml", devEnvironment, name), SHA256: "bbbb", Change: "changed"}},
		}
	}
	f.storePlan(id, manifest.Manifest{
		RunID: id, Environment: devEnvironment, Label: "dev", AWSAccount: "111122223333",
		Services: []manifest.Service{service("s1"), service("s2")}, Checks: []manifest.Check{},
	})
	deployLeg := func(name string) int64 {
		var legID int64
		if err := f.pool.QueryRow(context.Background(),
			`SELECT l.id FROM legs l JOIN run_steps s ON s.id = l.step_id WHERE l.run_id = $1 AND s.name = 'deploy' AND l.service = $2`, id, name).Scan(&legID); err != nil {
			f.t.Fatalf("deploy leg of %s: %v", name, err)
		}
		return legID
	}
	f.exec(`INSERT INTO operations (leg_id, seq, name, key, state, at) VALUES ($1, 1, 'update-service', 'k', 'done', now())`, deployLeg("s1"))
	f.exec(`UPDATE legs SET state = 'succeeded' WHERE run_id = $1 AND service = 's1'`, id)
	f.exec(`UPDATE legs SET state = 'failed' WHERE id = $1`, deployLeg("s2"))
	f.exec(`UPDATE legs SET state = 'skipped' WHERE run_id = $1 AND service = 's2' AND state = 'pending'`, id)
	f.exec(`UPDATE runs SET state = $2, finished_at = now() WHERE id = $1`, id, state)
	return id
}

func TestRestoreAndRerunEndpointsPermissionsAndAudit(t *testing.T) {
	f := newRunFixture(t)
	restorer := f.userWith("restorer", store.Grant{Action: "restore"})
	runner := f.userWith("runner", store.Grant{Action: "run"})
	prodRestorer := f.userWith("prodrestorer", store.Grant{Action: "restore", Label: "prod"})
	plain := f.userWith("plain")

	cases := []struct {
		name   string
		token  string
		setup  func() int64
		suffix string
		body   any
		want   int
		audit  string // the audit action written, "" for none
		check  func(t *testing.T, source int64, got map[string]any)
	}{
		{"restore as restorer", restorer, func() int64 { return f.finishedRun("partially_succeeded") }, "/restore", map[string]any{"services": []string{}}, 201, "run.restore",
			func(t *testing.T, source int64, got map[string]any) {
				if got["kind"] != "restore" || got["state"] != "resolving" || got["restore_of"] != float64(source) || got["id"] == float64(source) {
					t.Errorf("restore run = kind %v state %v restore_of %v id %v", got["kind"], got["state"], got["restore_of"], got["id"])
				}
				steps, _ := got["steps"].([]any)
				if len(steps) != 2 || steps[0].(map[string]any)["name"] != "restore-config" || steps[1].(map[string]any)["name"] != "restore-deploy" {
					t.Errorf("steps = %v", got["steps"])
				}
				if got["auto_start"] != false {
					t.Errorf("auto_start = %v, a restore never starts by itself", got["auto_start"])
				}
				legs, _ := got["legs"].([]any)
				services := map[string]bool{}
				for _, l := range legs {
					if s := l.(map[string]any)["service"].(string); s != "" {
						services[s] = true
					}
				}
				if !reflect.DeepEqual(services, map[string]bool{"s1": true}) {
					t.Errorf("restore services = %v, want only s1, the service the run changed", services)
				}
			}},
		{"restore named service", restorer, func() int64 { return f.finishedRun("failed") }, "/restore", map[string]any{"services": []string{"s1"}}, 201, "run.restore", nil},
		{"restore without a body", restorer, func() int64 { return f.finishedRun("failed") }, "/restore", nil, 201, "run.restore", nil},
		{"restore a service the run did not change", restorer, func() int64 { return f.finishedRun("failed") }, "/restore", map[string]any{"services": []string{"s2"}}, 400, "", nil},
		{"restore with an unknown field", restorer, func() int64 { return f.finishedRun("failed") }, "/restore", map[string]any{"service": "s1"}, 400, "", nil},
		{"restore without restore", runner, func() int64 { return f.finishedRun("failed") }, "/restore", map[string]any{"services": []string{}}, 403, "", nil},
		{"restore as a plain user", plain, func() int64 { return f.finishedRun("failed") }, "/restore", map[string]any{"services": []string{}}, 403, "", nil},
		{"restore with restore on another label", prodRestorer, func() int64 { return f.finishedRun("failed") }, "/restore", map[string]any{"services": []string{}}, 403, "", nil},
		{"restore a run that is still going", restorer, func() int64 { return f.finishedRun("running") }, "/restore", map[string]any{"services": []string{}}, 409, "", nil},
		{"restore a run that changed nothing", restorer, func() int64 {
			id := f.finishedRun("failed")
			f.exec(`DELETE FROM operations WHERE leg_id IN (SELECT id FROM legs WHERE run_id = $1)`, id)
			return id
		}, "/restore", map[string]any{"services": []string{}}, 409, "", nil},
		{"restore an unknown run", restorer, func() int64 { return 999999 }, "/restore", map[string]any{"services": []string{}}, 404, "", nil},

		{"re-run failed as runner", runner, func() int64 { return f.finishedRun("partially_succeeded") }, "/rerun-failed", map[string]any{}, 201, "run.create",
			func(t *testing.T, source int64, got map[string]any) {
				if got["kind"] != "deploy" || got["state"] != "resolving" || got["restore_of"] != nil || got["id"] == float64(source) {
					t.Errorf("re-run = kind %v state %v restore_of %v", got["kind"], got["state"], got["restore_of"])
				}
				legs, _ := got["legs"].([]any)
				services := map[string]bool{}
				for _, l := range legs {
					if s := l.(map[string]any)["service"].(string); s != "" {
						services[s] = true
					}
				}
				if !reflect.DeepEqual(services, map[string]bool{"s2": true}) {
					t.Errorf("re-run services = %v, want only the failed s2", services)
				}
			}},
		{"re-run failed without run", restorer, func() int64 { return f.finishedRun("partially_succeeded") }, "/rerun-failed", map[string]any{}, 403, "", nil},
		{"re-run a run that is still going", runner, func() int64 { return f.finishedRun("running") }, "/rerun-failed", map[string]any{}, 409, "", nil},
		{"re-run a run with nothing to re-run", runner, func() int64 {
			id := f.finishedRun("succeeded")
			f.exec(`UPDATE legs SET state = 'succeeded' WHERE run_id = $1`, id)
			return id
		}, "/rerun-failed", map[string]any{}, 409, "", nil},
		{"re-run an unknown run", runner, func() int64 { return 999999 }, "/rerun-failed", map[string]any{}, 404, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := c.setup()
			before := len(f.auditEntries())
			code, got := f.post(c.token, runPath(source, c.suffix), c.body)
			if code != c.want {
				t.Fatalf("status = %d %v, want %d", code, got, c.want)
			}
			if c.check != nil {
				c.check(t, source, got)
			}
			entries := f.auditEntries()
			added := len(entries) - before
			switch {
			case c.audit != "" && (added != 1 || entries[0].Action != c.audit):
				t.Errorf("audit entries added = %d (latest %q), want exactly one %q (the engine's own, not doubled)", added, entries[0].Action, c.audit)
			case c.audit == "" && added != 0:
				t.Errorf("a refused action wrote %d audit entries", added)
			}
			if c.want == http.StatusCreated {
				var detail map[string]any
				if err := json.Unmarshal(entries[0].Detail, &detail); err != nil {
					t.Fatalf("audit detail: %v", err)
				}
				key := "source_run_id"
				if c.suffix == "/rerun-failed" {
					key = "rerun_of"
				}
				if detail[key] != float64(source) || detail["via"] != "cli" {
					t.Errorf("audit detail = %v, want %s %d and via cli (the token)", detail, key, source)
				}
			}
		})
	}
}

func TestPlanViewServesRestoreFields(t *testing.T) {
	f := newRunFixture(t)
	id := f.newRun(f.jobID, devEnvironment, "s1")
	f.storePlan(id, manifest.Manifest{
		RunID: id, Environment: devEnvironment, Label: "dev", AWSAccount: "111122223333", RestoreOf: 7,
		Services: []manifest.Service{{
			Name: "s1", Version: "0.9", ECSService: "arn:aws:ecs:eu-west-1:1:service/c/s1",
			PreviousTaskDefinition: "arn:aws:ecs:eu-west-1:1:task-definition/s1:9", TargetTaskDefinition: "arn:aws:ecs:eu-west-1:1:task-definition/s1:4",
			Config: []manifest.ConfigItem{
				{Kind: "env", Name: "app", Destination: "s3://cfg/s1.env", Change: "unchanged", NotRestored: "did not exist before run #7; left in place"},
				{Kind: "toml", Name: "settings", Destination: "s3://cfg/s1.toml", Change: "changed", SHA256: "cafe"},
			},
		}},
		Checks: []manifest.Check{},
	})
	code, raw := f.doRaw("GET", runPath(id, "/plan"), f.adminToken, nil)
	if code != http.StatusOK {
		t.Fatalf("GET plan = %d %s", code, raw)
	}
	var got struct {
		RestoreOf *int64 `json:"restore_of"`
		Services  []struct {
			TargetTaskDefinition string `json:"target_task_definition"`
			Config               []struct {
				NotRestored *string `json:"not_restored"`
			} `json:"config"`
		} `json:"services"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if got.RestoreOf == nil || *got.RestoreOf != 7 {
		t.Errorf("restore_of = %v, want 7 in %s", got.RestoreOf, raw)
	}
	if len(got.Services) != 1 || got.Services[0].TargetTaskDefinition != "arn:aws:ecs:eu-west-1:1:task-definition/s1:4" {
		t.Fatalf("services = %+v in %s", got.Services, raw)
	}
	config := got.Services[0].Config
	if len(config) != 2 || config[0].NotRestored == nil || *config[0].NotRestored != "did not exist before run #7; left in place" || config[1].NotRestored != nil {
		t.Errorf("not_restored of the items = %+v, want the reason of the first and none for the second", config)
	}

	// A deploy's plan has none of the three.
	deploy := f.newRun(f.jobID, devEnvironment, "s1")
	f.storePlan(deploy, manifest.Manifest{
		RunID: deploy, Environment: devEnvironment, Label: "dev", AWSAccount: "111122223333",
		Services: []manifest.Service{{Name: "s1", Version: "1.0", Config: []manifest.ConfigItem{{Kind: "toml", Name: "t", Destination: "s3://x/y", Change: "new"}}}},
		Checks:   []manifest.Check{},
	})
	_, plain := f.doRaw("GET", runPath(deploy, "/plan"), f.adminToken, nil)
	var decoded map[string]any
	if err := json.Unmarshal(plain, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, present := decoded["restore_of"]; present {
		t.Errorf("a deploy plan serves restore_of: %s", plain)
	}
	service := decoded["services"].([]any)[0].(map[string]any)
	item := service["config"].([]any)[0].(map[string]any)
	if _, present := service["target_task_definition"]; present || item["not_restored"] != nil {
		t.Errorf("a deploy plan serves restore fields: %s", plain)
	}
}

func TestRunSummaryServesRestoreOf(t *testing.T) {
	f := newRunFixture(t)
	source := f.finishedRun("failed")
	restorer := f.userWith("restorer", store.Grant{Action: "restore"})
	code, created := f.post(restorer, runPath(source, "/restore"), map[string]any{})
	if code != http.StatusCreated {
		t.Fatalf("restore = %d %v", code, created)
	}
	if code, list := f.get(f.adminToken, "/api/v1/runs?limit=50"); code != http.StatusOK {
		t.Fatalf("list runs = %d", code)
	} else {
		byID := map[float64]any{}
		for _, r := range list["runs"].([]any) {
			byID[r.(map[string]any)["id"].(float64)] = r.(map[string]any)["restore_of"]
		}
		if byID[created["id"].(float64)] != float64(source) || byID[float64(source)] != nil {
			t.Errorf("restore_of by run = %v, want the source on the restore and null on the source", byID)
		}
	}
}

func TestStartRestoreNeedsRestoreNotRun(t *testing.T) {
	f := newRunFixture(t)
	restorer := f.userWith("restorer", store.Grant{Action: "restore"})
	runner := f.userWith("runner", store.Grant{Action: "run"})
	code, created := f.post(restorer, runPath(f.finishedRun("failed"), "/restore"), map[string]any{})
	if code != http.StatusCreated {
		t.Fatalf("restore = %d %v", code, created)
	}
	id := int64(created["id"].(float64))
	f.exec(`UPDATE runs SET state = 'planned' WHERE id = $1`, id)

	if code, got := f.post(runner, runPath(id, "/start"), nil); code != http.StatusForbidden {
		t.Fatalf("start of a restore with only run = %d %v, want 403", code, got)
	}
	f.wantRunState(t, id, "planned")
	if code, got := f.post(restorer, runPath(id, "/start"), nil); code != http.StatusOK {
		t.Fatalf("start of a restore with restore = %d %v, want 200", code, got)
	}
}
