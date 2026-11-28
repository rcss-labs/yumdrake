package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rcss-labs/yamdrake/internal/audit"
	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// People's actions on runs. Each checks permission, changes state and writes
// its audit entry in one transaction.

// Create plans a new run: it checks the person may run the job against the
// target, builds the legs and locks from the selection, and stores the run in
// resolving with a snapshot of the job. The resolve leg is queued; the run
// becomes planned (or, with autoStart, moves on) when it succeeds.
func (e *Engine) Create(ctx context.Context, p auth.Principal, jobSourceID int64, envName string, sel Selection, via string, autoStart bool) (store.Run, error) {
	return e.create(ctx, p, jobSourceID, envName, sel, via, autoStart, nil)
}

// create is Create for a run that may follow another (a re-run of failed
// services): a non-nil parentRunID is stored on the run and named in its audit entry.
func (e *Engine) create(ctx context.Context, p auth.Principal, jobSourceID int64, envName string, sel Selection, via string, autoStart bool, parentRunID *int64) (store.Run, error) {
	source, err := store.GetJobSource(ctx, e.db, jobSourceID)
	if err != nil {
		return store.Run{}, fmt.Errorf("create run: %w", err)
	}
	if !source.Runnable || len(source.LastGood) == 0 {
		return store.Run{}, fmt.Errorf("create run: %w: the job has problems or was never read", ErrNotRunnable)
	}
	snapshot, ok, err := store.ActiveSnapshot(ctx, e.db)
	if err != nil {
		return store.Run{}, fmt.Errorf("create run: %w", err)
	}
	if !ok {
		return store.Run{}, fmt.Errorf("create run: %w: no inventory is active", ErrNotRunnable)
	}
	var job spec.Job
	if err := json.Unmarshal(source.LastGood, &job); err != nil {
		return store.Run{}, fmt.Errorf("create run: decode job: %w", err)
	}
	var inventory spec.Inventory
	if err := json.Unmarshal(snapshot.Inventory, &inventory); err != nil {
		return store.Run{}, fmt.Errorf("create run: decode inventory: %w", err)
	}

	environment, known := inventory.Environment(envName)
	if known && !p.Can(auth.Run, auth.Target{Label: environment.Label, Environment: envName, FolderID: source.FolderID}) {
		return store.Run{}, fmt.Errorf("create run: %w", ErrForbidden)
	}
	built, problems := Build(job, inventory, envName, sel)
	if len(problems) > 0 { // includes an unknown environment, which has no label to check
		return store.Run{}, &SelectionError{Problems: problems}
	}
	if err := e.checkAccountsConnected(ctx, environment, built.Legs, inventory); err != nil {
		return store.Run{}, err
	}
	selectionJSON, err := json.Marshal(sel)
	if err != nil {
		return store.Run{}, fmt.Errorf("create run: encode selection: %w", err)
	}

	now := e.now()
	legs := built.Legs
	for i := range legs {
		if legs[i].State == legQueued {
			queuedAt := now
			legs[i].QueuedAt = &queuedAt
		}
	}
	run := store.Run{
		Kind: "deploy", JobSourceID: source.ID, JobID: job.ID, JobSnapshot: source.LastGood,
		InventoryCommit: snapshot.Commit, JobCommit: source.LastGoodCommit, InventorySnapshotID: snapshot.ID,
		Environment: envName, Label: environment.Label, AWSAccount: environment.AWSAccount, FolderID: source.FolderID,
		Selection: selectionJSON, LockResources: built.Resources,
		ApprovalRequired: needsApproval(job, environment.Label), AutoStart: autoStart,
		State: "resolving", RequestedBy: p.UserID, Via: via, CreatedAt: now, ParentRunID: parentRunID,
	}

	var created store.Run
	err = e.inTx(ctx, 0, func(tx pgx.Tx, o *outcome) error {
		created, err = store.InsertRun(ctx, tx, run, built.Steps, legs)
		if err != nil {
			return fmt.Errorf("create run: %w", err)
		}
		if err := store.AppendRunLine(ctx, tx, created.ID, "info",
			fmt.Sprintf("Run created by %s for %s in %s; working out the plan.", displayName(p), job.ID, envName), now); err != nil {
			return err
		}
		detail := map[string]any{"job_id": job.ID, "environment": envName, "via": via, "auto_start": autoStart, "approval_required": created.ApprovalRequired}
		if parentRunID != nil {
			detail["rerun_of"] = *parentRunID
		}
		return audit.Record(ctx, tx, &p, now, audit.Event{
			Action: "run.create", TargetType: "run", TargetID: fmt.Sprint(created.ID), Detail: detail,
		})
	})
	if err != nil {
		return store.Run{}, err
	}
	e.publish(ctx, created.ID, outcome{runChanged: true, legsQueued: true})
	return created, nil
}

// checkAccountsConnected refuses, with ErrNotRunnable, a run whose
// environment account, or the account of a selected service's ECR repository,
// is not connected: its legs could never get credentials.
func (e *Engine) checkAccountsConnected(ctx context.Context, environment spec.Environment, legs []store.Leg, inventory spec.Inventory) error {
	needed := map[string]string{environment.AWSAccount: "the environment's account"}
	for _, leg := range legs {
		if leg.Kind != "step" {
			continue
		}
		uri := inventory.Services[leg.Service].Environments[environment.Name].ECRRepository
		if uri == "" {
			continue
		}
		repo, err := awsx.ParseECRRepository(uri, e.aws)
		if err != nil {
			return fmt.Errorf("create run: %w: service %s: %w", ErrNotRunnable, leg.Service, err)
		}
		if _, seen := needed[repo.Account]; !seen {
			needed[repo.Account] = "the ECR repository of " + leg.Service
		}
	}
	accounts := make([]string, 0, len(needed))
	for account := range needed {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	var missing []string
	for _, account := range accounts {
		_, err := store.AWSAccountByAccountID(ctx, e.db, account)
		if errors.Is(err, store.ErrNotFound) {
			missing = append(missing, fmt.Sprintf("%s (%s)", account, needed[account]))
		} else if err != nil {
			return fmt.Errorf("create run: check AWS accounts: %w", err)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("create run: %w: AWS account not connected: %s; connect it under AWS accounts first", ErrNotRunnable, strings.Join(missing, ", "))
	}
	return nil
}

func needsApproval(job spec.Job, label string) bool {
	if job.Approval == nil {
		return false
	}
	for _, l := range job.Approval.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// lockedRunFor locks the run and checks the person may do action to it.
func (e *Engine) lockedRunFor(ctx context.Context, tx pgx.Tx, p auth.Principal, runID int64, action auth.Action) (store.Run, error) {
	run, err := store.LockRun(ctx, tx, runID)
	if err != nil {
		return store.Run{}, err
	}
	if !p.Can(action, targetOf(run)) {
		return store.Run{}, ErrForbidden
	}
	return run, nil
}

// moveTo changes a locked run's state for an action, writing the run-level line.
// For a final state the reason is kept on the run as its end reason and said in the line.
func (e *Engine) moveTo(ctx context.Context, tx pgx.Tx, run store.Run, state, reason, actor string, o *outcome) error {
	now := e.now()
	if err := store.UpdateRunState(ctx, tx, run.ID, state, "", now); err != nil {
		return err
	}
	if state != run.State {
		if err := e.onEnterState(ctx, tx, run, state, now); err != nil {
			return err
		}
	}
	if isFinalRunState(state) && reason != "" {
		if err := store.SetEndReason(ctx, tx, run.ID, reason); err != nil {
			return err
		}
	}
	level, text := transitionLine(run.State, state, reason)
	if actor != "" {
		text = actor + ": " + text
	}
	if err := store.AppendRunLine(ctx, tx, run.ID, level, text, now); err != nil {
		return err
	}
	if isFinalRunState(state) {
		if err := store.ReleaseLocks(ctx, tx, run.ID); err != nil {
			return err
		}
	}
	o.runChanged = true
	return nil
}

func (e *Engine) record(ctx context.Context, tx pgx.Tx, p *auth.Principal, action string, runID int64, detail map[string]any) error {
	return audit.Record(ctx, tx, p, e.now(), audit.Event{Action: action, TargetType: "run", TargetID: fmt.Sprint(runID), Detail: detail})
}

// Start lets a person start a planned run: it waits for approval if the job
// needs one, otherwise it queues and tries to take its locks at once.
func (e *Engine) Start(ctx context.Context, p auth.Principal, runID int64) error {
	return e.inTx(ctx, runID, func(tx pgx.Tx, o *outcome) error {
		run, err := store.LockRun(ctx, tx, runID)
		if err != nil {
			return fmt.Errorf("start run %d: %w", runID, err)
		}
		// A restore goes through the restore permission, a deploy through run.
		needed := auth.Run
		if run.Kind == "restore" {
			needed = auth.Restore
		}
		if !p.Can(needed, targetOf(run)) {
			return fmt.Errorf("start run %d: %w", runID, ErrForbidden)
		}
		if run.State != "planned" {
			return fmt.Errorf("start run %d: %w: it is %s", runID, ErrWrongState, run.State)
		}
		next := "queued"
		if run.ApprovalRequired {
			next = "waiting_approval"
		}
		if err := e.moveTo(ctx, tx, run, next, "", "Started by "+displayName(p), o); err != nil {
			return err
		}
		if err := e.record(ctx, tx, &p, "run.start", runID, map[string]any{"manifest_id": run.ManifestID, "next_state": next}); err != nil {
			return err
		}
		return e.advanceAfterAction(ctx, tx, runID, o)
	})
}

// advanceAfterAction re-reads the locked run and advances it in the same
// transaction as the action, so a person's action and its first consequence
// commit together.
func (e *Engine) advanceAfterAction(ctx context.Context, tx pgx.Tx, runID int64, o *outcome) error {
	run, err := store.GetRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	return e.advanceLocked(ctx, tx, run, o)
}

// Cancel records the request and advances: runs that have not started end at
// once, running ones stop their legs and settle as cancelled.
func (e *Engine) Cancel(ctx context.Context, p auth.Principal, runID int64) error {
	return e.inTx(ctx, runID, func(tx pgx.Tx, o *outcome) error {
		run, err := e.lockedRunFor(ctx, tx, p, runID, auth.Cancel)
		if err != nil {
			return fmt.Errorf("cancel run %d: %w", runID, err)
		}
		if isFinalRunState(run.State) {
			return fmt.Errorf("cancel run %d: %w: it is %s", runID, ErrWrongState, run.State)
		}
		now := e.now()
		if err := store.RequestCancel(ctx, tx, runID, p.UserID, now); err != nil {
			return err
		}
		if err := store.AppendRunLine(ctx, tx, runID, "warn", fmt.Sprintf("Cancel requested by %s while %s.", displayName(p), run.State), now); err != nil {
			return err
		}
		if err := e.record(ctx, tx, &p, "run.cancel", runID, map[string]any{"state": run.State}); err != nil {
			return err
		}
		o.runChanged = true
		return e.advanceAfterAction(ctx, tx, runID, o)
	})
}

// Expire ends a run nobody acted on: a planned run after an hour, a run
// waiting for approval once its approval request has expired. Any other run is left
// alone. It is the system's doing, not a person's.
func (e *Engine) Expire(ctx context.Context, runID int64) error {
	return e.inTx(ctx, runID, func(tx pgx.Tx, o *outcome) error {
		run, err := store.LockRun(ctx, tx, runID)
		if err != nil {
			return fmt.Errorf("expire run %d: %w", runID, err)
		}
		var reason string
		switch run.State {
		case "planned":
			if e.now().Sub(run.StateChangedAt) <= plannedRunLifetime {
				return nil
			}
			reason = "nobody started the plan within " + plannedRunLifetime.String()
		case "waiting_approval":
			due, window, err := e.approvalDue(ctx, tx, run)
			if err != nil || !due {
				return err
			}
			reason = "nobody approved the plan within " + window.String()
			if err := e.expireApproval(ctx, tx, run.ID); err != nil {
				return err
			}
		default:
			return nil
		}
		if err := e.moveTo(ctx, tx, run, "expired", reason, "", o); err != nil {
			return err
		}
		return e.record(ctx, tx, nil, "run.expire", runID, map[string]any{"was": run.State})
	})
}

// Reconcile records the real outcome of an unknown leg, as found by a person.
// It never resumes the run: dependents of the leg stay skipped.
func (e *Engine) Reconcile(ctx context.Context, p auth.Principal, runID, legID int64, outcomeChoice string) error {
	if outcomeChoice != legSucceeded && outcomeChoice != legFailed {
		return fmt.Errorf("reconcile run %d: %w: outcome must be succeeded or failed, not %q", runID, store.ErrInvalid, outcomeChoice)
	}
	return e.inTx(ctx, runID, func(tx pgx.Tx, o *outcome) error {
		run, leg, err := e.unknownLegOf(ctx, tx, p, runID, legID)
		if err != nil {
			return fmt.Errorf("reconcile run %d: %w", runID, err)
		}
		now := e.now()
		if err := store.SetLegState(ctx, tx, legID, outcomeChoice, now); err != nil {
			return legMovedAsWrongState(err)
		}
		if err := store.AppendRunLine(ctx, tx, runID, "info",
			fmt.Sprintf("%s recorded the unknown %s leg for %q as %s.", displayName(p), leg.Kind, leg.Service, outcomeChoice), now); err != nil {
			return err
		}
		if err := e.record(ctx, tx, &p, "run.reconcile", runID, map[string]any{"leg_id": legID, "service": leg.Service, "outcome": outcomeChoice}); err != nil {
			return err
		}
		o.runChanged = true
		return e.advanceLocked(ctx, tx, run, o)
	})
}

// CheckAgain gives an unknown leg one more reconciliation attempt: it is
// queued again with its resume data and operations. The run stays in
// needs_reconciliation until the leg settles.
func (e *Engine) CheckAgain(ctx context.Context, p auth.Principal, runID, legID int64) error {
	return e.inTx(ctx, runID, func(tx pgx.Tx, o *outcome) error {
		run, leg, err := e.unknownLegOf(ctx, tx, p, runID, legID)
		if err != nil {
			return fmt.Errorf("check again run %d: %w", runID, err)
		}
		now := e.now()
		if err := store.SetLegState(ctx, tx, legID, legQueued, now); err != nil {
			return legMovedAsWrongState(err)
		}
		if err := store.AppendRunLine(ctx, tx, runID, "info",
			fmt.Sprintf("%s asked to check the unknown %s leg for %q again.", displayName(p), leg.Kind, leg.Service), now); err != nil {
			return err
		}
		if err := e.record(ctx, tx, &p, "run.check_again", runID, map[string]any{"leg_id": legID, "service": leg.Service}); err != nil {
			return err
		}
		o.runChanged, o.legsQueued = true, true
		return e.advanceLocked(ctx, tx, run, o)
	})
}

// unknownLegOf locks the run and returns it with the named leg, after checking
// the person may restore, the run is in needs_reconciliation and the leg is
// one of its unknown legs.
func (e *Engine) unknownLegOf(ctx context.Context, tx pgx.Tx, p auth.Principal, runID, legID int64) (store.Run, store.Leg, error) {
	run, err := e.lockedRunFor(ctx, tx, p, runID, auth.Restore)
	if err != nil {
		return store.Run{}, store.Leg{}, err
	}
	if run.State != "needs_reconciliation" {
		return store.Run{}, store.Leg{}, fmt.Errorf("%w: it is %s", ErrWrongState, run.State)
	}
	legs, err := store.LegsOfRun(ctx, tx, runID)
	if err != nil {
		return store.Run{}, store.Leg{}, err
	}
	for _, l := range legs {
		if l.ID != legID {
			continue
		}
		if l.State != legUnknown {
			return store.Run{}, store.Leg{}, fmt.Errorf("%w: the leg is %s, not unknown", ErrWrongState, l.State)
		}
		return run, l, nil
	}
	return store.Run{}, store.Leg{}, fmt.Errorf("leg %d is not a leg of this run: %w", legID, store.ErrNotFound)
}

func legMovedAsWrongState(err error) error {
	if errors.Is(err, store.ErrLegMoved) {
		return fmt.Errorf("%w: the leg is no longer unknown", ErrWrongState)
	}
	return err
}

// ReleaseLocks lets an admin free the locks of a run stuck in
// needs_reconciliation without reconciling it. The run fails.
func (e *Engine) ReleaseLocks(ctx context.Context, p auth.Principal, runID int64) error {
	if !p.Admin {
		return fmt.Errorf("release locks of run %d: %w", runID, ErrForbidden)
	}
	return e.inTx(ctx, runID, func(tx pgx.Tx, o *outcome) error {
		run, err := store.LockRun(ctx, tx, runID)
		if err != nil {
			return fmt.Errorf("release locks of run %d: %w", runID, err)
		}
		if run.State != "needs_reconciliation" {
			return fmt.Errorf("release locks of run %d: %w: it is %s", runID, ErrWrongState, run.State)
		}
		legs, err := store.LegsOfRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		for _, l := range legs {
			if legActive(l.State) {
				// A leg being checked again could still resume; the locks must outlive it.
				return fmt.Errorf("release locks of run %d: %w: a leg is still %s", runID, ErrWrongState, l.State)
			}
		}
		reason := fmt.Sprintf("locks released by %s without reconciliation", displayName(p))
		held, err := store.LocksOfRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if err := e.moveTo(ctx, tx, run, "failed", reason, "", o); err != nil {
			return err
		}
		return e.record(ctx, tx, &p, "run.release_locks", runID, map[string]any{"released": held})
	})
}
