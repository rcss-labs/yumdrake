//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rcss-labs/yamdrake/internal/store"
)

const (
	planChangedMessage    = "The plan changed. Reload and review it again."
	alreadyDecidedMessage = "This run was already decided."
)

// waitingRun makes a run in the given environment that waits for approval,
// asked for by userID, with an open approval for its manifest. It sets the
// state directly, as the other run tests do; the engine's own tests cover how
// a run gets there.
func (f *runFixture) waitingRun(userID int64, environment string) (id int64, manifestID string) {
	f.t.Helper()
	id = f.newRun(f.jobID, environment, "s1")
	manifestID = "manifest-" + itoa(id)
	f.exec(`UPDATE runs SET state = 'waiting_approval', manifest_id = $2, approval_required = true, requested_by = $3 WHERE id = $1`, id, manifestID, userID)
	now := time.Now()
	if _, err := store.RequestApproval(context.Background(), f.pool, store.Approval{
		RunID: id, ManifestID: manifestID, RequestedBy: userID, RequestedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		f.t.Fatalf("RequestApproval: %v", err)
	}
	return id, manifestID
}

func TestApprovalsListShowsCanApproveAndReason(t *testing.T) {
	f := newRunFixture(t)
	starterUser := f.newUser("starter")
	// The starter is a person with an approve grant who also started the run.
	f.grant(starterUser.ID, "approve")
	own, _ := f.newToken(starterUser.ID)
	approver := f.userWith("approver", store.Grant{Action: "approve", Label: "prod"})
	devOnly := f.userWith("devonly", store.Grant{Action: "approve", Label: "dev"})

	// An empty list is {"approvals": []}, not null.
	_, raw := f.doRaw("GET", "/api/v1/approvals", f.adminToken, nil)
	if strings.TrimSpace(string(raw)) != `{"approvals":[]}` {
		t.Fatalf("empty approvals = %s, want {\"approvals\":[]}", raw)
	}

	runID, manifestID := f.waitingRun(starterUser.ID, prodEnvironment)
	// A run that is not waiting any more is not listed, even with an open approval left behind.
	staleID, _ := f.waitingRun(starterUser.ID, prodEnvironment)
	f.exec(`UPDATE runs SET state = 'cancelled' WHERE id = $1`, staleID)

	list := func(token string) []map[string]any {
		t.Helper()
		code, body := f.doRaw("GET", "/api/v1/approvals", token, nil)
		if code != http.StatusOK {
			t.Fatalf("GET /api/v1/approvals = %d %s", code, body)
		}
		var wrapper struct {
			Approvals []map[string]any `json:"approvals"`
		}
		if err := json.Unmarshal(body, &wrapper); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return wrapper.Approvals
	}
	single := func(who string, items []map[string]any) map[string]any {
		t.Helper()
		if len(items) != 1 {
			t.Fatalf("%s sees %d approvals %v, want 1", who, len(items), items)
		}
		return items[0]
	}

	got := single("approver", list(approver))
	for key, want := range map[string]any{
		"run_id": float64(runID), "job_id": "release", "job_name": "release", "environment": prodEnvironment, "label": "prod",
		"manifest_id": manifestID, "requested_by": float64(starterUser.ID),
		"requested_by_name": "starter", "can_approve": true, "reason": "",
	} {
		if got[key] != want {
			t.Errorf("approver sees %s = %v, want %v", key, got[key], want)
		}
	}
	if want := []any{map[string]any{"service": "s1", "version": "1.0"}}; !reflect.DeepEqual(got["versions"], want) {
		t.Errorf("approver sees versions = %v, want %v", got["versions"], want)
	}
	// A requester with no name is shown by email.
	f.exec(`UPDATE users SET name = '' WHERE id = $1`, starterUser.ID)
	if nameless := single("approver", list(approver)); nameless["requested_by_name"] != starterUser.Email {
		t.Errorf("requested_by_name of a nameless requester = %v, want the email %q", nameless["requested_by_name"], starterUser.Email)
	}
	for _, key := range []string{"requested_at", "expires_at"} {
		if s, _ := got[key].(string); s == "" {
			t.Errorf("%s = %v, want a time", key, got[key])
		}
	}
	// The run the approval belongs to is named by its source too.
	if run := f.mustDo("GET", runPath(runID, ""), nil, http.StatusOK); got["job_source_id"] != run["job_source_id"] || got["job_source_id"] == nil {
		t.Errorf("job_source_id = %v, want the run's %v", got["job_source_id"], run["job_source_id"])
	}

	ownView := single("the starter", list(own))
	if ownView["can_approve"] != false || ownView["reason"] != "You started this run." {
		t.Errorf("starter sees can_approve %v reason %q, want false and the own-run reason", ownView["can_approve"], ownView["reason"])
	}
	devView := single("dev-only approver", list(devOnly))
	if devView["can_approve"] != false || devView["reason"] != "You do not have approve permission in prod." {
		t.Errorf("dev-only approver sees can_approve %v reason %q, want false and the label reason", devView["can_approve"], devView["reason"])
	}
	adminView := single("admin", list(f.adminToken))
	if adminView["can_approve"] != true {
		t.Errorf("admin can_approve = %v, want true", adminView["can_approve"])
	}
}

func TestApproveAndRejectEndpoints(t *testing.T) {
	f := newRunFixture(t)
	starter := f.newUser("starter")
	approver := f.userWith("approver", store.Grant{Action: "approve", Label: "prod"})
	devOnly := f.userWith("devonly", store.Grant{Action: "approve", Label: "dev"})
	nobody := f.userWith("nobody")
	f.grant(starter.ID, "approve")
	ownToken, _ := f.newToken(starter.ID)

	runID, manifestID := f.waitingRun(starter.ID, prodEnvironment)
	approvePath, rejectPath := runPath(runID, "/approve"), runPath(runID, "/reject")
	body := map[string]any{"manifest_id": manifestID, "comment": "checked"}

	t.Run("permission", func(t *testing.T) {
		if code, _ := f.post("", approvePath, body); code != http.StatusUnauthorized {
			t.Errorf("without credentials = %d, want 401", code)
		}
		for name, token := range map[string]string{"no grant": nobody, "approve scoped to another label": devOnly, "own run": ownToken} {
			if code, got := f.post(token, approvePath, body); code != http.StatusForbidden || errorCode(t, got) != "forbidden" {
				t.Errorf("approve with %s = %d %v, want 403 forbidden", name, code, got)
			}
			if code, got := f.post(token, rejectPath, map[string]any{}); code != http.StatusForbidden || errorCode(t, got) != "forbidden" {
				t.Errorf("reject with %s = %d %v, want 403 forbidden", name, code, got)
			}
		}
		if code, _ := f.post(approver, runPath(999999, "/approve"), body); code != http.StatusNotFound {
			t.Errorf("approve of an unknown run = %d, want 404", code)
		}
		if got := f.mustDo("GET", runPath(runID, ""), nil, http.StatusOK); got["state"] != "waiting_approval" {
			t.Errorf("refused requests changed the run to %v", got["state"])
		}
		if code, _ := f.post(approver, approvePath, map[string]any{"comment": "no manifest"}); code != http.StatusBadRequest {
			t.Errorf("approve without manifest_id = %d, want 400", code)
		}
	})

	t.Run("a stale plan is a 409 with the plan's words", func(t *testing.T) {
		code, got := f.post(approver, approvePath, map[string]any{"manifest_id": "an-older-plan"})
		if code != http.StatusConflict || errorCode(t, got) != "conflict" || errorMessage(got) != planChangedMessage {
			t.Errorf("stale approve = %d %v, want 409 conflict %q", code, got, planChangedMessage)
		}
		if run := f.mustDo("GET", runPath(runID, ""), nil, http.StatusOK); run["state"] != "waiting_approval" {
			t.Errorf("a stale approval moved the run to %v", run["state"])
		}
	})

	t.Run("approve answers with the run and its approval", func(t *testing.T) {
		code, got := f.post(approver, approvePath, body)
		if code != http.StatusOK || got["state"] != "queued" {
			t.Fatalf("approve = %d %v, want 200 and a queued run", code, got)
		}
		approval, _ := got["approval"].(map[string]any)
		if approval["decision"] != "approved" || approval["comment"] != "checked" || approval["manifest_id"] != manifestID ||
			approval["requested_by"] != float64(starter.ID) || approval["decided_by"] == nil || approval["decided_at"] == nil {
			t.Errorf("approval = %v, want approved with who, when, comment and manifest", approval)
		}
		if approval["requested_by_name"] != starter.Name || approval["decided_by_name"] != "approver" {
			t.Errorf("approval names = %v and %v, want %q and \"approver\"", approval["requested_by_name"], approval["decided_by_name"], starter.Name)
		}
		for _, key := range []string{"requested_at", "expires_at"} {
			if s, _ := approval[key].(string); s == "" {
				t.Errorf("approval %s = %v, want a time", key, approval[key])
			}
		}
		for _, p := range []struct {
			path string
			body any
		}{{approvePath, body}, {rejectPath, map[string]any{}}} {
			code, got := f.post(approver, p.path, p.body)
			if code != http.StatusConflict || errorCode(t, got) != "conflict" || errorMessage(got) != alreadyDecidedMessage {
				t.Errorf("%s after the decision = %d %v, want 409 conflict %q", p.path, code, got, alreadyDecidedMessage)
			}
		}
	})

	t.Run("reject ends the run", func(t *testing.T) {
		id, _ := f.waitingRun(starter.ID, prodEnvironment)
		code, got := f.post(approver, runPath(id, "/reject"), map[string]any{"comment": "wrong version"})
		if code != http.StatusOK || got["state"] != "rejected" {
			t.Fatalf("reject = %d %v, want 200 and a rejected run", code, got)
		}
		approval, _ := got["approval"].(map[string]any)
		if approval["decision"] != "rejected" || approval["comment"] != "wrong version" {
			t.Errorf("approval = %v, want rejected with the comment", approval)
		}
	})

	t.Run("both decisions are audited once", func(t *testing.T) {
		count := map[string]int{}
		for _, a := range f.auditActions() {
			count[a]++
		}
		if count["run.approve"] != 1 || count["run.reject"] != 1 {
			t.Errorf("audit counts approve=%d reject=%d, want 1 and 1", count["run.approve"], count["run.reject"])
		}
	})
}

func TestDecidingAnExpiredApprovalAnswersConflict(t *testing.T) {
	f := newRunFixture(t)
	approver := f.userWith("approver", store.Grant{Action: "approve", Label: "prod"})
	const expiredMessage = "The approval expired before the decision. The run has been expired."

	for _, action := range []string{"approve", "reject"} {
		t.Run(action, func(t *testing.T) {
			runID, manifestID := f.waitingRun(f.admin.ID, prodEnvironment)
			// Move the open approval's expiry into the past, as the engine test does, without waiting for the sweep.
			f.exec(`UPDATE approvals SET expires_at = now() - interval '1 minute' WHERE run_id = $1 AND decision = ''`, runID)

			body := map[string]any{"manifest_id": manifestID}
			if action == "reject" {
				body = map[string]any{} // the reject body takes no manifest id, as in TestApproveAndRejectEndpoints
			}
			code, got := f.post(approver, runPath(runID, "/"+action), body)
			if code != http.StatusConflict || errorCode(t, got) != "conflict" || errorMessage(got) != expiredMessage {
				t.Errorf("%s of an expired approval = %d %v, want 409 conflict %q", action, code, got, expiredMessage)
			}
			if run := f.mustDo("GET", runPath(runID, ""), nil, http.StatusOK); run["state"] != "expired" {
				t.Errorf("run after %s of an expired approval = %v, want expired", action, run["state"])
			}
		})
	}
}

func TestRunViewShowsAWithdrawnApprovalOfACancelledRun(t *testing.T) {
	f := newRunFixture(t)
	runID, _ := f.waitingRun(f.admin.ID, prodEnvironment)
	f.mustDo("POST", runPath(runID, "/cancel"), nil, http.StatusOK)
	got := f.mustDo("GET", runPath(runID, ""), nil, http.StatusOK)
	approval, _ := got["approval"].(map[string]any)
	if got["state"] != "cancelled" || approval["decision"] != "withdrawn" || approval["decided_by"] != nil {
		t.Errorf("state %v approval %v, want a cancelled run with a withdrawn approval decided by nobody", got["state"], approval)
	}
	if _, raw := f.doRaw("GET", "/api/v1/approvals", f.adminToken, nil); strings.TrimSpace(string(raw)) != `{"approvals":[]}` {
		t.Errorf("approvals list = %s, want {\"approvals\":[]}", raw)
	}
}

func TestRunViewShowsTheApproval(t *testing.T) {
	f := newRunFixture(t)
	plain := f.newRun(f.jobID, devEnvironment, "s1")
	if got := f.mustDo("GET", runPath(plain, ""), nil, http.StatusOK); got["approval"] != nil {
		t.Errorf("a run that never asked has approval %v, want null", got["approval"])
	}
	_, raw := f.doRaw("GET", runPath(plain, ""), f.adminToken, nil)
	if found := nullSlices(reflect.TypeFor[runDetailView](), raw, ""); len(found) > 0 {
		t.Errorf("run detail has null where a list belongs: %v", found)
	}

	runID, manifestID := f.waitingRun(f.admin.ID, prodEnvironment)
	got := f.mustDo("GET", runPath(runID, ""), nil, http.StatusOK)
	approval, _ := got["approval"].(map[string]any)
	if approval["decision"] != "" || approval["decided_by"] != nil || approval["decided_at"] != nil ||
		approval["manifest_id"] != manifestID || approval["requested_by"] != float64(f.admin.ID) {
		t.Errorf("open approval = %v, want undecided with null decided_by and decided_at", approval)
	}
	if approval["requested_by_name"] != f.admin.Name || approval["decided_by_name"] != nil {
		t.Errorf("open approval names = %v and %v, want the requester's name and null decided_by_name", approval["requested_by_name"], approval["decided_by_name"])
	}
}
