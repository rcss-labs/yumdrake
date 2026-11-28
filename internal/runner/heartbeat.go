package runner

import (
	"context"
	"time"

	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/version"
)

// RegistryInterval is how often a runner reports itself to the registry.
const RegistryInterval = 10 * time.Second

// registryWriteTimeout bounds one registry write so a stuck database cannot
// hold up the runner's shutdown.
const registryWriteTimeout = 5 * time.Second

// registry is the runner's entry in the runners table.
type registry struct {
	db        store.DBTX
	every     time.Duration
	startedAt time.Time
}

// WithRegistry makes the runner report itself to the runners table through db:
// when it starts, every `every` (RegistryInterval when zero), and once more
// with Busy 0 after it has drained on a clean shutdown. A failed write is
// logged at warn and never affects the runner. Call it before Run.
func (r *Runner) WithRegistry(db store.DBTX, every time.Duration) *Runner {
	if every <= 0 {
		every = RegistryInterval
	}
	r.registry = &registry{db: db, every: every, startedAt: time.Now().UTC()}
	return r
}

// report writes one row; busy is read at the time of the call.
func (r *Runner) report(ctx context.Context, busy int) {
	ctx, cancel := context.WithTimeout(ctx, registryWriteTimeout)
	defer cancel()
	err := store.UpsertRunner(ctx, r.registry.db, store.RunnerInfo{
		ID: r.id, Version: version.Version, Concurrency: r.concurrency, Busy: busy,
		StartedAt: r.registry.startedAt, LastSeenAt: time.Now().UTC(),
	})
	if err != nil {
		r.log.Warn("could not report to the runner registry", "error", err.Error())
	}
}

func (r *Runner) busyNow() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// beat reports every interval until ctx ends. Run stops it only after the legs
// have drained, so the registry follows the busy count through the shutdown.
func (r *Runner) beat(ctx context.Context) {
	ticker := time.NewTicker(r.registry.every)
	defer ticker.Stop()
	for {
		r.report(ctx, r.busyNow())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
