package metrics

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Interval is how often metrics are emitted. Collect's window is the same length.
const Interval = 60 * time.Second

// Namespace is the CloudWatch namespace of every metric.
const Namespace = "Yumdrake"

// emitterLockKey is the PostgreSQL session advisory lock that lets only one
// of several orchestrator replicas emit. It is ASCII "YUMDMETR" and differs
// from the sync engine's key (ASCII "yumdrake").
const emitterLockKey int64 = 0x59554d444d455452

// lockTimeout bounds each statement on the lock connection so a stuck
// database cannot hold up a tick or the shutdown.
const lockTimeout = 5 * time.Second

// Emitter writes one EMF line of Collect's samples every interval, but only
// while it holds the emitter advisory lock.
type Emitter struct {
	pool      *pgxpool.Pool
	out       io.Writer
	namespace string
	dims      map[string]string
	every     time.Duration
	log       *slog.Logger
	now       func() time.Time
}

// NewEmitter returns an emitter that writes to out (standard output in
// production). A zero every means Interval; a nil log discards.
func NewEmitter(pool *pgxpool.Pool, out io.Writer, namespace string, dims map[string]string, every time.Duration, log *slog.Logger) *Emitter {
	if every <= 0 {
		every = Interval
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Emitter{pool: pool, out: out, namespace: namespace, dims: dims, every: every, log: log, now: time.Now}
}

// Run emits until ctx ends. Each tick it first makes sure it holds the lock:
// the lock lives on one connection taken from the pool and kept for as long
// as the lock is held. A replica that does not get the lock returns the
// connection at once and tries again on the next tick. If the held
// connection has broken (its session, and so its lock, may be gone) it is
// closed and a new attempt is made on the next tick. When ctx ends the lock
// is released and the connection returned.
func (e *Emitter) Run(ctx context.Context) {
	var holder *pgxpool.Conn
	defer func() { e.release(holder) }()

	ticker := time.NewTicker(e.every)
	defer ticker.Stop()
	for {
		holder = e.hold(ctx, holder)
		if holder != nil {
			e.emit(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// hold returns the connection holding the lock, or nil when this replica does
// not have it. held is the connection kept from earlier ticks, if any.
func (e *Emitter) hold(ctx context.Context, held *pgxpool.Conn) *pgxpool.Conn {
	if ctx.Err() != nil {
		return held
	}
	if held != nil {
		pingCtx, cancel := context.WithTimeout(ctx, lockTimeout)
		err := held.Ping(pingCtx)
		cancel()
		if err == nil {
			return held
		}
		if ctx.Err() == nil {
			e.log.Warn("metrics lock connection is broken; dropping it", "error", err.Error())
		}
		e.drop(held)
		return nil
	}
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Warn("metrics could not get a connection for the lock", "error", err.Error())
		}
		return nil
	}
	lockCtx, cancel := context.WithTimeout(ctx, lockTimeout)
	defer cancel()
	var got bool
	if err := conn.QueryRow(lockCtx, "SELECT pg_try_advisory_lock($1)", emitterLockKey).Scan(&got); err != nil {
		if ctx.Err() == nil {
			e.log.Warn("metrics could not try the lock", "error", err.Error())
		}
		e.drop(conn) // the statement may have run; closing the session frees the lock either way
		return nil
	}
	if !got {
		conn.Release()
		return nil
	}
	e.log.Info("this replica now emits metrics")
	return conn
}

// drop closes a connection for good, which ends its session and frees any lock on it.
func (e *Emitter) drop(conn *pgxpool.Conn) {
	closeCtx, cancel := context.WithTimeout(context.Background(), lockTimeout)
	defer cancel()
	_ = conn.Hijack().Close(closeCtx)
}

// release gives up the lock and the connection (a nil conn is fine).
func (e *Emitter) release(conn *pgxpool.Conn) {
	if conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
	defer cancel()
	var released bool
	if err := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", emitterLockKey).Scan(&released); err != nil || !released {
		// Do not hand a connection that may still hold the lock back to the pool.
		e.drop(conn)
		return
	}
	conn.Release()
}

// emit collects and writes one line. A failure is logged; the next tick tries again.
func (e *Emitter) emit(ctx context.Context) {
	at := e.now().UTC()
	samples, err := Collect(ctx, e.pool, at)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Error("could not collect metrics", "error", err.Error())
		}
		return
	}
	for _, line := range splitSamples(samples) {
		if err := Write(e.out, e.namespace, e.dims, at, line); err != nil {
			e.log.Error("could not write metrics", "error", err.Error())
		}
	}
}

// splitSamples divides samples into lines so that no name has more than
// maxValuesPerMetric values in one line (Write refuses more). Line k holds the
// k-th chunk of every name that has one, so with up to 100 durations there is
// one line, and with more the first line has every metric and the later ones
// only the remaining durations.
func splitSamples(samples []Sample) [][]Sample {
	var lines [][]Sample
	seen := map[string]int{}
	for _, s := range samples {
		k := seen[s.Name] / maxValuesPerMetric
		seen[s.Name]++
		for len(lines) <= k {
			lines = append(lines, nil)
		}
		lines[k] = append(lines[k], s)
	}
	return lines
}
