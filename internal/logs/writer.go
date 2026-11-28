// Package logs stores, reads and archives the log lines of runs.
//
// A line counts as stored once its batch is committed to PostgreSQL. The
// writer numbers lines per leg and sends them in order, so a gap can never
// come from the writer; the reader resumes with a cursor that holds the last
// sequence seen per leg, so lines committed late by one leg are never skipped.
package logs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rcss-labs/yamdrake/internal/store"
)

const (
	// maxLineBytes is the longest text stored for one line.
	maxLineBytes     = 16 * 1024
	truncationMarker = " ...[line cut]"
	flushTimeout     = 30 * time.Second
)

// pendingLine is a line accepted by Write and not yet numbered.
type pendingLine struct {
	at         time.Time
	level      string
	text       string
	generation int64
}

// legBuffer holds what is waiting to be stored for one leg.
type legBuffer struct {
	// flushMu serialises flushing of this leg so batches leave in order.
	flushMu sync.Mutex

	// Guarded by Writer.mu.
	runID int64
	queue []pendingLine
	// refusing is set after the first ErrBufferFull: later Writes fail at once
	// until the queue has room again.
	refusing bool

	// Guarded by flushMu.
	numbered   bool  // next and generation are valid
	generation int64 // generation the numbering belongs to
	next       int64 // sequence of the next line to number
	inflight   []store.LogLine
	inflightGn int64
	// failedSends counts the failed sends of the batch in flight.
	failedSends int
}

// Writer collects the lines runners produce and stores them in batches. It
// satisfies runner.LineSink.
type Writer struct {
	pool       *pgxpool.Pool
	flushEvery time.Duration
	maxBatch   int
	now        func() time.Time
	notify     func(runID int64)
	log        *slog.Logger

	// Test hooks: beforeInsert fails an attempt before it reaches PostgreSQL,
	// afterCommit fails it after the commit (a lost acknowledgement).
	beforeInsert func() error
	afterCommit  func() error

	mu        sync.Mutex
	legs      map[int64]*legBuffer
	closed    bool
	maxQueued int           // most lines one leg may have waiting
	fullWait  time.Duration // how long Write waits for room before refusing

	kick chan struct{}
	stop chan struct{}
	done chan struct{}
}

// NewWriter starts a writer that flushes every flushEvery and whenever a leg
// has maxBatch lines waiting. notify, when not nil, is called with the run id
// after each committed batch. log receives warnings and flush errors; nil
// discards them.
func NewWriter(pool *pgxpool.Pool, flushEvery time.Duration, maxBatch int, now func() time.Time, notify func(runID int64), log *slog.Logger) *Writer {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if maxBatch < 1 {
		maxBatch = 1
	}
	if flushEvery <= 0 {
		flushEvery = time.Second
	}
	if now == nil {
		now = time.Now
	}
	w := &Writer{
		pool: pool, flushEvery: flushEvery, maxBatch: maxBatch, now: now, notify: notify, log: log,
		maxQueued: DefaultMaxQueuedLines, fullWait: defaultFullWait,
		legs: make(map[int64]*legBuffer),
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	go w.loop()
	return w
}

// Write queues one line of a leg. generation is the one the runner was given
// when it claimed the leg; lines of a stale generation are refused at insert.
// It does not wait for the database, except when the leg's queue is full (see
// WithQueueLimit): then it waits briefly and returns ErrBufferFull.
func (w *Writer) Write(runID, legID, generation int64, level, text string) error {
	line := pendingLine{at: w.now(), level: normalizeLevel(level), text: cutText(text), generation: generation}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.log.Warn("log line written after the writer was closed; dropped", "run", runID, "leg", legID)
		return nil
	}
	lb := w.legs[legID]
	if lb == nil {
		lb = &legBuffer{runID: runID}
		w.legs[legID] = lb
	}
	// A leg's queue is capped: with PostgreSQL unreachable it would otherwise
	// grow until the runner runs out of memory. Wait briefly for room, then refuse.
	if len(lb.queue) >= w.maxQueued && lb.refusing {
		w.mu.Unlock()
		return ErrBufferFull
	}
	if len(lb.queue) >= w.maxQueued {
		deadline := time.Now().Add(w.fullWait)
		for len(lb.queue) >= w.maxQueued {
			w.mu.Unlock()
			select {
			case w.kick <- struct{}{}:
			default:
			}
			if !time.Now().Before(deadline) {
				w.log.Error("log queue of a leg is full and PostgreSQL is not taking lines; line refused",
					"run", runID, "leg", legID, "queued", w.maxQueued)
				w.mu.Lock()
				firstRefusal := false
				if cur := w.legs[legID]; cur != nil {
					firstRefusal = !cur.refusing
					cur.refusing = true
				}
				w.mu.Unlock()
				if firstRefusal {
					go w.recordPersistFailure() // the database is not taking lines; do not hold the executor
				}
				return ErrBufferFull
			}
			time.Sleep(fullPollInterval)
			w.mu.Lock()
			// The leg may have been forgotten and recreated while unlocked.
			if lb = w.legs[legID]; lb == nil {
				lb = &legBuffer{runID: runID}
				w.legs[legID] = lb
			}
		}
	}
	lb.refusing = false
	lb.queue = append(lb.queue, line)
	full := len(lb.queue) >= w.maxBatch
	w.mu.Unlock()
	if full {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
	return nil
}

// ErrBufferFull is returned by Write when a leg's queue is at its limit and
// PostgreSQL did not take lines within the wait. The line is not stored.
var ErrBufferFull = errors.New("log buffer is full and the database is not taking lines")

const (
	// DefaultMaxQueuedLines is how many lines one leg may have waiting.
	DefaultMaxQueuedLines = 10000
	// defaultFullWait is how long Write blocks the executor when the queue is full.
	defaultFullWait  = 5 * time.Second
	fullPollInterval = 10 * time.Millisecond
)

// WithQueueLimit sets the most lines one leg may have waiting (default 10,000)
// and how long Write waits for room when the queue is full (default 5s) before
// it refuses the line. Call it before the writer is used.
func (w *Writer) WithQueueLimit(maxLines int, wait time.Duration) *Writer {
	w.mu.Lock()
	defer w.mu.Unlock()
	if maxLines > 0 {
		w.maxQueued = maxLines
	}
	w.fullWait = wait
	return w
}

// Flush stores everything queued for the leg before the call, retrying a
// failed earlier batch first. It returns the first error; the unsent lines
// stay queued for the next attempt.
func (w *Writer) Flush(ctx context.Context, legID int64) error {
	w.mu.Lock()
	lb := w.legs[legID]
	w.mu.Unlock()
	if lb == nil {
		return nil
	}
	return w.flushLeg(ctx, legID, lb)
}

// Close stops the background flushing and stores what is still queued.
func (w *Writer) Close(ctx context.Context) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()
	close(w.stop)
	<-w.done
	return w.flushAll(ctx)
}

func (w *Writer) loop() {
	defer close(w.done)
	ticker := time.NewTicker(w.flushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
		case <-w.kick:
		}
		ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
		if err := w.flushAll(ctx); err != nil {
			w.log.Error("flushing log lines failed; will retry", "error", err)
		}
		cancel()
	}
}

func (w *Writer) flushAll(ctx context.Context) error {
	w.mu.Lock()
	ids := make([]int64, 0, len(w.legs))
	bufs := make([]*legBuffer, 0, len(w.legs))
	for id, lb := range w.legs {
		ids = append(ids, id)
		bufs = append(bufs, lb)
	}
	w.mu.Unlock()
	var errs []error
	for i, lb := range bufs {
		if err := w.flushLeg(ctx, ids[i], lb); err != nil {
			errs = append(errs, fmt.Errorf("leg %d: %w", ids[i], err))
		}
	}
	return errors.Join(errs...)
}

func (w *Writer) flushLeg(ctx context.Context, legID int64, lb *legBuffer) error {
	lb.flushMu.Lock()
	defer lb.flushMu.Unlock()
	for {
		if lb.inflight == nil {
			ok, err := w.prepareBatch(ctx, legID, lb)
			if err != nil {
				return err
			}
			if !ok {
				w.forgetIfIdle(legID, lb)
				return nil
			}
		}
		// A failed batch stays in flight and is sent again, with the same
		// numbers, before any later batch of this leg.
		if err := w.sendBatch(ctx, legID, lb); err != nil {
			lb.failedSends++
			if lb.failedSends == persistFailureThreshold {
				w.recordPersistFailure()
			}
			return err
		}
		lb.inflight = nil
		lb.failedSends = 0
	}
}

// recordPersistFailure counts one failure to persist log lines for the
// metrics. A batch that fails persistFailureThreshold times in a row counts
// once, however long it keeps failing, and so does the first line refused
// because a leg's queue is full. Recording is best effort: with the database
// down it fails too, and that is logged only.
func (w *Writer) recordPersistFailure() {
	ctx, cancel := context.WithTimeout(context.Background(), persistRecordTimeout)
	defer cancel()
	if err := store.RecordEvent(ctx, w.pool, "log_persist_failed", w.now()); err != nil {
		w.log.Warn("could not record the log persistence failure", "error", err)
	}
}

// prepareBatch numbers the next batch of queued lines into lb.inflight. It
// returns false when nothing is queued.
func (w *Writer) prepareBatch(ctx context.Context, legID int64, lb *legBuffer) (bool, error) {
	w.mu.Lock()
	if len(lb.queue) == 0 {
		w.mu.Unlock()
		return false, nil
	}
	generation := lb.queue[0].generation
	runID := lb.runID
	w.mu.Unlock()

	// First line of this leg, or a new generation: continue after the highest
	// stored sequence so the new attempt never collides with older lines.
	if !lb.numbered || lb.generation != generation {
		highest, err := store.MaxLogSeq(ctx, w.pool, runID, legID)
		if err != nil {
			return false, err
		}
		lb.numbered, lb.generation, lb.next = true, generation, highest+1
	}

	w.mu.Lock()
	n := 0
	for n < len(lb.queue) && n < w.maxBatch && lb.queue[n].generation == generation {
		n++
	}
	taken := lb.queue[:n:n]
	lb.queue = append([]pendingLine(nil), lb.queue[n:]...)
	w.mu.Unlock()

	batch := make([]store.LogLine, n)
	for i, p := range taken {
		batch[i] = store.LogLine{RunID: runID, LegID: legID, Seq: lb.next, At: p.at.UTC().Truncate(time.Microsecond), Level: p.level, Text: p.text}
		lb.next++
	}
	lb.inflight, lb.inflightGn = batch, generation
	return true, nil
}

// storeBatch inserts the batch in a short transaction and reports whether the
// leg accepted it (false: the runner no longer owns the leg, or it is
// archived). The steps are separate statements on purpose: the first locks the
// leg row FOR SHARE (waiting for an uncommitted re-claim or archive, and
// re-checking the generation on the row's final version), and the second reads
// log_archives with a fresh snapshot taken after that wait, so an archive that
// committed while we waited is seen. A single statement would use the snapshot
// from before the wait.
func (w *Writer) storeBatch(ctx context.Context, legID int64, lb *legBuffer) (bool, error) {
	batch := lb.inflight
	runID := batch[0].RunID
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var locked int64
	err = tx.QueryRow(ctx, `SELECT id FROM legs WHERE id = $1 AND run_id = $2 AND generation = $3 FOR SHARE`,
		legID, runID, lb.inflightGn).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var archived bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM log_archives WHERE run_id = $1 AND leg_id = $2)`, runID, legID).Scan(&archived); err != nil {
		return false, err
	}
	if archived {
		return false, nil
	}
	// The conflict clause drops lines that an earlier, unacknowledged attempt of
	// this very batch already stored.
	if err := store.InsertLogLines(ctx, tx, batch); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (w *Writer) sendBatch(ctx context.Context, legID int64, lb *legBuffer) error {
	if w.beforeInsert != nil {
		if err := w.beforeInsert(); err != nil {
			return err
		}
	}
	batch := lb.inflight
	accepted, err := w.storeBatch(ctx, legID, lb)
	if err != nil {
		return fmt.Errorf("store log batch: %w", err)
	}
	if w.afterCommit != nil {
		if err := w.afterCommit(); err != nil {
			return err
		}
	}
	if !accepted {
		// Retrying cannot help; drop the batch.
		w.log.Warn("log batch refused: the leg moved on; dropped",
			"run", batch[0].RunID, "leg", legID, "generation", lb.inflightGn, "lines", len(batch))
		return nil
	}
	if w.notify != nil {
		w.notify(batch[0].RunID)
	}
	return nil
}

// forgetIfIdle frees the bookkeeping of a leg with nothing queued. The caller
// holds lb.flushMu and lb has nothing in flight.
func (w *Writer) forgetIfIdle(legID int64, lb *legBuffer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(lb.queue) == 0 && w.legs[legID] == lb {
		delete(w.legs, legID)
	}
}

func normalizeLevel(level string) string {
	switch level {
	case "info", "ok", "warn", "error":
		return level
	}
	return "info"
}

// cutText makes text storable: valid UTF-8, no NUL bytes, at most
// maxLineBytes plus the marker that says it was cut.
func cutText(text string) string {
	text = strings.ToValidUTF8(text, "�")
	text = strings.ReplaceAll(text, "\x00", "�")
	if len(text) <= maxLineBytes {
		return text
	}
	cut := maxLineBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + truncationMarker
}

// persistFailureThreshold is how many failed sends of the same batch count as
// a failure to persist (see recordPersistFailure).
const persistFailureThreshold = 3

const persistRecordTimeout = 3 * time.Second
