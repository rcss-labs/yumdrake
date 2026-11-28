package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/logs"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/wake"
)

const (
	defaultStreamKeepAlive    = 15 * time.Second
	defaultStreamPollInterval = time.Second
	defaultStreamBatchLines   = 200
)

// Waker delivers the ids of runs that changed; *wake.Bus satisfies it.
type Waker interface {
	Subscribe(ctx context.Context, topic string) <-chan int64
}

// eventStream is the state of one client's stream.
type eventStream struct {
	s        *server
	run      store.Run
	steps    []store.RunStep
	withLogs bool
	// cursor is the log position after the last batch handed to the client;
	// lastState is the last state event body, so unchanged state is not resent.
	cursor    logs.Cursor
	lastState []byte
	// session keeps what a paged read of the log needs between pages (decoded
	// archived legs), so the whole log is read in linear time.
	session *logs.Session
}

// streamEvents serves GET /runs/{id}/events as Server-Sent Events.
//
// The client first gets a state event, then log events (id: the cursor after
// the batch), then state events as the run changes, and an end event once the
// run is final and its last lines were sent. A client that reconnects with
// Last-Event-ID (or ?after=) gets the state again and then only the lines after
// that cursor: the cursor holds the last sequence seen per leg, so no line is
// repeated or skipped whatever was committed in between. The stream looks for
// changes when a wake-up arrives and also on a timer, so it works without
// Valkey. The cursor belongs to an unfiltered log, which is what the stream
// reads.
func (s *server) streamEvents(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runFromPath(w, r)
	if !ok {
		return
	}
	principal, ok := s.principal(w, r)
	if !ok {
		return
	}
	steps, err := store.StepsOfRun(r.Context(), s.Pool, run.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	stream := &eventStream{s: s, run: run, steps: steps, withLogs: principal.Can(auth.ViewLogs, targetOfRun(run)),
		session: s.Logs.NewSession(run.ID)}
	// The header is what a reconnecting browser sends; ?after= is for clients
	// that start a stream at a known position.
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		stream.cursor = logs.Cursor(id)
	} else {
		stream.cursor = logs.Cursor(r.URL.Query().Get("after"))
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// A stream is open for as long as the run runs: lift any write deadline.
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})
	started := false
	send := func(data []byte) bool {
		if !started {
			header := w.Header()
			header.Set("Content-Type", "text/event-stream")
			header.Set("Cache-Control", "no-cache")
			header.Set("X-Accel-Buffering", "no") // nginx: do not buffer the stream
			w.WriteHeader(http.StatusOK)
			started = true
		}
		if _, err := w.Write(data); err != nil {
			return false
		}
		return controller.Flush() == nil
	}

	// The first round writes each page as it reads it. Nothing is sent before
	// the first page has been read, so a bad cursor or a database failure there
	// is still an ordinary error answer.
	var out bytes.Buffer
	final, err := stream.collect(ctx, &out, send)
	switch {
	case !started && errors.Is(err, logs.ErrBadCursor):
		invalid(w, "the cursor is not one this log produced")
		return
	case !started && err != nil && !errors.Is(err, errClientGone):
		s.internalError(w, r, err)
		return
	case err != nil:
		if !errors.Is(err, errClientGone) && ctx.Err() == nil {
			s.Log.Error("event stream ended", "run", run.ID, "error", err.Error())
		}
		return
	}

	var wakeRuns, wakeLogs <-chan int64
	if s.Wake != nil {
		wakeRuns = s.Wake.Subscribe(ctx, wake.TopicRuns)
		wakeLogs = s.Wake.Subscribe(ctx, wake.TopicLogs)
	}
	poll := time.NewTicker(s.StreamPollInterval)
	defer poll.Stop()
	keepAlive := time.NewTicker(s.StreamKeepAlive)
	defer keepAlive.Stop()

	for {
		if final {
			send(frame("end", "", stream.lastState))
			return
		}
		// Wait for a reason to look again.
		select {
		case <-ctx.Done():
			return
		case <-keepAlive.C:
			// A named event, not a comment: EventSource never shows comments to the page, so the web
			// app could not tell a quiet stream from a dead one.
			if !send(frame("ping", "", []byte("{}"))) {
				return
			}
			continue
		case id, open := <-wakeRuns:
			if !open {
				wakeRuns = nil
			}
			if open && id != run.ID {
				continue
			}
		case id, open := <-wakeLogs:
			if !open {
				wakeLogs = nil
			}
			if open && id != run.ID {
				continue
			}
		case <-poll.C:
		}
		// Pages already written are valid and the cursor moved past them; on an
		// error the stream ends and the browser reconnects with Last-Event-ID.
		final, err = stream.collect(ctx, &out, send)
		if err != nil {
			if !errors.Is(err, errClientGone) {
				if out.Len() > 0 {
					send(out.Bytes()) // a state change read before the failure
				}
				if ctx.Err() == nil {
					s.Log.Error("event stream ended", "run", run.ID, "error", err.Error())
				}
			}
			return
		}
	}
}

// errClientGone means a write to the client failed: the stream is over.
var errClientGone = errors.New("event stream client is gone")

// collect writes the events for what changed since the last call through send:
// a state event when the state differs, then log events one page at a time
// until the log is read to its end. It reports whether the run was final when
// its state was read; the log is read after the state, so a final run's lines
// are all included. out is scratch space reused between calls: it holds the
// state event and then one page, never more.
//
// The state event is held back until the first page is read (or there is none),
// so that an error reading the first page is reported before any byte is sent.
func (e *eventStream) collect(ctx context.Context, out *bytes.Buffer, send func([]byte) bool) (final bool, err error) {
	out.Reset()
	run, err := store.GetRun(ctx, e.s.Pool, e.run.ID)
	if err != nil {
		return false, err
	}
	state, err := e.s.buildState(ctx, run, e.steps)
	if err != nil {
		return false, err
	}
	body, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	changed := !bytes.Equal(body, e.lastState)
	if changed {
		out.Write(frame("state", "", body))
	}
	final = finalRuns[run.State]
	flush := func() error {
		if out.Len() == 0 {
			return nil
		}
		ok := send(out.Bytes())
		out.Reset()
		if !ok {
			return errClientGone
		}
		return nil
	}
	if e.withLogs {
		for {
			page, err := e.session.Read(ctx, logs.Filter{}, e.cursor, e.s.StreamBatchLines)
			if err != nil {
				return false, err
			}
			if len(page.Lines) == 0 {
				break
			}
			data, err := json.Marshal(shapeLogPage(page))
			if err != nil {
				return false, err
			}
			out.Write(frame("log", string(page.Next), data))
			if err := flush(); err != nil {
				return false, err
			}
			e.cursor = page.Next
			if changed {
				e.lastState = body // sent together with the first page
				changed = false
			}
			if len(page.Lines) < e.s.StreamBatchLines {
				break
			}
		}
	}
	if err := flush(); err != nil {
		return false, err
	}
	if changed {
		e.lastState = body
	}
	return final, nil
}

// frame is one Server-Sent Event. data is compact JSON, which has no line
// breaks, so one data line holds it.
func frame(event, id string, data []byte) []byte {
	var b bytes.Buffer
	b.WriteString("event: " + event + "\n")
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	b.WriteString("data: ")
	b.Write(data)
	b.WriteString("\n\n")
	return b.Bytes()
}
