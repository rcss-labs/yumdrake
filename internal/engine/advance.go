package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/wake"
)

var (
	// ErrForbidden means the person may not do this to this run.
	ErrForbidden = errors.New("not allowed")
	// ErrNotRunnable means the job has problems or no inventory is active.
	ErrNotRunnable = errors.New("job is not runnable")
	// ErrWrongState means the run (or leg) is not in a state that allows the action.
	ErrWrongState = errors.New("run is not in a state that allows this")
)

// SelectionError reports every problem with a selection (Build's problems).
type SelectionError struct{ Problems spec.Problems }

func (e *SelectionError) Error() string { return e.Problems.Error() }

const (
	// plannedRunLifetime is how long a planned run waits for a person to start it.
	plannedRunLifetime = time.Hour
	// defaultApprovalExpiry applies when the job's approval block sets none.
	defaultApprovalExpiry = 24 * time.Hour

	defaultLimitPerEnvironment = 10
	defaultLimitPerAccount     = 20
)

// Publisher announces that something changed, so sleepers need not wait for
// their next poll. The id is the run the news is about.
type Publisher interface {
	Publish(ctx context.Context, topic string, id int64)
}

// Deps is what an Engine needs.
type Deps struct {
	DB     store.DBTX // a pool; every action opens its own transaction
	Limits Limits     // zero value means the defaults, 10 per environment and 20 per account
	Now    func() time.Time
	Log    *slog.Logger
	// Publisher is optional: nil means no wake-ups.
	Publisher Publisher
	// AWS carries the deployment's AWS settings; an endpoint override lets
	// Create read the ECR repository URIs of a local emulator.
	AWS awsx.Options
	// PublicURL is the address notifications link to (config.PublicURL).
	PublicURL string
}

// Engine creates runs, advances them under a run row lock and carries out
// people's actions on them.
type Engine struct {
	db     store.DBTX
	limits Limits
	now    func() time.Time
	log    *slog.Logger
	pub    Publisher
	aws    awsx.Options
	// publicURL is the base of the run links in notifications.
	publicURL string
	// lastPruned is when the loop last deleted old runner rows and system
	// events. Only the loop goroutine touches it.
	lastPruned time.Time
}

// New builds an Engine, filling defaults for the clock, logger and limits.
func New(deps Deps) *Engine {
	e := &Engine{db: deps.DB, limits: deps.Limits, now: deps.Now, log: deps.Log, pub: deps.Publisher, aws: deps.AWS, publicURL: deps.PublicURL}
	if e.limits == (Limits{}) {
		e.limits = Limits{PerEnvironment: defaultLimitPerEnvironment, PerAccount: defaultLimitPerAccount}
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	return e
}

// ApprovalExpiry is how long a run may wait for approval: the job snapshot's
// approval.expires, 24 hours when the job does not say.
func ApprovalExpiry(r store.Run) time.Duration {
	var job spec.Job
	if err := json.Unmarshal(r.JobSnapshot, &job); err == nil && job.Approval != nil && job.Approval.Expires > 0 {
		return job.Approval.Expires
	}
	return defaultApprovalExpiry
}

// outcome collects what to announce once a transaction has committed.
type outcome struct {
	runChanged, legsQueued bool
}

func (e *Engine) publish(ctx context.Context, runID int64, o outcome) {
	if e.pub == nil {
		return
	}
	if o.legsQueued {
		e.pub.Publish(ctx, wake.TopicLegs, runID)
	}
	if o.runChanged {
		e.pub.Publish(ctx, wake.TopicRuns, runID)
	}
}

// inTx runs fn in a transaction and publishes what it reports after commit.
func (e *Engine) inTx(ctx context.Context, runID int64, fn func(tx pgx.Tx, o *outcome) error) error {
	tx, err := e.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var o outcome
	if err := fn(tx, &o); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	e.publish(ctx, runID, o)
	return nil
}

func targetOf(r store.Run) auth.Target {
	return auth.Target{Label: r.Label, Environment: r.Environment, FolderID: r.FolderID}
}

func displayName(p auth.Principal) string {
	if p.Name != "" {
		return p.Name
	}
	return p.Email
}

// Advance moves a run as far as one decision takes it: in one transaction it
// locks the run row, looks at its legs, decides and applies. Any number of
// orchestrators may call it for one run at once; the row lock makes them take
// turns, and each sees the result of the one before. Calling it on a final run
// does nothing.
func (e *Engine) Advance(ctx context.Context, runID int64) error {
	return e.inTx(ctx, runID, func(tx pgx.Tx, o *outcome) error {
		run, err := store.LockRun(ctx, tx, runID)
		if err != nil {
			return fmt.Errorf("advance run %d: %w", runID, err)
		}
		return e.advanceLocked(ctx, tx, run, o)
	})
}

// advanceLocked decides and applies for a run whose row the caller has locked.
func (e *Engine) advanceLocked(ctx context.Context, tx pgx.Tx, run store.Run, o *outcome) error {
	if isFinalRunState(run.State) {
		return nil
	}
	steps, err := store.StepsOfRun(ctx, tx, run.ID)
	if err != nil {
		return err
	}
	legs, err := store.LegsOfRun(ctx, tx, run.ID)
	if err != nil {
		return err
	}
	locks, err := store.LocksOfRun(ctx, tx, run.ID)
	if err != nil {
		return err
	}
	intended, err := store.LegsWithIntendedOperation(ctx, tx, run.ID)
	if err != nil {
		return err
	}
	view := View{Run: run, Steps: steps, Legs: legs, LocksHeld: len(locks) > 0, IntendedLegs: intended, Limits: e.limits}
	if run.State == "running" {
		view.RunningInEnv, view.RunningInAccount, err = store.ActiveLegsOfOtherRuns(ctx, tx, run.ID, run.Environment, run.AWSAccount)
		if err != nil {
			return err
		}
	}
	if run.State == "queued" {
		if err := e.checkQueuedRun(ctx, tx, &view); err != nil {
			return err
		}
	}
	return e.apply(ctx, tx, run, legs, Decide(view), o)
}

// apply carries out a decision. The caller holds the run row lock.
func (e *Engine) apply(ctx context.Context, tx pgx.Tx, run store.Run, legs []store.Leg, d Decision, o *outcome) error {
	now := e.now()

	if d.TryAcquireLocks {
		held, err := store.AcquireLocks(ctx, tx, run.ID, run.LockResources, now)
		if err != nil {
			return err
		}
		if len(held) > 0 {
			// Locks are all or nothing: nothing was taken. Stay queued and say who we wait for.
			reason := fmt.Sprintf("waiting for run #%d", lowestHolder(held))
			if reason == run.WaitReason {
				return nil
			}
			if err := store.UpdateRunState(ctx, tx, run.ID, run.State, reason, now); err != nil {
				return ignoreFinal(err)
			}
			o.runChanged = true
			return store.AppendRunLine(ctx, tx, run.ID, "info",
				fmt.Sprintf("Waiting: %s holds locks this run needs.", reason[len("waiting for "):]), now)
		}
		if err := store.AppendRunLine(ctx, tx, run.ID, "info",
			fmt.Sprintf("Took all %d locks this run needs.", len(run.LockResources)), now); err != nil {
			return err
		}
	}

	// Every multi-leg write goes in ascending id order, so two transactions
	// touching the same legs (an Advance and a sweep) never wait on each other
	// in a cycle.
	for _, id := range sortedIDs(d.QueueLegs) {
		if err := store.SetLegState(ctx, tx, id, legQueued, now); err != nil {
			if errors.Is(err, store.ErrLegMoved) {
				continue
			}
			return err
		}
		o.legsQueued = true
	}
	for _, id := range sortedIDs(d.SkipLegs) {
		if err := store.SetLegState(ctx, tx, id, legSkipped, now); err != nil && !errors.Is(err, store.ErrLegMoved) {
			return err
		}
	}
	for _, id := range sortedIDs(d.CancelLegs) {
		err := store.SetLegState(ctx, tx, id, legCancelled, now)
		if errors.Is(err, store.ErrLegMoved) {
			// A runner claimed the leg since we read it: ask it to stop instead.
			err = store.FlagLegCancel(ctx, tx, id)
		}
		if err != nil {
			return err
		}
	}
	for _, id := range sortedIDs(d.MarkCancelRequested) {
		if err := store.FlagLegCancel(ctx, tx, id); err != nil {
			return err
		}
	}
	if d.CreateRevalidateLeg {
		if _, err := store.InsertLeg(ctx, tx, store.Leg{RunID: run.ID, Kind: "revalidate", State: legQueued, QueuedAt: &now}); err != nil {
			return err
		}
		o.legsQueued = true
	}

	if d.NewState != "" || d.WaitReason != "" || d.ClearWaitReason {
		// A run leaves resolving only with its plan stored. A result that cannot
		// be used ends the run instead of leaving it stuck.
		if run.State == "resolving" && d.NewState != "" {
			problem, err := copyManifest(ctx, tx, run, legs)
			if err != nil {
				return err
			}
			if problem != "" {
				d.NewState, d.Reason = "failed", problem
			}
		}
		newState, wait := run.State, d.WaitReason
		entersFinal := d.NewState != "" && isFinalRunState(d.NewState)
		if d.NewState != "" {
			newState = d.NewState
		}
		if entersFinal {
			wait = "" // a final run waits for nothing; its end reason says why it ended
		}
		if err := store.UpdateRunState(ctx, tx, run.ID, newState, wait, now); err != nil {
			return ignoreFinal(err)
		}
		if entersFinal {
			reason := d.Reason
			if reason == "" {
				reason = defaultEndReason(newState)
			}
			if err := store.SetEndReason(ctx, tx, run.ID, reason); err != nil {
				return err
			}
		}
		o.runChanged = true
		if newState != run.State {
			if err := e.onEnterState(ctx, tx, run, newState, now); err != nil {
				return err
			}
			level, text := transitionLine(run.State, newState, d.Reason)
			if err := store.AppendRunLine(ctx, tx, run.ID, level, text, now); err != nil {
				return err
			}
		}
	}

	if d.FailUnauthorized {
		if err := e.recordUnauthorizedEnd(ctx, tx, run, d.Reason); err != nil {
			return err
		}
	}
	if d.ReleaseLocks || (d.NewState != "" && isFinalRunState(d.NewState)) {
		if err := store.ReleaseLocks(ctx, tx, run.ID); err != nil {
			return err
		}
	}
	return nil
}

// copyManifest stores the resolve leg's result as the run's manifest when the
// leg settled, failed ones too: a failed plan is shown with its failed checks.
// The id is the manifest's own identity (a hash over the pinned inputs, see
// manifest.Identity), recomputed from the result rather than taken from its id
// field on trust: approvals bind to this value, so it must describe exactly
// what is stored.
//
// The manifest must name this run and its AWS account, and a succeeded leg's
// must have services; anything else is not this run's plan.
//
// A non-empty problem means a succeeded leg left no usable plan; the caller
// ends the run with it as the reason. On a failed leg an unusable result is
// only a diagnostic and is skipped.
func copyManifest(ctx context.Context, tx pgx.Tx, run store.Run, legs []store.Leg) (problem string, err error) {
	resolve, ok := latestLeg(legs, "resolve")
	if !ok || (resolve.State != legSucceeded && resolve.State != legFailed) {
		return "", nil
	}
	succeeded := resolve.State == legSucceeded
	if len(resolve.Result) == 0 {
		if succeeded {
			return "the resolve step reported no plan; nothing was changed", nil
		}
		return "", nil
	}
	var m manifest.Manifest
	if err := json.Unmarshal(resolve.Result, &m); err != nil {
		if succeeded {
			return "the resolve step's plan could not be read; nothing was changed", nil
		}
		return "", nil
	}
	identity := manifest.Identity(m)
	if m.ID != "" && m.ID != identity {
		if succeeded {
			return "the resolve step's plan does not match its identity; nothing was changed", nil
		}
		return "", nil
	}
	if m.RunID != run.ID || m.AWSAccount != run.AWSAccount || (succeeded && len(m.Services) == 0) {
		// Approvals and the writing steps trust this plan to be this run's.
		if succeeded {
			return "the resolve step's plan is not for this run; nothing was changed", nil
		}
		return "", nil
	}
	return "", store.SetManifest(ctx, tx, run.ID, resolve.Result, identity)
}

func sortedIDs(ids []int64) []int64 {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	return sorted
}

// defaultEndReason is the plain-words reason for a final state Decide gave no
// reason for.
func defaultEndReason(state string) string {
	switch state {
	case "invalidated":
		return "the targets changed since the plan was made, so nothing was run"
	case "cancelled":
		return "cancelled at a person's request"
	case "partially_succeeded":
		return "some steps did not succeed"
	case "failed":
		return "no step succeeded"
	}
	return ""
}

func lowestHolder(held map[string]int64) int64 {
	var lowest int64
	for _, id := range held {
		if lowest == 0 || id < lowest {
			lowest = id
		}
	}
	return lowest
}

// ignoreFinal treats "the run is already final" as success: nothing to change.
func ignoreFinal(err error) error {
	if errors.Is(err, store.ErrRunFinal) {
		return nil
	}
	return err
}

// transitionLine words a state change for the run's log.
func transitionLine(from, to, reason string) (level, text string) {
	text = fmt.Sprintf("Run moved from %s to %s: ", from, to)
	level = "info"
	switch to {
	case "planned":
		text += "the plan is ready and waits for a person to start it."
	case "waiting_approval":
		text += "the plan is ready and needs an approval before it can run. No locks are held."
	case "queued":
		text += "waiting for its turn to take locks."
	case "running":
		text += "checking that the plan still holds, then the steps run."
	case "cancelling":
		text += "cancel requested; waiting for the legs in flight to stop."
		level = "warn"
	case "needs_reconciliation":
		text += "at least one leg's outcome is unknown. Locks stay held until a person reconciles it."
		level = "warn"
	case "succeeded":
		text += "every step succeeded. Locks released."
		level = "ok"
	case "partially_succeeded":
		text += "some steps succeeded and some did not. Locks released."
		level = "warn"
	case "failed":
		text += "the run failed. Locks released."
		level = "error"
	case "cancelled":
		text += "the run was cancelled. Locks released."
		level = "warn"
	case "rejected":
		text += "an approver rejected the plan."
		level = "warn"
	case "expired":
		text += "nobody acted in time."
		level = "warn"
	case "invalidated":
		text += "the targets changed since the plan was made, so nothing was run. Locks released. Plan again."
		level = "error"
	default:
		text += "state changed."
	}
	if reason != "" {
		text += " " + reason + "."
	}
	return level, text
}
