package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rcss-labs/yamdrake/internal/audit"
	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// restoreGroup is the selection group, and the step group, of a restore run.
const restoreGroup = "restore"

// Operation names that mean a leg changed something in the environment.
var changingOperations = []string{"put-config", "update-service"}

// CreateRestore plans a restore of what a finished run changed: each chosen
// service goes back to the task definition and the config copies the source
// run recorded as previous. Empty services means every service the source run
// changed. The new run is never started here: a person reads its plan first.
// It needs the restore permission on the source run's target.
func (e *Engine) CreateRestore(ctx context.Context, p auth.Principal, sourceRunID int64, services []string, via string) (store.Run, error) {
	source, err := store.GetRun(ctx, e.db, sourceRunID)
	if err != nil {
		return store.Run{}, fmt.Errorf("restore run %d: %w", sourceRunID, err)
	}
	if !p.Can(auth.Restore, targetOf(source)) {
		return store.Run{}, fmt.Errorf("restore run %d: %w", sourceRunID, ErrForbidden)
	}
	if !isFinalRunState(source.State) {
		return store.Run{}, fmt.Errorf("restore run %d: %w: it is %s; restore it when it has finished", sourceRunID, ErrWrongState, source.State)
	}
	if len(source.Manifest) == 0 {
		return store.Run{}, fmt.Errorf("restore run %d: %w: it has no plan, so there is nothing to restore", sourceRunID, ErrWrongState)
	}
	var sourceManifest manifest.Manifest
	if err := json.Unmarshal(source.Manifest, &sourceManifest); err != nil {
		return store.Run{}, fmt.Errorf("restore run %d: decode its plan: %w", sourceRunID, err)
	}
	changed, err := store.ServicesWithDoneOperation(ctx, e.db, sourceRunID, changingOperations...)
	if err != nil {
		return store.Run{}, fmt.Errorf("restore run %d: %w", sourceRunID, err)
	}
	if len(changed) == 0 {
		return store.Run{}, fmt.Errorf("restore run %d: %w: nothing in run #%d can be restored", sourceRunID, ErrWrongState, sourceRunID)
	}
	chosen := changed
	if len(services) > 0 {
		chosen = slices.Clone(services)
		sort.Strings(chosen)
		chosen = slices.Compact(chosen)
		for _, name := range chosen {
			if !slices.Contains(changed, name) {
				return store.Run{}, fmt.Errorf("restore run %d: %w: the run did not change service %q", sourceRunID, store.ErrInvalid, name)
			}
		}
	}

	byName := map[string]manifest.Service{}
	for _, s := range sourceManifest.Services {
		byName[s.Name] = s
	}
	versions := map[string]string{}
	resources := map[string]bool{}
	for _, name := range chosen {
		service, ok := byName[name]
		if !ok {
			return store.Run{}, fmt.Errorf("restore run %d: %w: the plan of run #%d has no service %q", sourceRunID, store.ErrInvalid, sourceRunID, name)
		}
		versions[name] = service.PreviousVersion
		if service.ECSService != "" {
			resources[service.ECSService] = true
		}
		for _, item := range service.Config {
			resources[item.Destination] = true
		}
	}

	// A restore touches no ECR repository, so only the environment's account must be connected.
	environment := spec.Environment{Name: source.Environment, Label: source.Label, AWSAccount: source.AWSAccount}
	if err := e.checkAccountsConnected(ctx, environment, nil, spec.Inventory{}); err != nil {
		return store.Run{}, err
	}

	var job spec.Job
	if err := json.Unmarshal(source.JobSnapshot, &job); err != nil {
		return store.Run{}, fmt.Errorf("restore run %d: decode its job: %w", sourceRunID, err)
	}
	selectionJSON, err := json.Marshal(Selection{restoreGroup: {Services: versions}})
	if err != nil {
		return store.Run{}, fmt.Errorf("restore run %d: encode selection: %w", sourceRunID, err)
	}

	now := e.now()
	steps, legs := restorePlan(chosen, versions, now)
	parent := source.ID
	run := store.Run{
		Kind: "restore", ParentRunID: &parent,
		JobSourceID: source.JobSourceID, JobID: source.JobID, JobSnapshot: source.JobSnapshot,
		InventoryCommit: source.InventoryCommit, JobCommit: source.JobCommit, InventorySnapshotID: source.InventorySnapshotID,
		Environment: source.Environment, Label: source.Label, AWSAccount: source.AWSAccount, FolderID: source.FolderID,
		Selection: selectionJSON, LockResources: sortedKeys(resources),
		ApprovalRequired: needsApproval(job, source.Label), AutoStart: false,
		State: "resolving", RequestedBy: p.UserID, Via: via, CreatedAt: now,
	}

	var created store.Run
	err = e.inTx(ctx, 0, func(tx pgx.Tx, o *outcome) error {
		created, err = store.InsertRun(ctx, tx, run, steps, legs)
		if err != nil {
			return fmt.Errorf("restore run %d: %w", sourceRunID, err)
		}
		if err := store.AppendRunLine(ctx, tx, created.ID, "info",
			fmt.Sprintf("Restore of run #%d created by %s for %s; working out the plan.", sourceRunID, displayName(p), strings.Join(chosen, ", ")), now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, &p, now, audit.Event{
			Action: "run.restore", TargetType: "run", TargetID: fmt.Sprint(created.ID),
			Detail: map[string]any{"source_run_id": sourceRunID, "services": chosen, "via": via},
		})
	})
	if err != nil {
		return store.Run{}, err
	}
	e.publish(ctx, created.ID, outcome{runChanged: true, legsQueued: true})
	return created, nil
}

// restorePlan is the two generated steps of a restore and their legs: the
// queued resolve leg, then a pending leg of each step for each service. The
// steps set no MaxParallel (0 means no limit, as decide.go reads it) and no
// Timeout (0, as for a job step that sets none).
func restorePlan(services []string, versions map[string]string, now time.Time) ([]store.RunStep, []store.Leg) {
	needs := func(list []spec.Need) []byte {
		encoded, err := json.Marshal(list)
		if err != nil { // a slice of plain structs cannot fail to encode
			panic(fmt.Sprintf("encode needs: %v", err))
		}
		return encoded
	}
	steps := []store.RunStep{
		{Name: "restore-config", Use: "copy-config", GroupID: restoreGroup, Position: 0, Needs: needs([]spec.Need{})},
		{Name: "restore-deploy", Use: "ecs-deploy", GroupID: restoreGroup, Position: 1,
			Needs: needs([]spec.Need{{Step: "restore-config", Scope: "service"}})},
	}
	queuedAt := now
	legs := []store.Leg{{Kind: "resolve", State: legQueued, QueuedAt: &queuedAt}}
	for index := range steps {
		stepIndex := int64(index)
		for _, name := range services {
			legs = append(legs, store.Leg{StepID: &stepIndex, Kind: "step", Service: name, Version: versions[name], State: legPending})
		}
	}
	return steps, legs
}

// RerunSelection works out what re-running a finished deploy's failed
// services means: the job source and environment of the run, and a selection
// of its services that failed, were skipped or were cancelled, each at the
// version it had. The caller creates a normal run from it.
func (e *Engine) RerunSelection(ctx context.Context, p auth.Principal, sourceRunID int64) (jobSourceID int64, envName string, sel Selection, err error) {
	source, err := store.GetRun(ctx, e.db, sourceRunID)
	if err != nil {
		return 0, "", nil, fmt.Errorf("re-run failed services of run %d: %w", sourceRunID, err)
	}
	if !p.Can(auth.Run, targetOf(source)) {
		return 0, "", nil, fmt.Errorf("re-run failed services of run %d: %w", sourceRunID, ErrForbidden)
	}
	if !isFinalRunState(source.State) {
		return 0, "", nil, fmt.Errorf("re-run failed services of run %d: %w: it is %s", sourceRunID, ErrWrongState, source.State)
	}
	if source.Kind != "deploy" {
		parent := int64(0)
		if source.ParentRunID != nil {
			parent = *source.ParentRunID
		}
		return 0, "", nil, fmt.Errorf("re-run failed services of run %d: %w: re-run a restore by restoring run #%d again", sourceRunID, ErrWrongState, parent)
	}
	legs, err := store.LegsOfRun(ctx, e.db, sourceRunID)
	if err != nil {
		return 0, "", nil, fmt.Errorf("re-run failed services of run %d: %w", sourceRunID, err)
	}
	versions := map[string]string{} // the version of each service with a leg that did not succeed
	for _, leg := range legs {
		if leg.Kind == "step" && (leg.State == legFailed || leg.State == legSkipped || leg.State == legCancelled) {
			versions[leg.Service] = leg.Version
		}
	}
	if len(versions) == 0 {
		return 0, "", nil, fmt.Errorf("re-run failed services of run %d: %w: no service of run #%d failed, was skipped or was cancelled", sourceRunID, ErrWrongState, sourceRunID)
	}
	var original Selection
	if err := json.Unmarshal(source.Selection, &original); err != nil {
		return 0, "", nil, fmt.Errorf("re-run failed services of run %d: decode its selection: %w", sourceRunID, err)
	}
	sel = Selection{}
	for groupID, choice := range original {
		kept := map[string]string{}
		for name := range choice.Services {
			if version, again := versions[name]; again {
				kept[name] = version
			}
		}
		if len(kept) > 0 {
			sel[groupID] = GroupSelection{Version: choice.Version, Services: kept}
		}
	}
	return source.JobSourceID, source.Environment, sel, nil
}

// CreateRerun creates the normal run RerunSelection describes, as a child of
// the source run. It is planned like any run and never starts by itself.
func (e *Engine) CreateRerun(ctx context.Context, p auth.Principal, sourceRunID int64, via string) (store.Run, error) {
	jobSourceID, envName, sel, err := e.RerunSelection(ctx, p, sourceRunID)
	if err != nil {
		return store.Run{}, err
	}
	return e.create(ctx, p, jobSourceID, envName, sel, via, false, &sourceRunID)
}
