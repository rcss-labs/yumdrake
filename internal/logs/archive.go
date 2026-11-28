package logs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rcss-labs/yamdrake/internal/secrets"
)

// Archive stores the compressed log of settled legs.
type Archive interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
}

// logArchivePurpose binds every stored archive to this use in the cipher.
const logArchivePurpose = "log-archive"

type dirArchive struct {
	root   string
	cipher secrets.Cipher
}

// NewDirArchive stores encrypted archives as files under dir (created when
// missing). The cipher is required: archived logs are never written in clear.
func NewDirArchive(dir string, c secrets.Cipher) (Archive, error) {
	if dir == "" {
		return nil, errors.New("log archive: directory is empty")
	}
	if c == nil {
		return nil, errors.New("log archive: cipher is nil")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("log archive: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("log archive: create %s: %w", abs, err)
	}
	return &dirArchive{root: abs, cipher: c}, nil
}

// file maps a key to a path inside the root and refuses keys that escape it.
func (d *dirArchive) file(key string) (string, error) {
	clean := path.Clean("/" + key)
	if key == "" || strings.Contains(key, "\\") || clean != "/"+key {
		return "", fmt.Errorf("log archive: invalid key %q", key)
	}
	return filepath.Join(d.root, filepath.FromSlash(clean)), nil
}

// Put encrypts the archive and writes it through a temporary file and a rename,
// so a reader never sees a partial archive and a repeated Put of the same key
// replaces it.
func (d *dirArchive) Put(ctx context.Context, key string, data []byte) error {
	target, err := d.file(key)
	if err != nil {
		return err
	}
	sealed, err := d.cipher.Encrypt(ctx, logArchivePurpose, data)
	if err != nil {
		return fmt.Errorf("log archive: encrypt %s: %w", key, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("log archive: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".put-*")
	if err != nil {
		return fmt.Errorf("log archive: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(sealed); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("log archive: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("log archive: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("log archive: close: %w", err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return fmt.Errorf("log archive: rename: %w", err)
	}
	return nil
}

func (d *dirArchive) Get(ctx context.Context, key string) ([]byte, error) {
	target, err := d.file(key)
	if err != nil {
		return nil, err
	}
	sealed, err := os.ReadFile(target) //nolint:gosec // target comes from file(), which confines keys to the archive root
	if err != nil {
		return nil, fmt.Errorf("log archive: read %s: %w", key, err)
	}
	data, err := d.cipher.Decrypt(ctx, logArchivePurpose, sealed)
	if err != nil {
		// The message carries the key only, never the stored bytes.
		return nil, fmt.Errorf("log archive: cannot decrypt %s", key)
	}
	return data, nil
}

// archiveKey is where a leg's lines are stored.
func archiveKey(runID, legID int64) string {
	return fmt.Sprintf("runs/%d/legs/%d.ndjson.gz", runID, legID)
}

// archivedLine is one line of the NDJSON file.
type archivedLine struct {
	RunID int64     `json:"run_id"`
	LegID int64     `json:"leg_id"`
	Seq   int64     `json:"seq"`
	At    time.Time `json:"at"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
}

func encodeArchive(lines []archivedLine) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeArchive(data []byte) ([]archivedLine, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	dec := json.NewDecoder(bufio.NewReader(zr))
	var out []archivedLine
	for {
		var l archivedLine
		if err := dec.Decode(&l); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
}

// Archiver moves the lines of settled legs out of PostgreSQL.
type Archiver struct {
	pool      *pgxpool.Pool
	archive   Archive
	olderThan time.Duration
	now       func() time.Time
	log       *slog.Logger
}

// NewArchiver returns an Archiver that takes legs settled more than olderThan
// ago.
func NewArchiver(pool *pgxpool.Pool, a Archive, olderThan time.Duration, now func() time.Time, log *slog.Logger) *Archiver {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Archiver{pool: pool, archive: a, olderThan: olderThan, now: now, log: log}
}

// settledStates are the leg states that never change again. 'unknown' is left
// out on purpose: a reconciled unknown leg can be queued again, and its new
// attempt continues numbering after the lines stored in PostgreSQL.
const settledStates = `('succeeded', 'failed', 'skipped', 'cancelled')`

const archiveBatchSize = 500

// ArchiveSettledLegs archives up to archiveBatchSize legs per call and returns
// how many it archived. A leg that fails is logged and left for the next call;
// the error returned joins those failures.
func (a *Archiver) ArchiveSettledLegs(ctx context.Context) (int, error) {
	cutoff := a.now().Add(-a.olderThan)
	rows, err := a.pool.Query(ctx, `
SELECT l.run_id, l.id FROM legs l
WHERE l.state IN `+settledStates+`
  AND coalesce(l.finished_at, l.started_at, l.queued_at) < $1
  AND EXISTS (SELECT 1 FROM log_lines x WHERE x.run_id = l.run_id AND x.leg_id = l.id)
  AND NOT EXISTS (SELECT 1 FROM log_archives z WHERE z.run_id = l.run_id AND z.leg_id = l.id)
ORDER BY l.id LIMIT $2`, cutoff, archiveBatchSize)
	if err != nil {
		return 0, fmt.Errorf("find legs to archive: %w", err)
	}
	type target struct{ run, leg int64 }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.run, &t.leg); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan leg to archive: %w", err)
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("find legs to archive: %w", err)
	}

	a.reportLeftoverRows(ctx)
	archived := 0
	var errs []error
	for _, t := range targets {
		ok, err := a.archiveLeg(ctx, t.run, t.leg)
		if err != nil {
			a.log.Error("archiving a leg's log failed", "run", t.run, "leg", t.leg, "error", err)
			errs = append(errs, fmt.Errorf("leg %d: %w", t.leg, err))
			continue
		}
		if ok {
			archived++
		}
	}
	return archived, errors.Join(errs...)
}

// reportLeftoverRows logs, loudly, legs that are archived yet have rows in
// PostgreSQL. They are never archived again; someone has to look at them.
func (a *Archiver) reportLeftoverRows(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
SELECT x.run_id, x.leg_id, count(*) FROM log_lines x
JOIN log_archives z ON z.run_id = x.run_id AND z.leg_id = x.leg_id
GROUP BY x.run_id, x.leg_id LIMIT 20`)
	if err != nil {
		a.log.Error("checking for leftover log rows failed", "error", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var run, leg, n int64
		if err := rows.Scan(&run, &leg, &n); err != nil {
			a.log.Error("checking for leftover log rows failed", "error", err)
			return
		}
		a.log.Error("archived leg has log rows in PostgreSQL; left alone, the archive is not rewritten", "run", run, "leg", leg, "rows", n)
	}
}

// archiveLeg writes one leg to the archive, records it and deletes its rows
// in one transaction. Locking the leg row waits for a writer that is mid-insert
// and, once the record is committed, makes later inserts for the leg refuse.
// Rows are deleted only after Put succeeded; if the commit then fails the
// object is simply written again on the next attempt.
func (a *Archiver) archiveLeg(ctx context.Context, runID, legID int64) (bool, error) {
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var locked int64
	err = tx.QueryRow(ctx, `SELECT id FROM legs WHERE id = $1 AND run_id = $2 AND state IN `+settledStates+` FOR UPDATE`, legID, runID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock leg: %w", err)
	}

	// Never replace an existing archive: rows that reappear for an archived leg
	// would otherwise overwrite the real archive with just those rows.
	var already bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM log_archives WHERE run_id = $1 AND leg_id = $2)`, runID, legID).Scan(&already); err != nil {
		return false, fmt.Errorf("check archive: %w", err)
	}
	if already {
		var leftover bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM log_lines WHERE run_id = $1 AND leg_id = $2)`, runID, legID).Scan(&leftover); err != nil {
			return false, fmt.Errorf("check leftover rows: %w", err)
		}
		if !leftover {
			// Another orchestrator archived the leg between our listing and our lock.
			a.log.Debug("leg was archived by another orchestrator; nothing to do", "run", runID, "leg", legID)
			return false, nil
		}
		return false, errors.New("leg is already archived but has rows in PostgreSQL; refusing to overwrite the archive")
	}

	rows, err := tx.Query(ctx, `SELECT seq, at, level, text FROM log_lines WHERE run_id = $1 AND leg_id = $2 ORDER BY seq`, runID, legID)
	if err != nil {
		return false, fmt.Errorf("read lines: %w", err)
	}
	var lines []archivedLine
	for rows.Next() {
		l := archivedLine{RunID: runID, LegID: legID}
		if err := rows.Scan(&l.Seq, &l.At, &l.Level, &l.Text); err != nil {
			rows.Close()
			return false, fmt.Errorf("scan line: %w", err)
		}
		l.At = l.At.UTC()
		lines = append(lines, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read lines: %w", err)
	}
	if len(lines) == 0 {
		return false, nil
	}

	data, err := encodeArchive(lines)
	if err != nil {
		return false, fmt.Errorf("encode: %w", err)
	}
	key := archiveKey(runID, legID)
	if err := a.archive.Put(ctx, key, data); err != nil {
		return false, fmt.Errorf("put %s: %w", key, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO log_archives (run_id, leg_id, key, line_count, first_seq, last_seq, archived_at, first_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		runID, legID, key, len(lines), lines[0].Seq, lines[len(lines)-1].Seq, a.now().UTC().Truncate(time.Microsecond), lines[0].At); err != nil {
		return false, fmt.Errorf("record archive: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM log_lines WHERE run_id = $1 AND leg_id = $2`, runID, legID); err != nil {
		return false, fmt.Errorf("delete lines: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// archiveRef is a row of log_archives.
type archiveRef struct {
	LegID, LineCount, LastSeq int64
	Key                       string
	FirstAt                   *time.Time // nil for archives written before it was recorded
}

func (r *Reader) archivedLegs(ctx context.Context, q querier, runID int64) (map[int64]archiveRef, error) {
	rows, err := q.Query(ctx, `SELECT leg_id, key, line_count, last_seq, first_at FROM log_archives WHERE run_id = $1`, runID)
	if err != nil {
		return nil, fmt.Errorf("list log archives: %w", err)
	}
	defer rows.Close()
	out := map[int64]archiveRef{}
	for rows.Next() {
		var ref archiveRef
		if err := rows.Scan(&ref.LegID, &ref.Key, &ref.LineCount, &ref.LastSeq, &ref.FirstAt); err != nil {
			return nil, fmt.Errorf("scan log archive: %w", err)
		}
		out[ref.LegID] = ref
	}
	return out, rows.Err()
}

// readArchived loads the archived legs that still have lines after the cursor
// and returns their matching lines (ordered like PostgreSQL rows) plus every
// gap after the cursor, per leg.
//
// At most limit matching lines are returned per leg: lines of one leg come out
// in sequence order with a non-decreasing sort key, so no later line of the leg
// can belong to a page of limit lines. cache, when not nil, keeps each decoded
// leg between calls so a paged read decompresses a leg once.
func (r *Reader) readArchived(ctx context.Context, q querier, runID int64, f Filter, cursor map[int64]int64, archived map[int64]archiveRef, limit int, cache archiveCache, rowCands []candidate) ([]candidate, map[int64][]Gap, error) {
	var wanted []int64
	for leg, ref := range archived {
		if ref.LastSeq > cursor[leg] {
			wanted = append(wanted, leg)
		}
	}
	// Legs that must be decoded now (already being read, or with no recorded
	// start time) come first, then the others by start time, so a leg that
	// starts after the page is already full is left alone.
	mustRead := func(leg int64) bool {
		_, cached := cache[leg]
		return archived[leg].FirstAt == nil || cursor[leg] > 0 || cached
	}
	sort.Slice(wanted, func(i, j int) bool {
		a, b := wanted[i], wanted[j]
		if mustRead(a) != mustRead(b) {
			return mustRead(a)
		}
		if !mustRead(a) && !archived[a].FirstAt.Equal(*archived[b].FirstAt) {
			return archived[a].FirstAt.Before(*archived[b].FirstAt)
		}
		return a < b
	})
	if len(wanted) == 0 {
		return nil, nil, nil
	}
	if r.archive == nil {
		return nil, nil, errors.New("read log: legs are archived but no archive is configured")
	}
	type legMeta struct {
		stepID  *int64
		service string
	}
	meta := map[int64]legMeta{}
	rows, err := q.Query(ctx, `SELECT id, step_id, service FROM legs WHERE run_id = $1 AND id = ANY($2)`, runID, wanted)
	if err != nil {
		return nil, nil, fmt.Errorf("read archived legs: %w", err)
	}
	for rows.Next() {
		var id int64
		var m legMeta
		if err := rows.Scan(&id, &m.stepID, &m.service); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan archived leg: %w", err)
		}
		meta[id] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read archived legs: %w", err)
	}

	var out []candidate
	gaps := map[int64][]Gap{}
	query := strings.ToLower(f.Query)
	for _, leg := range wanted {
		ref := archived[leg]
		if !mustRead(leg) && len(rowCands)+len(out) >= limit {
			// Every line of this leg sorts at or after its first line's time. When
			// the page already holds `limit` lines before that time, it cannot
			// reach the leg (and neither can any later-starting leg).
			orders := make([]time.Time, 0, len(rowCands)+len(out))
			for _, c := range rowCands {
				orders = append(orders, c.order)
			}
			for _, c := range out {
				orders = append(orders, c.order)
			}
			sort.Slice(orders, func(i, j int) bool { return orders[i].Before(orders[j]) })
			if ref.FirstAt.After(orders[limit-1]) {
				break
			}
		}
		lines, cached := cache[leg]
		if !cached {
			data, err := r.archive.Get(ctx, ref.Key)
			if err != nil {
				return nil, nil, fmt.Errorf("read archived leg %d: %w", leg, err)
			}
			lines, err = decodeArchive(data)
			if err != nil {
				return nil, nil, fmt.Errorf("decode archived leg %d: %w", leg, err)
			}
			if int64(len(lines)) != ref.LineCount {
				return nil, nil, fmt.Errorf("archived leg %d has %d lines, recorded %d", leg, len(lines), ref.LineCount)
			}
			if cache != nil {
				cache[leg] = lines
			}
		}
		prev := cursor[leg]
		var order time.Time
		m := meta[leg]
		matched := 0
		// Lines are in sequence order: skip straight to the first one after the cursor.
		start := sort.Search(len(lines), func(i int) bool { return lines[i].Seq > cursor[leg] })
		for _, l := range lines[start:] {
			if matched >= limit {
				break
			}
			if l.Seq-prev > 1 {
				gaps[leg] = append(gaps[leg], Gap{LegID: leg, FromSeq: prev + 1, ToSeq: l.Seq - 1})
			}
			prev = l.Seq
			if l.At.After(order) {
				order = l.At
			}
			if f.StepID != nil && (m.stepID == nil || *m.stepID != *f.StepID) {
				continue
			}
			if f.Service != "" && m.service != f.Service {
				continue
			}
			if f.Level != "" && l.Level != f.Level {
				continue
			}
			if query != "" && !strings.Contains(strings.ToLower(l.Text), query) {
				continue
			}
			matched++
			out = append(out, candidate{
				Line:  Line{RunID: runID, LegID: leg, Seq: l.Seq, At: l.At.UTC(), Level: l.Level, Text: l.Text},
				order: order.UTC(),
			})
		}
	}
	return out, gaps, nil
}
