//go:build integration

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/wake"
)

type sseEvent struct {
	Name, ID, Data string
	// KeepAlive is set for the server's `event: ping` frames. A comment line is not one: browsers never show
	// comments to a page, so the keep-alive must be a named event.
	KeepAlive bool
}

// sseStream reads a Server-Sent Events response in the background.
type sseStream struct {
	events chan sseEvent
	resp   *http.Response
	cancel context.CancelFunc
}

func (f *runFixture) openStream(server *httptest.Server, token, path string, headers map[string]string) *sseStream {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", server.URL+path, nil)
	if err != nil {
		f.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		f.t.Fatalf("open stream: %v", err)
	}
	s := &sseStream{events: make(chan sseEvent, 4096), resp: resp, cancel: cancel}
	go func() {
		defer close(s.events)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1<<20), 8<<20)
		var cur sseEvent
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				if cur.Name != "" || cur.Data != "" {
					cur.KeepAlive = cur.Name == "ping"
					s.events <- cur
				}
				cur = sseEvent{}
			case strings.HasPrefix(line, ":"):
				s.events <- sseEvent{Name: "comment", Data: strings.TrimSpace(line[1:])}
			case strings.HasPrefix(line, "event: "):
				cur.Name = line[len("event: "):]
			case strings.HasPrefix(line, "id: "):
				cur.ID = line[len("id: "):]
			case strings.HasPrefix(line, "data: "):
				cur.Data += line[len("data: "):]
			}
		}
	}()
	f.t.Cleanup(s.close)
	return s
}

func (s *sseStream) close() {
	s.cancel()
	_ = s.resp.Body.Close()
}

// next returns the next event, or false when the stream ended or nothing came in time.
func (s *sseStream) next(timeout time.Duration) (sseEvent, bool) {
	select {
	case e, ok := <-s.events:
		return e, ok
	case <-time.After(timeout):
		return sseEvent{}, false
	}
}

// nextNamed skips comments and returns the next real event.
func (s *sseStream) nextNamed(t *testing.T, timeout time.Duration) sseEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		e, ok := s.next(time.Until(deadline))
		if !ok {
			t.Fatalf("stream ended or stalled while waiting for an event")
		}
		if !e.KeepAlive {
			return e
		}
	}
}

type logBatch struct {
	Lines []struct {
		LegID int64  `json:"leg_id"`
		Seq   int64  `json:"seq"`
		Text  string `json:"text"`
	} `json:"lines"`
	Gaps []any `json:"gaps"`
}

func (f *runFixture) streamServer(change func(*Deps)) *httptest.Server {
	f.t.Helper()
	f.rebuild(func(d *Deps) {
		d.StreamKeepAlive = 40 * time.Millisecond
		d.StreamPollInterval = 15 * time.Millisecond
		d.StreamBatchLines = 7
		if change != nil {
			change(d)
		}
	})
	server := httptest.NewServer(f.handler)
	f.t.Cleanup(server.Close)
	return server
}

func (f *runFixture) setRun(id int64, state string) {
	f.t.Helper()
	f.exec(`UPDATE runs SET state = $2, state_changed_at = now() WHERE id = $1`, id, state)
}

func TestEventsSendStateThenLogsThenEnd(t *testing.T) {
	f := newRunFixture(t)
	server := f.streamServer(nil)
	id := f.newRun(f.jobID, devEnvironment, "s1")
	f.setRun(id, "running")
	legs, _ := store.LegsOfRun(context.Background(), f.pool, id)
	leg := legs[1].ID
	f.addLines(id, store.LogLine{LegID: leg, Seq: 1, Text: "first"}, store.LogLine{LegID: leg, Seq: 2, Text: "second"})

	stream := f.openStream(server, f.adminToken, runPath(id, "/events"), nil)
	h := stream.resp.Header
	if h.Get("Content-Type") != "text/event-stream" || !strings.Contains(h.Get("Cache-Control"), "no-cache") || h.Get("X-Accel-Buffering") != "no" {
		t.Errorf("headers = %v, want text/event-stream, Cache-Control no-cache and X-Accel-Buffering no", h)
	}
	first := stream.nextNamed(t, 3*time.Second)
	if first.Name != "state" {
		t.Fatalf("first event = %q, want state", first.Name)
	}
	var state struct {
		ID      int64  `json:"id"`
		State   string `json:"state"`
		Legs    []any  `json:"legs"`
		Summary struct {
			Counts map[string]int `json:"counts"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(first.Data), &state); err != nil || state.ID != id || state.State != "running" || len(state.Legs) != 3 || state.Summary.Counts == nil {
		t.Errorf("state event = %s (%v), want the run's states and summary", first.Data, err)
	}
	seen := map[string]bool{}
	for len(seen) < 3 { // the run-level creation line and the two lines
		e := stream.nextNamed(t, 3*time.Second)
		if e.Name != "log" || e.ID == "" {
			t.Fatalf("event %q id %q, want log events with the cursor as id", e.Name, e.ID)
		}
		var b logBatch
		_ = json.Unmarshal([]byte(e.Data), &b)
		for _, l := range b.Lines {
			seen[fmt.Sprintf("%d/%d", l.LegID, l.Seq)] = true
		}
	}
	// Idle: only `event: ping` with an empty object arrives (a comment would never reach a browser page).
	gotPing := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && !gotPing; {
		e, ok := stream.next(200 * time.Millisecond)
		if ok && !e.KeepAlive {
			t.Fatalf("event %q while idle, want only ping events", e.Name)
		}
		if ok && e.Data != "{}" {
			t.Fatalf("ping data = %q, want {}", e.Data)
		}
		gotPing = ok && e.KeepAlive
	}
	if !gotPing {
		t.Error("no ping event on an idle stream")
	}

	// A state change is sent before the lines that follow it.
	f.setRun(id, "cancelling")
	change := stream.nextNamed(t, 3*time.Second)
	if change.Name != "state" || !strings.Contains(change.Data, `"cancelling"`) {
		t.Fatalf("event after the state changed = %q %s, want the state event first", change.Name, change.Data)
	}
	// The line and the final state are separate commits, so the stream may send the line before the
	// final state; only the end is fixed: after the final state and after every line.
	f.addLines(id, store.LogLine{LegID: leg, Seq: 3, Text: "last line"})
	f.setRun(id, "succeeded")
	var names []string
	var lines []string
	finalStateAt := -1
	for {
		e, ok := stream.next(3 * time.Second)
		if !ok {
			break
		}
		if e.KeepAlive {
			continue
		}
		names = append(names, e.Name)
		if e.Name == "state" && strings.Contains(e.Data, `"succeeded"`) {
			finalStateAt = len(names) - 1
		}
		if e.Name == "log" {
			var b logBatch
			_ = json.Unmarshal([]byte(e.Data), &b)
			for _, l := range b.Lines {
				lines = append(lines, l.Text)
			}
		}
	}
	if len(names) < 3 || names[len(names)-1] != "end" || finalStateAt < 0 || finalStateAt > len(names)-2 {
		t.Fatalf("events after the run finished = %v, want the final state and the log in either order, then end and then the stream closes", names)
	}
	if fmt.Sprint(lines) != "[last line]" {
		t.Errorf("lines after finishing = %v, want the last line exactly once", lines)
	}
}

func TestEventsResumeWithLastEventIDHasNoGapAndNoDuplicate(t *testing.T) {
	f := newRunFixture(t)
	server := f.streamServer(nil)
	id := f.newRun(f.jobID, devEnvironment, "s1")
	f.setRun(id, "running")
	legs, _ := store.LegsOfRun(context.Background(), f.pool, id)
	legIDs := []int64{legs[0].ID, legs[1].ID, legs[2].ID}
	const perLeg = 40
	write := func(seq int64) {
		for _, leg := range legIDs {
			f.addLines(id, store.LogLine{LegID: leg, Seq: seq, Text: fmt.Sprintf("leg %d line %d", leg, seq)})
		}
	}
	for seq := int64(1); seq <= 10; seq++ {
		write(seq)
	}
	// A writer keeps adding lines while the client reads, drops and returns.
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for seq := int64(11); seq <= perLeg; seq++ {
			write(seq)
			time.Sleep(3 * time.Millisecond)
		}
	}()

	received := map[string]int{}
	collect := func(e sseEvent) {
		var b logBatch
		if err := json.Unmarshal([]byte(e.Data), &b); err != nil {
			t.Fatalf("decode log event: %v", err)
		}
		for _, l := range b.Lines {
			received[fmt.Sprintf("%d/%d", l.LegID, l.Seq)]++
		}
	}

	first := f.openStream(server, f.adminToken, runPath(id, "/events"), nil)
	if e := first.nextNamed(t, 3*time.Second); e.Name != "state" {
		t.Fatalf("first event = %q, want state", e.Name)
	}
	lastID := ""
	for range 3 {
		e := first.nextNamed(t, 3*time.Second)
		if e.Name != "log" {
			t.Fatalf("event %q, want log", e.Name)
		}
		collect(e)
		lastID = e.ID
	}
	first.close() // the browser's connection drops

	second := f.openStream(server, f.adminToken, runPath(id, "/events"), map[string]string{"Last-Event-ID": lastID})
	if e := second.nextNamed(t, 3*time.Second); e.Name != "state" {
		t.Fatalf("after reconnecting, first event = %q, want the current state", e.Name)
	}
	writer.Wait()
	f.setRun(id, "succeeded")
	ended := false
	for {
		e, ok := second.next(5 * time.Second)
		if !ok {
			break
		}
		switch {
		case e.KeepAlive:
		case e.Name == "log":
			collect(e)
		case e.Name == "end":
			ended = true
		}
	}
	if !ended {
		t.Fatal("the resumed stream never sent end")
	}

	var stored int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM log_lines WHERE run_id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := 3*perLeg + 1; stored != want { // plus the run-level creation line
		t.Fatalf("stored lines = %d, want %d", stored, want)
	}
	if len(received) != stored {
		t.Errorf("distinct lines received = %d, stored %d: lines were missed", len(received), stored)
	}
	for key, n := range received {
		if n != 1 {
			t.Errorf("line %s received %d times, want exactly once", key, n)
		}
	}
}

func TestEventsWithoutViewLogsCarryNoLogEvents(t *testing.T) {
	f := newRunFixture(t)
	server := f.streamServer(nil)
	id := f.newRun(f.jobID, devEnvironment, "s1")
	f.setRun(id, "running")
	legs, _ := store.LegsOfRun(context.Background(), f.pool, id)
	f.addLines(id, store.LogLine{LegID: legs[1].ID, Seq: 1, Text: "secret-looking detail"})
	f.setRun(id, "succeeded")

	for name, token := range map[string]string{
		"view only":           f.userWith("viewonly"),
		"view_logs elsewhere": f.userWith("elsewhere", store.Grant{Action: "view_logs", Label: "prod"}),
	} {
		stream := f.openStream(server, token, runPath(id, "/events"), nil)
		var names []string
		for {
			e, ok := stream.next(3 * time.Second)
			if !ok {
				break
			}
			if !e.KeepAlive {
				names = append(names, e.Name)
				if strings.Contains(e.Data, "secret-looking detail") {
					t.Errorf("%s: an event carries log text", name)
				}
			}
		}
		if fmt.Sprint(names) != "[state end]" {
			t.Errorf("%s: events = %v, want state then end, no log events", name, names)
		}
	}
	// With view_logs the same run does carry them.
	stream := f.openStream(server, f.userWith("logs", store.Grant{Action: "view_logs", Label: "dev"}), runPath(id, "/events"), nil)
	var names []string
	for {
		e, ok := stream.next(3 * time.Second)
		if !ok {
			break
		}
		if !e.KeepAlive {
			names = append(names, e.Name)
		}
	}
	if len(names) < 3 || names[1] != "log" {
		t.Errorf("with view_logs: events = %v, want state, log, ..., end", names)
	}
}

func TestEventsRefuseBadRequestsBeforeStreaming(t *testing.T) {
	f := newRunFixture(t)
	server := f.streamServer(nil)
	id := f.newRun(f.jobID, devEnvironment, "s1")
	for name, c := range map[string]struct {
		path    string
		headers map[string]string
		want    int
	}{
		"unknown run":           {runPath(999999, "/events"), nil, 404},
		"bad Last-Event-ID":     {runPath(id, "/events"), map[string]string{"Last-Event-ID": "garbage!"}, 400},
		"bad after":             {runPath(id, "/events?after=garbage!"), nil, 400},
		"good after is allowed": {runPath(id, "/events?after="), nil, 200},
	} {
		stream := f.openStream(server, f.adminToken, c.path, c.headers)
		if stream.resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", name, stream.resp.StatusCode, c.want)
		}
		stream.close()
	}
}

func TestEventsReactToWakeupsWithoutWaitingForThePoll(t *testing.T) {
	f := newRunFixture(t)
	bus := wake.New(f.valkey, nil, wake.WithKeyPrefix(f.deps.KeyPrefix))
	server := f.streamServer(func(d *Deps) {
		d.Wake = bus
		d.StreamPollInterval = time.Hour // only a wake-up can make the stream look again
		d.StreamKeepAlive = time.Hour
	})
	id := f.newRun(f.jobID, devEnvironment, "s1")
	f.setRun(id, "running")
	stream := f.openStream(server, f.adminToken, runPath(id, "/events"), nil)
	for e := stream.nextNamed(t, 3*time.Second); e.Name != "state"; e = stream.nextNamed(t, 3*time.Second) {
	}
	// Give the subscription a moment to be in place, then change and publish.
	deadline := time.Now().Add(5 * time.Second)
	f.setRun(id, "cancelling")
	for time.Now().Before(deadline) {
		bus.Publish(context.Background(), wake.TopicRuns, id)
		e, ok := stream.next(200 * time.Millisecond)
		if ok && e.Name == "state" && strings.Contains(e.Data, `"cancelling"`) {
			return
		}
	}
	t.Fatal("a published wake-up did not make the stream send the new state")
}

func goroutinesIn(name string) bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	return strings.Contains(string(buf), name)
}

func TestEventsHandlerStopsOnClientDisconnect(t *testing.T) {
	// Keep-alive and poll are an hour, so nothing but the request context
	// ending can stop the handler: without wake-ups only its ctx.Done branch
	// can; with them, the subscriptions must end as well.
	for _, withBus := range []bool{false, true} {
		t.Run(fmt.Sprintf("wakeups=%v", withBus), func(t *testing.T) {
			f := newRunFixture(t)
			server := f.streamServer(func(d *Deps) {
				if withBus {
					d.Wake = wake.New(f.valkey, nil, wake.WithKeyPrefix(f.deps.KeyPrefix))
				}
				d.StreamKeepAlive = time.Hour
				d.StreamPollInterval = time.Hour
			})
			id := f.newRun(f.jobID, devEnvironment, "s1")
			f.setRun(id, "running")

			stream := f.openStream(server, f.adminToken, runPath(id, "/events"), nil)
			if e := stream.nextNamed(t, 3*time.Second); e.Name != "state" {
				t.Fatalf("first event = %q", e.Name)
			}
			// While the client is connected the handler runs; the checks below
			// would otherwise prove nothing.
			if !goroutinesIn("api.(*server).streamEvents") {
				t.Fatal("no stream handler goroutine while a client is connected")
			}
			stream.close()
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				if !goroutinesIn("api.(*server).streamEvents") && !goroutinesIn("wake.(*Bus).Subscribe") {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("the handler or its wake-up subscriptions still run 1s after the client disconnected")
		})
	}
}

// TestEventsOfAHugeLogAreWrittenPageByPageInBoundedMemory sends 50,000 lines
// through the stream handler. The first round used to read the whole log into
// one buffer before the first byte went out; the handler must hold one page at
// a time, so the heap while streaming stays near its starting size however long
// the log is.
func TestEventsOfAHugeLogAreWrittenPageByPageInBoundedMemory(t *testing.T) {
	f := newRunFixture(t)
	server := f.streamServer(func(d *Deps) {
		d.StreamBatchLines = 200
		d.StreamPollInterval = time.Second
	})
	id := f.newRun(f.jobID, devEnvironment, "s1")
	f.setRun(id, "running")
	legs, _ := store.LegsOfRun(context.Background(), f.pool, id)
	const (
		perLeg    = 25000
		textBytes = 400
	)
	for _, leg := range []int64{legs[1].ID, legs[2].ID} {
		f.exec(`INSERT INTO log_lines (run_id, leg_id, seq, at, level, text)
			SELECT $1, $2, g, now() + g * interval '1 millisecond', 'info', repeat('x', $3) || g::text
			FROM generate_series(1, $4) g`, id, leg, textBytes, perLeg)
	}
	f.setRun(id, "succeeded") // final: the stream sends everything and ends

	// Keep garbage tight so the heap reflects what the handler holds.
	old := debug.SetGCPercent(10)
	defer debug.SetGCPercent(old)
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stopSampling := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			for p := peak.Load(); m.HeapAlloc > p && !peak.CompareAndSwap(p, m.HeapAlloc); p = peak.Load() {
			}
			select {
			case <-stopSampling:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()

	req, _ := http.NewRequest("GET", server.URL+runPath(id, "/events"), nil)
	req.Header.Set("Authorization", "Bearer "+f.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Count the lines without keeping the stream: each log line carries one "seq".
	const marker = `"seq":`
	counted := 0
	buf := make([]byte, 32<<10)
	tail, last := "", ""
	for {
		n, readErr := resp.Body.Read(buf)
		chunk := tail + string(buf[:n])
		counted += strings.Count(chunk, marker)
		// A marker lies in the chunk or across its end; the tail is shorter than
		// a marker, so nothing is counted twice.
		tail = chunk[max(0, len(chunk)-(len(marker)-1)):]
		last = (last + string(buf[:n]))[max(0, len(last)+n-16384):]
		if readErr != nil {
			break
		}
	}
	ended := strings.Contains(last, "event: end")
	close(stopSampling)
	<-samplerDone

	if want := 2*perLeg + 1; counted != want { // plus the run-level creation line
		t.Fatalf("stream carried %d lines, want %d", counted, want)
	}
	if !ended {
		t.Fatalf("the stream never sent end")
	}
	growth := int64(peak.Load()) - int64(base.HeapAlloc)
	const bound = 24 << 20 // the log's JSON is over 20 MB; one page is about 100 KB
	if growth > bound {
		t.Fatalf("heap grew by %d MB while streaming %d lines, want under %d MB: the handler holds more than a page", growth>>20, counted, bound>>20)
	}
}
