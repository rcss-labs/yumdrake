//go:build integration

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// restoreService is a service of a finished run's manifest: it ran task
// definition revision 4 before the run and has one config file.
func restoreService(name string) manifest.Service {
	return manifest.Service{
		Name: name, Group: "g", Version: "1.0", ImageDigest: "sha256:aaaa",
		ECSService:             fmt.Sprintf("arn:aws:ecs:eu-west-1:%s:service/cluster/%s-staging", testAWSAccount, name),
		PreviousTaskDefinition: fmt.Sprintf("arn:aws:ecs:eu-west-1:%s:task-definition/%s:4", testAWSAccount, name),
		PreviousVersion:        "0.9",
		Config: []manifest.ConfigItem{{
			Kind: "toml", Name: "app", Destination: fmt.Sprintf("s3://cfg/staging/%s.toml", name),
			SHA256: "bbbb", PreviousExists: true, PreviousSHA256: "aaaa", PreviousArtifactKey: "manifests/1/" + name + "/prev", Change: "changed",
		}},
	}
}

// finishedRun creates a run of the services, plans it with the manifest, ends
// it in the state and records a done operation of the named kind for each
// service in changed.
func (h *harness) finishedRun(state string, services []manifest.Service, changed map[string]string) store.Run {
	h.t.Helper()
	names := make([]string, len(services))
	for i, s := range services {
		names[i] = s.Name
	}
	run := h.create(h.starter, false, names...)
	h.advance(run.ID)
	m := manifest.Manifest{
		RunID: run.ID, Environment: testEnvironment, Label: testLabel, AWSAccount: testAWSAccount,
		InventoryCommit: "inv-commit", JobCommit: "job-commit", Services: services, Checks: []manifest.Check{},
	}
	m.ID = manifest.Identity(m)
	h.settleResolve(run.ID, "succeeded", m)
	h.advance(run.ID)
	h.endRun(run.ID, state)
	for service, opName := range changed {
		h.recordOp(h.stepLeg(run.ID, "deploy", service).ID, opName, "done")
	}
	return h.run(run.ID)
}

func (h *harness) endRun(runID int64, state string) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(), `UPDATE runs SET state = $2, finished_at = now() WHERE id = $1`, runID, state); err != nil {
		h.t.Fatalf("end run: %v", err)
	}
}

func (h *harness) recordOp(legID int64, name, state string) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO operations (leg_id, seq, name, key, state, at)
		 VALUES ($1, (SELECT coalesce(max(seq), 0) + 1 FROM operations WHERE leg_id = $1), $2, $2 || '-' || $3, $3, now())`,
		legID, name, state); err != nil {
		h.t.Fatalf("record op: %v", err)
	}
}

func (h *harness) stepsOf(runID int64) []store.RunStep {
	h.t.Helper()
	steps, err := store.StepsOfRun(context.Background(), h.pool, runID)
	if err != nil {
		h.t.Fatalf("StepsOfRun: %v", err)
	}
	return steps
}

func TestCreateRestoreBuildsTwoStepsForChangedServices(t *testing.T) {
	h := newHarness(t, harnessOptions{approvalFor: []string{testLabel}})
	source := h.finishedRun("succeeded",
		[]manifest.Service{restoreService("s1"), restoreService("s2"), restoreService("s3")},
		map[string]string{"s1": "put-config", "s2": "update-service"}) // s3 changed nothing

	restore, err := h.eng.CreateRestore(context.Background(), h.starter, source.ID, nil, "cli")
	if err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}
	got := h.run(restore.ID)
	if got.Kind != "restore" || got.ParentRunID == nil || *got.ParentRunID != source.ID || got.State != "resolving" || got.AutoStart || got.Via != "cli" {
		t.Errorf("restore run = kind %q parent %v state %q auto-start %v via %q", got.Kind, got.ParentRunID, got.State, got.AutoStart, got.Via)
	}
	if got.Environment != source.Environment || got.Label != source.Label || got.AWSAccount != source.AWSAccount || got.JobSourceID != source.JobSourceID ||
		got.InventorySnapshotID != source.InventorySnapshotID || got.FolderID != source.FolderID || got.JobCommit != source.JobCommit {
		t.Errorf("restore run does not copy the source's target and snapshots: %+v", got)
	}
	if !got.ApprovalRequired {
		t.Errorf("approval required = false, want the source job's rule (approval for %s) to apply", testLabel)
	}

	steps := h.stepsOf(restore.ID)
	if len(steps) != 2 || steps[0].Name != "restore-config" || steps[0].Use != "copy-config" || steps[0].GroupID != "restore" || steps[0].Position != 0 ||
		steps[1].Name != "restore-deploy" || steps[1].Use != "ecs-deploy" || steps[1].GroupID != "restore" || steps[1].Position != 1 {
		t.Fatalf("steps = %+v, want restore-config (copy-config) then restore-deploy (ecs-deploy), both in group restore", steps)
	}
	var needs []spec.Need
	if err := json.Unmarshal(steps[1].Needs, &needs); err != nil || len(needs) != 1 || needs[0].Step != "restore-config" || needs[0].Scope != "service" {
		t.Errorf("restore-deploy needs = %s (%v), want restore-config with scope service", steps[1].Needs, err)
	}
	if len(steps[0].Needs) > 0 && string(steps[0].Needs) != "[]" {
		t.Errorf("restore-config needs = %s, want none", steps[0].Needs)
	}

	legs := h.legs(restore.ID)
	type legKey struct{ kind, step, service, version, state string }
	var gotLegs []legKey
	for _, l := range legs {
		step := ""
		if l.StepID != nil {
			for _, s := range steps {
				if s.ID == *l.StepID {
					step = s.Name
				}
			}
		}
		gotLegs = append(gotLegs, legKey{l.Kind, step, l.Service, l.Version, l.State})
	}
	wantLegs := []legKey{
		{"resolve", "", "", "", "queued"},
		{"step", "restore-config", "s1", "0.9", "pending"}, {"step", "restore-config", "s2", "0.9", "pending"},
		{"step", "restore-deploy", "s1", "0.9", "pending"}, {"step", "restore-deploy", "s2", "0.9", "pending"},
	}
	if !reflect.DeepEqual(gotLegs, wantLegs) {
		t.Errorf("legs = %v, want %v", gotLegs, wantLegs)
	}
	if legs[0].QueuedAt == nil {
		t.Error("the resolve leg has no queued time")
	}

	wantLocks := []string{
		"arn:aws:ecs:eu-west-1:111122223333:service/cluster/s1-staging", "arn:aws:ecs:eu-west-1:111122223333:service/cluster/s2-staging",
		"s3://cfg/staging/s1.toml", "s3://cfg/staging/s2.toml",
	}
	if !slices.Equal(got.LockResources, wantLocks) {
		t.Errorf("lock resources = %v, want %v", got.LockResources, wantLocks)
	}
	var sel Selection
	if err := json.Unmarshal(got.Selection, &sel); err != nil || !reflect.DeepEqual(sel, Selection{"restore": {Services: map[string]string{"s1": "0.9", "s2": "0.9"}}}) {
		t.Errorf("selection = %s (%v)", got.Selection, err)
	}

	lines := h.runLines(restore.ID)
	wantLine := fmt.Sprintf("Restore of run #%d created by starter for s1, s2; working out the plan.", source.ID)
	if len(lines) == 0 || lines[0] != wantLine {
		t.Errorf("run lines = %v, want first %q", lines, wantLine)
	}
	audits := h.audits("run.restore")
	if len(audits) != 1 || audits[0].TargetID != fmt.Sprint(restore.ID) {
		t.Fatalf("audit run.restore = %+v", audits)
	}
	var detail map[string]any
	if err := json.Unmarshal(audits[0].Detail, &detail); err != nil || detail["via"] != "cli" || detail["source_run_id"] != float64(source.ID) ||
		!reflect.DeepEqual(detail["services"], []any{"s1", "s2"}) {
		t.Errorf("audit detail = %s (%v)", audits[0].Detail, err)
	}
	if !h.pub.has("runs", restore.ID) {
		t.Error("creating the restore run did not publish a wake-up")
	}
}

func TestCreateRestoreWithNamedServicesRestoresOnlyThose(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	source := h.finishedRun("partially_succeeded",
		[]manifest.Service{restoreService("s1"), restoreService("s2")},
		map[string]string{"s1": "update-service", "s2": "put-config"})

	restore, err := h.eng.CreateRestore(context.Background(), h.starter, source.ID, []string{"s2"}, "web")
	if err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}
	if got := h.run(restore.ID); got.ApprovalRequired {
		t.Error("approval required although the job has no approval rule")
	}
	var services []string
	for _, l := range h.legs(restore.ID) {
		if l.Kind == "step" && !slices.Contains(services, l.Service) {
			services = append(services, l.Service)
		}
	}
	if !slices.Equal(services, []string{"s2"}) {
		t.Errorf("restore legs are for %v, want only s2", services)
	}

	_, err = h.eng.CreateRestore(context.Background(), h.starter, source.ID, []string{"s1", "nope"}, "web")
	if !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), `the run did not change service "nope"`) {
		t.Errorf("CreateRestore with an unchanged service = %v, want ErrInvalid naming it", err)
	}
}

func TestCreateRestoreNeedsRestorePermission(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	source := h.finishedRun("succeeded", []manifest.Service{restoreService("s1")}, map[string]string{"s1": "update-service"})
	before := len(h.audits("run.restore"))

	for name, person := range map[string]auth.Principal{"stranger": h.stranger, "approver": h.approver} {
		if _, err := h.eng.CreateRestore(context.Background(), person, source.ID, nil, "web"); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: CreateRestore = %v, want ErrForbidden", name, err)
		}
	}
	if len(h.audits("run.restore")) != before {
		t.Error("a refused restore left an audit entry")
	}
	var runs int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM runs WHERE kind = 'restore'`).Scan(&runs); err != nil || runs != 0 {
		t.Errorf("restore runs = %d (%v), want none", runs, err)
	}
}

func TestRestoreOfRunThatChangedNothingIsRefused(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	// Operations that never completed, or are not changes, do not count.
	failedRun := h.finishedRun("failed", []manifest.Service{restoreService("s1")}, nil)
	leg := h.stepLeg(failedRun.ID, "deploy", "s1")
	h.recordOp(leg.ID, "update-service", "intended")
	h.recordOp(h.stepLeg(failedRun.ID, "config", "s1").ID, "put-config", "failed")
	h.recordOp(leg.ID, "describe", "done")
	if _, err := h.eng.CreateRestore(ctx, h.starter, failedRun.ID, nil, "web"); !errors.Is(err, ErrWrongState) || !strings.Contains(err.Error(), fmt.Sprintf("nothing in run #%d can be restored", failedRun.ID)) {
		t.Errorf("restore of a run that changed nothing = %v, want ErrWrongState saying nothing can be restored", err)
	}

	// A run still going cannot be restored, even when it has changed something.
	running := h.finishedRun("running", []manifest.Service{restoreService("s1")}, map[string]string{"s1": "update-service"})
	if _, err := h.eng.CreateRestore(ctx, h.starter, running.ID, nil, "web"); !errors.Is(err, ErrWrongState) {
		t.Errorf("restore of a running run = %v, want ErrWrongState", err)
	}

	// A run that never resolved has no manifest.
	unplanned := h.create(h.starter, false, "s1")
	h.endRun(unplanned.ID, "cancelled")
	if _, err := h.eng.CreateRestore(ctx, h.starter, unplanned.ID, nil, "web"); !errors.Is(err, ErrWrongState) {
		t.Errorf("restore of a run without a manifest = %v, want ErrWrongState", err)
	}
	if _, err := h.eng.CreateRestore(ctx, h.starter, 987654, nil, "web"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("restore of an unknown run = %v, want ErrNotFound", err)
	}
}

func TestRestoreCanItselfBeRestored(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	source := h.finishedRun("succeeded", []manifest.Service{restoreService("s1")}, map[string]string{"s1": "update-service"})
	first, err := h.eng.CreateRestore(context.Background(), h.starter, source.ID, nil, "web")
	if err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}

	// The restore resolves, recording the state before it as its previous state, and runs.
	restored := restoreService("s1")
	restored.PreviousTaskDefinition = "arn:aws:ecs:eu-west-1:111122223333:task-definition/s1:9"
	restored.PreviousVersion = "1.0"
	restored.TargetTaskDefinition = "arn:aws:ecs:eu-west-1:111122223333:task-definition/s1:4"
	h.advance(first.ID)
	m := manifest.Manifest{
		RunID: first.ID, RestoreOf: source.ID, Environment: testEnvironment, Label: testLabel, AWSAccount: testAWSAccount,
		InventoryCommit: "inv-commit", JobCommit: "job-commit", Services: []manifest.Service{restored}, Checks: []manifest.Check{},
	}
	m.ID = manifest.Identity(m)
	h.settleResolve(first.ID, "succeeded", m)
	h.advance(first.ID)
	h.endRun(first.ID, "succeeded")
	h.recordOp(h.stepLeg(first.ID, "restore-deploy", "s1").ID, "update-service", "done")

	second, err := h.eng.CreateRestore(context.Background(), h.starter, first.ID, nil, "web")
	if err != nil {
		t.Fatalf("restoring a restore: %v", err)
	}
	got := h.run(second.ID)
	var sel Selection
	if got.Kind != "restore" || got.ParentRunID == nil || *got.ParentRunID != first.ID ||
		json.Unmarshal(got.Selection, &sel) != nil || sel["restore"].Services["s1"] != "1.0" {
		t.Errorf("second restore = kind %q parent %v selection %s, want a restore of run #%d back to version 1.0", got.Kind, got.ParentRunID, got.Selection, first.ID)
	}
}

func TestRerunSelectionCoversFailedSkippedAndCancelled(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()
	// s1 had its own version in the source run; the others took the group's.
	source := h.finishedRunWithSelection(Selection{"g": {Version: "1.0", Services: map[string]string{"s1": "1.1", "s2": "", "s3": "", "s4": ""}}})
	for service, state := range map[string]string{"s1": "failed", "s2": "skipped", "s3": "cancelled", "s4": "succeeded"} {
		for _, step := range []string{"config", "deploy"} {
			h.setLeg(h.stepLeg(source.ID, step, service).ID, state)
		}
	}

	jobSourceID, envName, sel, err := h.eng.RerunSelection(ctx, h.starter, source.ID)
	if err != nil {
		t.Fatalf("RerunSelection: %v", err)
	}
	if jobSourceID != h.sourceID || envName != testEnvironment {
		t.Errorf("job source %d environment %q, want %d and %q", jobSourceID, envName, h.sourceID, testEnvironment)
	}
	want := Selection{"g": {Version: "1.0", Services: map[string]string{"s1": "1.1", "s2": "1.0", "s3": "1.0"}}}
	if !reflect.DeepEqual(sel, want) {
		t.Errorf("selection = %+v, want %+v (the failed, skipped and cancelled services at the versions they had)", sel, want)
	}

	rerun, err := h.eng.CreateRerun(ctx, h.starter, source.ID, "web")
	if err != nil {
		t.Fatalf("CreateRerun: %v", err)
	}
	got := h.run(rerun.ID)
	if got.Kind != "deploy" || got.ParentRunID == nil || *got.ParentRunID != source.ID || got.AutoStart || got.State != "resolving" {
		t.Errorf("rerun = kind %q parent %v auto-start %v state %q", got.Kind, got.ParentRunID, got.AutoStart, got.State)
	}
	var audited store.AuditEntry
	for _, entry := range h.audits("run.create") {
		if entry.TargetID == fmt.Sprint(rerun.ID) {
			audited = entry
		}
	}
	var detail map[string]any
	if err := json.Unmarshal(audited.Detail, &detail); err != nil || detail["rerun_of"] != float64(source.ID) {
		t.Errorf("audit of the rerun = %s (%v), want rerun_of %d", audited.Detail, err, source.ID)
	}

	if _, err := h.eng.CreateRerun(ctx, h.stranger, source.ID, "web"); !errors.Is(err, ErrForbidden) {
		t.Errorf("CreateRerun by a stranger = %v, want ErrForbidden", err)
	}
}

func TestRerunSelectionRefusals(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	allGood := h.finishedRun("succeeded", []manifest.Service{restoreService("s1")}, nil)
	if _, _, _, err := h.eng.RerunSelection(ctx, h.starter, allGood.ID); !errors.Is(err, ErrWrongState) {
		t.Errorf("rerun of a run with nothing to re-run = %v, want ErrWrongState", err)
	}

	source := h.finishedRun("failed", []manifest.Service{restoreService("s1")}, map[string]string{"s1": "update-service"})
	h.setLeg(h.stepLeg(source.ID, "deploy", "s1").ID, "failed")
	restore, err := h.eng.CreateRestore(ctx, h.starter, source.ID, nil, "web")
	if err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}
	h.endRun(restore.ID, "failed")
	h.setLeg(h.stepLeg(restore.ID, "restore-deploy", "s1").ID, "failed")
	_, _, _, err = h.eng.RerunSelection(ctx, h.starter, restore.ID)
	if !errors.Is(err, ErrWrongState) || !strings.Contains(err.Error(), fmt.Sprintf("restoring run #%d again", source.ID)) {
		t.Errorf("rerun of a restore = %v, want ErrWrongState pointing at the parent run", err)
	}

	notFinal := h.finishedRun("running", []manifest.Service{restoreService("s1")}, nil)
	h.setLeg(h.stepLeg(notFinal.ID, "deploy", "s1").ID, "failed")
	if _, _, _, err := h.eng.RerunSelection(ctx, h.starter, notFinal.ID); !errors.Is(err, ErrWrongState) {
		t.Errorf("rerun of a run that has not finished = %v, want ErrWrongState", err)
	}
}

// finishedRunWithSelection is finishedRun for services s1..s4 created with sel.
func (h *harness) finishedRunWithSelection(sel Selection) store.Run {
	h.t.Helper()
	run, err := h.eng.Create(context.Background(), h.starter, h.sourceID, testEnvironment, sel, "web", false)
	if err != nil {
		h.t.Fatalf("Create: %v", err)
	}
	h.advance(run.ID)
	var services []manifest.Service
	for _, name := range []string{"s1", "s2", "s3", "s4"} {
		services = append(services, restoreService(name))
	}
	m := manifest.Manifest{
		RunID: run.ID, Environment: testEnvironment, Label: testLabel, AWSAccount: testAWSAccount,
		InventoryCommit: "inv-commit", JobCommit: "job-commit", Services: services, Checks: []manifest.Check{},
	}
	m.ID = manifest.Identity(m)
	h.settleResolve(run.ID, "succeeded", m)
	h.advance(run.ID)
	h.endRun(run.ID, "partially_succeeded")
	return h.run(run.ID)
}

// plannedRestore creates a restore of a finished run and lets it resolve, so it is planned.
func (h *harness) plannedRestore() store.Run {
	h.t.Helper()
	source := h.finishedRun("succeeded", []manifest.Service{restoreService("s1")}, map[string]string{"s1": "update-service"})
	restore, err := h.eng.CreateRestore(context.Background(), h.starter, source.ID, nil, "web")
	if err != nil {
		h.t.Fatalf("CreateRestore: %v", err)
	}
	h.advance(restore.ID)
	restored := restoreService("s1")
	restored.TargetTaskDefinition = restoreService("s1").PreviousTaskDefinition
	m := manifest.Manifest{
		RunID: restore.ID, RestoreOf: source.ID, Environment: testEnvironment, Label: testLabel, AWSAccount: testAWSAccount,
		InventoryCommit: "inv-commit", JobCommit: "job-commit", Services: []manifest.Service{restored}, Checks: []manifest.Check{},
	}
	m.ID = manifest.Identity(m)
	h.settleResolve(restore.ID, "succeeded", m)
	h.advance(restore.ID)
	return h.expectState(restore.ID, "planned")
}

func TestStartRestoreNeedsRestorePermission(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()
	restore := h.plannedRestore()

	runOnly, restoreOnly := h.starter, h.starter
	runOnly.Grants = []store.Grant{{Action: "run"}}
	restoreOnly.Grants = []store.Grant{{Action: "restore"}}

	if err := h.eng.Start(ctx, runOnly, restore.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("Start of a restore by a person who may only run = %v, want ErrForbidden", err)
	}
	h.expectState(restore.ID, "planned")
	if err := h.eng.Start(ctx, restoreOnly, restore.ID); err != nil {
		t.Fatalf("Start of a restore by a person who may restore: %v", err)
	}
	if got := h.run(restore.ID).State; got == "planned" {
		t.Errorf("restore is still planned after Start")
	}
}

func TestStartDeployStillNeedsRunPermission(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	run := h.planned(h.starter, "s1")
	restoreOnly := h.starter
	restoreOnly.Grants = []store.Grant{{Action: "restore"}}
	if err := h.eng.Start(context.Background(), restoreOnly, run.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("Start of a deploy by a person who may only restore = %v, want ErrForbidden", err)
	}
}

func TestCreateRestoreRefusesDisconnectedAccount(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	source := h.finishedRun("succeeded", []manifest.Service{restoreService("s1")}, map[string]string{"s1": "update-service"})
	if _, err := h.pool.Exec(context.Background(), `DELETE FROM aws_accounts WHERE account_id = $1`, testAWSAccount); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	_, err := h.eng.CreateRestore(context.Background(), h.starter, source.ID, nil, "web")
	if !errors.Is(err, ErrNotRunnable) || !strings.Contains(err.Error(), testAWSAccount) {
		t.Errorf("CreateRestore on a disconnected account = %v, want ErrNotRunnable naming the account", err)
	}
	var runs int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM runs WHERE kind = 'restore'`).Scan(&runs); err != nil || runs != 0 {
		t.Errorf("restore runs = %d (%v), want none", runs, err)
	}
}
