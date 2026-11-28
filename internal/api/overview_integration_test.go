//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestOverviewValidatesDaysAndLabel(t *testing.T) {
	f := newRunFixture(t)
	for _, tc := range []struct{ query, message string }{
		{"?days=0", "days must be 7, 14 or 30"},
		{"?days=10", "days must be 7, 14 or 30"},
		{"?days=abc", "days must be 7, 14 or 30"},
		{"?days=", "days must be 7, 14 or 30"},
		{"?label=nowhere", "unknown label"},
		{"?label=Prod", "unknown label"},
	} {
		code, body := f.get(f.adminToken, "/api/v1/analytics/overview"+tc.query)
		if code != http.StatusBadRequest || errorCode(t, body) != "invalid" || errorMessage(body) != tc.message {
			t.Errorf("%s = %d %v, want 400 invalid %q", tc.query, code, body, tc.message)
		}
	}
	for _, query := range []string{"", "?days=7", "?days=14&label=prod", "?days=30&label=dev", "?label="} {
		if code, body := f.get(f.adminToken, "/api/v1/analytics/overview"+query); code != http.StatusOK {
			t.Errorf("%s = %d %v, want 200", query, code, body)
		}
	}
}

func TestOverviewWithNoRunsHasEmptyListsAndNullFigures(t *testing.T) {
	f := newRunFixture(t)
	code, raw := f.doRaw("GET", "/api/v1/analytics/overview", f.adminToken, nil)
	if code != http.StatusOK {
		t.Fatalf("overview = %d %s", code, raw)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, name := range []string{"success_rate", "prev_success_rate", "median_duration_seconds", "prev_median_duration_seconds", "oldest_waiting"} {
		if string(body[name]) != "null" {
			t.Errorf("%s = %s, want null", name, body[name])
		}
	}
	for _, name := range []string{"top_failing", "attention"} {
		if string(body[name]) != "[]" {
			t.Errorf("%s = %s, want []", name, body[name])
		}
	}
	var days []map[string]any
	if err := json.Unmarshal(body["days"], &days); err != nil || len(days) != 14 {
		t.Errorf("days: %v, %d entries, want 14 (the default)", err, len(days))
	}
	var environments []map[string]any
	if err := json.Unmarshal(body["environments"], &environments); err != nil || len(environments) != 2 {
		t.Fatalf("environments: %v %s", err, body["environments"])
	}
	if first := environments[0]; first["environment"] != devEnvironment || first["label"] != "dev" || first["run_id"] != nil || first["at"] != nil || first["state"] != "" {
		t.Errorf("environments[0] = %v, want the dev environment with no run", first)
	}
}

func TestOverviewShapeWithRuns(t *testing.T) {
	f := newRunFixture(t)
	failed := f.newRun(f.jobID, prodEnvironment, "s1")
	ok := f.newRun(f.jobID, devEnvironment, "s2")
	waiting := f.newRun(f.jobID, prodEnvironment, "s3")
	started := time.Now().UTC().Add(-3 * time.Hour)
	f.exec(`UPDATE runs SET state = 'failed', started_at = $2, finished_at = $3, end_reason = 'a step failed' WHERE id = $1`, failed, started, started.Add(90*time.Second+400*time.Millisecond))
	f.exec(`UPDATE runs SET state = 'succeeded', started_at = $2, finished_at = $3 WHERE id = $1`, ok, started, started.Add(30*time.Second))
	f.exec(`UPDATE runs SET state = 'waiting_approval' WHERE id = $1`, waiting)

	code, raw := f.doRaw("GET", "/api/v1/analytics/overview?days=7&label=prod", f.adminToken, nil)
	if code != http.StatusOK {
		t.Fatalf("overview = %d %s", code, raw)
	}
	var got struct {
		Runs                  int
		WaitingApproval       int      `json:"waiting_approval"`
		SuccessRate           *float64 `json:"success_rate"`
		MedianDurationSeconds *int64   `json:"median_duration_seconds"`
		OldestWaiting         *string  `json:"oldest_waiting"`
		Days                  []struct {
			Date                     string
			Succeeded, Failed, Other int
		}
		Attention []struct {
			RunID                                          int64 `json:"run_id"`
			State, Job, Environment, Label, Reason, By, At string
		}
		Environments []struct {
			Environment, Label, State, Job string
			RunID                          *int64 `json:"run_id"`
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Runs != 2 || got.WaitingApproval != 1 || got.OldestWaiting == nil {
		t.Errorf("runs %d, waiting %d, oldest %v, want 2, 1 and a time (label prod: the failed and the waiting run)", got.Runs, got.WaitingApproval, got.OldestWaiting)
	}
	if got.SuccessRate == nil || *got.SuccessRate != 0 {
		t.Errorf("success_rate = %v, want 0 (one finished run, failed)", got.SuccessRate)
	}
	if got.MedianDurationSeconds == nil || *got.MedianDurationSeconds != 90 {
		t.Errorf("median_duration_seconds = %v, want 90 (rounded from 90.4)", got.MedianDurationSeconds)
	}
	if len(got.Days) != 7 {
		t.Errorf("days has %d entries, want 7", len(got.Days))
	}
	if len(got.Attention) != 2 || got.Attention[0].RunID != waiting || got.Attention[0].Reason != "Waiting for approval" ||
		got.Attention[1].RunID != failed || got.Attention[1].Reason != "a step failed" || got.Attention[1].Job != "release" || got.Attention[1].By == "" {
		t.Errorf("attention = %+v, want the waiting run then the failed one", got.Attention)
	}
	if len(got.Environments) != 1 || got.Environments[0].Environment != prodEnvironment || got.Environments[0].RunID == nil {
		t.Errorf("environments = %+v, want only %s with a run", got.Environments, prodEnvironment)
	}
}
