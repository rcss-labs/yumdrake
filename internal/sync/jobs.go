// Package sync polls GitHub and keeps the stored inventory and job definitions
// current. The inventory is activated one commit at a time as a whole, and each
// job keeps its last good version when a newer commit is invalid.
//
// Two kinds of failure are kept apart on purpose. A source error (network,
// 5xx, rate limit, missing connection, unreadable branch) says nothing about
// the YAML, so it changes no stored row and is returned. A definitions error
// (a file that does not parse or validate) is a result: it is stored as
// problems next to the commit that caused it.
package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	gosync "sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// Source reads a repository. It is satisfied by *github.Client.
type Source interface {
	HeadCommit(ctx context.Context, repo, branch string) (string, error)
	ListFiles(ctx context.Context, repo, commit, dir string) ([]string, error)
	ReadFile(ctx context.Context, repo, commit, path string) ([]byte, error)
}

// SourceFor looks up (and decrypts) the connection for a repository owner.
type SourceFor func(ctx context.Context, owner string) (Source, error)

// Engine syncs the inventory and the job sources.
type Engine struct {
	pool    *pgxpool.Pool
	sources SourceFor
	log     *slog.Logger
	now     func() time.Time
	// local serialises callers inside this process (a 1-slot semaphore) before
	// any of them takes a pool connection, so at most one connection per
	// process waits on or holds the advisory lock; see withLock.
	local chan struct{}
	// backfilled holds the rows whose missing file text was already attempted
	// in this process. A row that cannot be filled (old commit gone, files
	// changed) would otherwise be read from GitHub again on every poll.
	backfillMu gosync.Mutex
	backfilled map[string]bool
}

// firstBackfill reports true once per key for the life of the engine.
func (e *Engine) firstBackfill(key string) bool {
	e.backfillMu.Lock()
	defer e.backfillMu.Unlock()
	if e.backfilled[key] {
		return false
	}
	e.backfilled[key] = true
	return true
}

// NewEngine builds an engine. now supplies every stored timestamp.
func NewEngine(pool *pgxpool.Pool, sources SourceFor, log *slog.Logger, now func() time.Time) *Engine {
	return &Engine{pool: pool, sources: sources, log: log, now: now, local: make(chan struct{}, 1), backfilled: map[string]bool{}}
}

const noInventoryMessage = "no active inventory yet"

// SyncJob brings one job source up to date with the head of its branch. It
// serialises with every other sync (see withLock).
func (e *Engine) SyncJob(ctx context.Context, jobSourceID int64) error {
	return e.withLock(ctx, func(ctx context.Context) error { return e.syncJob(ctx, jobSourceID) })
}

// syncJob is SyncJob without the lock; the caller must hold it.
func (e *Engine) syncJob(ctx context.Context, jobSourceID int64) error {
	job, err := store.GetJobSource(ctx, e.pool, jobSourceID)
	if err != nil {
		return err
	}

	snapshot, haveInventory, err := store.ActiveSnapshot(ctx, e.pool)
	if err != nil {
		return err
	}
	if !haveInventory {
		// The problem is stored without a commit so that every round reads the
		// file again until an inventory exists.
		problems := spec.Problems{{File: job.Path, Message: noInventoryMessage}}
		return e.saveJobProblems(ctx, e.pool, job, problems, "", false, e.now())
	}
	var inventory spec.Inventory
	if err := json.Unmarshal(snapshot.Inventory, &inventory); err != nil {
		return fmt.Errorf("decode active inventory: %w", err)
	}

	// A source error stores only the failure message; definitions stay as they
	// are. It is cleared once GitHub has answered again.
	fail := func(what string, err error, attrs ...any) error {
		e.recordSyncError(ctx, what, err, func(message string, at time.Time) error {
			return store.SetJobSyncError(ctx, e.pool, job.ID, message, at)
		})
		return e.sourceError(what, err, append(attrs, "job_source_id", job.ID, "repo", job.Repo)...)
	}
	// SourceFor errors are stored and shown to users as they are: the
	// implementation must return credential-free errors.
	source, err := e.sourceForRepo(ctx, job.Repo)
	if err != nil {
		return fail("job source", err)
	}
	head, err := source.HeadCommit(ctx, job.Repo, job.Branch)
	if err != nil {
		return fail("read job branch head", err, "branch", job.Branch)
	}
	if err := store.ClearJobSyncError(ctx, e.pool, job.ID); err != nil {
		return err
	}

	// A row written before the text was kept gets it from the last good commit
	// (never from the head, which may be a rejected one).
	if job.LastGood != nil && job.LastGoodSource == "" && job.LastGoodCommit != "" &&
		e.firstBackfill(fmt.Sprintf("job:%d:%s", job.ID, job.LastGoodCommit)) {
		// Best effort: a failure is logged and the sync goes on.
		text, err := source.ReadFile(ctx, job.Repo, job.LastGoodCommit, job.Path)
		if err == nil {
			err = store.FillJobSource(ctx, e.pool, job.ID, job.LastGoodCommit, string(text))
		}
		if err != nil {
			e.log.Warn("skipped filling job file text", "job_source_id", job.ID, "commit", job.LastGoodCommit, "error", err.Error())
		}
	}

	// Unchanged means: nothing new at the head and the file was last checked
	// against the inventory that is active now. A job re-validated at
	// activation, or read since, has checked_at at or after ActivatedAt.
	if head == newestSeenCommit(job) && job.CheckedAt != nil && !job.CheckedAt.Before(snapshot.ActivatedAt) {
		return store.TouchJobChecked(ctx, e.pool, job.ID, e.now())
	}

	data, err := source.ReadFile(ctx, job.Repo, head, job.Path)
	if err != nil {
		return fail("read job file", err, "commit", head)
	}
	at := e.now() // taken after the reads, just before the write
	parsed, problems := spec.ParseJob(job.Path, data)
	if len(problems) == 0 {
		problems = spec.ValidateJob(parsed, inventory)
	}
	if len(problems) > 0 {
		// Keep LastGood; the job stays runnable only if it already was.
		return e.saveJobProblems(ctx, e.pool, job, problems, head, job.Runnable && job.LastGood != nil, at)
	}

	definition, err := json.Marshal(parsed)
	if err != nil {
		return fmt.Errorf("encode job %q: %w", parsed.ID, err)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin job save: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = store.SaveJobGood(ctx, tx, job.ID, definition, string(data), parsed.ID, parsed.Title, head, at)
	if err == nil {
		err = store.SaveJobProblems(ctx, tx, job.ID, nil, head, true, at)
	}
	if errors.Is(err, store.ErrDuplicate) {
		_ = tx.Rollback(ctx)
		duplicate := spec.Problems{{File: job.Path, Line: 0, Message: fmt.Sprintf("job id %q is already used by another job source", parsed.ID)}}
		return e.saveJobProblems(ctx, e.pool, job, duplicate, head, job.Runnable && job.LastGood != nil, at)
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit job save: %w", err)
	}
	return nil
}

// newestSeenCommit is the newest commit read for the job: the one the stored
// problems belong to, or else the last good one.
func newestSeenCommit(job store.JobSource) string {
	if job.Problems != nil {
		return job.ProblemsCommit
	}
	return job.LastGoodCommit
}

func (e *Engine) saveJobProblems(ctx context.Context, db store.DBTX, job store.JobSource, problems spec.Problems,
	commit string, runnable bool, at time.Time) error {
	encoded, err := json.Marshal(problems.Sorted())
	if err != nil {
		return fmt.Errorf("encode problems: %w", err)
	}
	return store.SaveJobProblems(ctx, db, job.ID, encoded, commit, runnable, at)
}

// revalidateJobs checks every job's last good definition against a newly
// activated inventory. It runs inside the activation transaction.
//
// Rule: a job whose newest commit is its last good one (no problems, or
// problems recorded for that same commit) is re-validated: valid means
// runnable with problems cleared, invalid means not runnable with the reasons
// and LastGood kept. A job whose newest commit is a different, rejected one
// keeps those problems untouched, because they describe what its author must
// fix; only its runnable flag follows the last good definition. A job that has
// never had a good read has nothing to validate.
func (e *Engine) revalidateJobs(ctx context.Context, tx store.DBTX, inventory spec.Inventory, at time.Time) error {
	jobs, err := store.ListJobSources(ctx, tx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.LastGood == nil {
			continue
		}
		var lastGood spec.Job
		if err := json.Unmarshal(job.LastGood, &lastGood); err != nil {
			return fmt.Errorf("decode last good of job source %d: %w", job.ID, err)
		}
		problems := spec.ValidateJob(lastGood, inventory)
		ownProblems := job.Problems != nil && job.ProblemsCommit != job.LastGoodCommit
		switch {
		case ownProblems:
			err = store.SetJobRunnable(ctx, tx, job.ID, len(problems) == 0)
		case len(problems) == 0:
			err = store.SaveJobProblems(ctx, tx, job.ID, nil, job.LastGoodCommit, true, at)
		default:
			err = e.saveJobProblems(ctx, tx, job, problems, job.LastGoodCommit, false, at)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// sourceForRepo resolves the connection for the owner part of "owner/name".
func (e *Engine) sourceForRepo(ctx context.Context, repo string) (Source, error) {
	owner, _, _ := strings.Cut(repo, "/")
	return e.sources(ctx, owner)
}

// recordSyncError stores a credential-free description of a source error. The
// message is the failing step plus the error text; the GitHub client's errors
// carry only method, path and status, never a token. A cancelled context is not
// a source failure and is not recorded. Failing to record is logged only.
func (e *Engine) recordSyncError(ctx context.Context, what string, err error, save func(message string, at time.Time) error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	message := truncateMessage("source error: "+what+": "+err.Error(), maxSyncErrorLength)
	if serr := save(message, e.now()); serr != nil {
		e.log.Warn("could not record sync error", "error", serr.Error())
	}
	// A system event counts the failure; failing to record it changes nothing.
	if serr := store.RecordEvent(ctx, e.pool, "sync_failed", e.now()); serr != nil {
		e.log.Warn("could not record sync failure event", "error", serr.Error())
	}
}

const maxSyncErrorLength = 500

// truncateMessage cuts message to at most max bytes without leaving a partial
// UTF-8 character, which PostgreSQL would reject.
func truncateMessage(message string, max int) string {
	if len(message) <= max {
		return message
	}
	return strings.ToValidUTF8(message[:max], "")
}

// ErrSource marks a failure to reach or read the source (network, GitHub
// status, missing connection). The failure is recorded as sync_error; any other
// error from a Sync method is an internal failure.
var ErrSource = errors.New("source error")

// sourceFailure wraps a source error so it matches ErrSource while keeping its
// message and its own chain (for example *github.RateLimitError).
type sourceFailure struct {
	what string
	err  error
}

func (f *sourceFailure) Error() string        { return f.what + ": " + f.err.Error() }
func (f *sourceFailure) Unwrap() error        { return f.err }
func (f *sourceFailure) Is(target error) bool { return target == ErrSource }

// sourceError logs a failure to reach the source at warn level and returns it
// wrapped. Nothing is stored for it.
func (e *Engine) sourceError(what string, err error, attrs ...any) error {
	e.log.Warn("sync source error", append(attrs, "step", what, "error", err.Error())...)
	return &sourceFailure{what: what, err: err}
}
