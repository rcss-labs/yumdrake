//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/rcss-labs/yamdrake/internal/store"
)

func awsRoleBody(name, account string) map[string]any {
	role := map[string]any{"method": "role", "role_arn": "arn:aws:iam::" + account + ":role/yumdrake"}
	return map[string]any{"name": name, "account_id": account, "discovery": role, "deploy": role}
}

func awsKeyCredential(keyID, secret string) map[string]any {
	return map[string]any{"method": "key", "access_key_id": keyID, "secret_access_key": secret}
}

func TestCreateAccountNeverReturnsSecretAndAudits(t *testing.T) {
	h := newHarness(t)
	const (
		discoveryKeyID  = "AKIADISCOVERYKEY0001"
		discoverySecret = "discovery/Secret+AccessKey/0123456789abcdef"
		deployKeyID     = "AKIADEPLOYKEYXXXX002"
		deploySecret    = "deploy/Secret+AccessKey/fedcba9876543210"
	)
	body := h.mustDo("POST", "/api/v1/admin/aws-accounts", map[string]any{
		"name": "prod", "account_id": "123456789012",
		"discovery": awsKeyCredential(discoveryKeyID, discoverySecret),
		"deploy":    awsKeyCredential(deployKeyID, deploySecret),
	}, http.StatusCreated)
	h.mustDo("GET", "/api/v1/admin/aws-accounts", nil, http.StatusOK)

	// The answer has the shape the web screen reads, and nothing more.
	if body["name"] != "prod" || body["account_id"] != "123456789012" || body["id"] == nil || body["created_at"] == nil {
		t.Fatalf("created account = %v", body)
	}
	discovery, _ := body["discovery"].(map[string]any)
	if len(discovery) != 4 || discovery["method"] != "key" || discovery["access_key_id_last4"] != "0001" ||
		discovery["role_arn"] != "" || discovery["external_id"] != "" {
		t.Errorf("discovery view = %v", discovery)
	}
	if envs, ok := body["environments"].([]any); !ok || len(envs) != 0 {
		t.Errorf("environments = %v, want []", body["environments"])
	}

	// No response and no audit detail holds a secret, or more of a key id than its last 4.
	var searched []string
	searched = append(searched, h.bodies...)
	searched = append(searched, h.logs.String())
	for _, e := range h.auditEntries() {
		searched = append(searched, e.Action+" "+e.TargetType+" "+e.TargetID+" "+string(e.Detail))
	}
	if len(h.bodies) != 2 || len(searched) < 4 {
		t.Fatalf("searched %d responses and %d texts, want 2 and at least 3", len(h.bodies), len(searched))
	}
	forbidden := []string{discoverySecret, deploySecret, discoveryKeyID, deployKeyID,
		discoveryKeyID[:len(discoveryKeyID)-4], deployKeyID[:len(deployKeyID)-4],
		strings.Split(discoverySecret, "/")[1], strings.Split(deploySecret, "/")[1]}
	for i, text := range searched {
		for _, bad := range forbidden {
			if strings.Contains(text, bad) {
				t.Errorf("text %d contains %q: %s", i, bad, text)
			}
		}
	}
	if h.auditCount("aws_account.create") != 1 {
		t.Errorf("audit actions = %v, want one aws_account.create", h.auditActions())
	}

	// What is stored is ciphertext that only decrypts under its own purpose.
	ctx := context.Background()
	storedDiscovery, err := store.DiscoverySecret(ctx, h.pool, "123456789012")
	if err != nil {
		t.Fatalf("DiscoverySecret: %v", err)
	}
	// The orchestrator code has no reader of this column (it is read only in
	// internal/awsx/deploy), so the test queries it directly.
	var storedDeploy []byte
	if err := h.pool.QueryRow(ctx, "SELECT deploy_secret_ciphertext FROM aws_accounts WHERE account_id = $1", "123456789012").Scan(&storedDeploy); err != nil {
		t.Fatalf("read the deploy ciphertext: %v", err)
	}
	if plain, err := h.cipher.Decrypt(ctx, "aws-discovery", storedDiscovery); err != nil || string(plain) != discoverySecret {
		t.Errorf("discovery ciphertext under aws-discovery = %q, %v", plain, err)
	}
	if plain, err := h.cipher.Decrypt(ctx, "aws-deploy", storedDeploy); err != nil || string(plain) != deploySecret {
		t.Errorf("deploy ciphertext under aws-deploy = %q, %v", plain, err)
	}
	if _, err := h.cipher.Decrypt(ctx, "aws-deploy", storedDiscovery); err == nil {
		t.Error("the discovery secret decrypts as a deployment secret")
	}
	if _, err := h.cipher.Decrypt(ctx, "aws-discovery", storedDeploy); err == nil {
		t.Error("the deployment secret decrypts as a discovery secret")
	}
	if strings.Contains(string(storedDiscovery), discoverySecret) || strings.Contains(string(storedDeploy), deploySecret) {
		t.Error("a stored secret is plaintext")
	}
}

func TestCreateAccountValidatesInputAndRejectsDuplicates(t *testing.T) {
	h := newHarness(t)
	role := func(account string) map[string]any { return awsRoleBody("x", account)["discovery"].(map[string]any) }
	with := func(change func(map[string]any)) map[string]any {
		b := awsRoleBody("valid", "111111111111")
		change(b)
		return b
	}
	for name, body := range map[string]map[string]any{
		"no name":            with(func(b map[string]any) { b["name"] = " " }),
		"short account id":   with(func(b map[string]any) { b["account_id"] = "12345" }),
		"letters in account": with(func(b map[string]any) { b["account_id"] = "12345678901a" }),
		"unknown method":     with(func(b map[string]any) { b["discovery"] = map[string]any{"method": "sso"} }),
		"role without arn":   with(func(b map[string]any) { b["deploy"] = map[string]any{"method": "role"} }),
		"role arn not a role": with(func(b map[string]any) {
			b["deploy"] = map[string]any{"method": "role", "role_arn": "arn:aws:s3:::bucket"}
		}),
		"role with a key": with(func(b map[string]any) {
			b["discovery"] = map[string]any{"method": "role", "role_arn": role("111111111111")["role_arn"], "secret_access_key": "leak-check-secret"}
		}),
		"key id too short to hide": with(func(b map[string]any) {
			b["discovery"] = awsKeyCredential("AKIA12345", "s3cret-value")
		}),
		"key without secret": with(func(b map[string]any) {
			b["discovery"] = map[string]any{"method": "key", "access_key_id": "AKIAEXAMPLE0001"}
		}),
		"key with a role": with(func(b map[string]any) {
			b["discovery"] = map[string]any{"method": "key", "access_key_id": "AKIAEXAMPLE0002", "secret_access_key": "s3cret-value", "role_arn": "arn:aws:iam::111111111111:role/x"}
		}),
		"unknown field":        with(func(b map[string]any) { b["extra"] = true }),
		"unknown nested field": with(func(b map[string]any) { b["deploy"].(map[string]any)["session_token"] = "t" }),
		"control character":    with(func(b map[string]any) { b["name"] = "bad\x07name" }),
		"missing deploy":       with(func(b map[string]any) { delete(b, "deploy") }),
	} {
		code, got := h.do("POST", "/api/v1/admin/aws-accounts", body)
		if code != http.StatusBadRequest || errorCode(t, got) != "invalid" {
			t.Errorf("%s: POST = %d %v, want 400 invalid", name, code, got)
		}
	}
	for _, body := range h.bodies {
		if strings.Contains(body, "leak-check-secret") || strings.Contains(body, "s3cret-value") {
			t.Errorf("an error response repeats a submitted secret: %s", body)
		}
	}
	if len(h.auditActions()) != 0 {
		t.Errorf("refused requests were audited: %v", h.auditActions())
	}

	h.mustDo("POST", "/api/v1/admin/aws-accounts", awsRoleBody("one", "111111111111"), http.StatusCreated)
	for name, body := range map[string]map[string]any{
		"same name":       awsRoleBody("one", "222222222222"),
		"same account id": awsRoleBody("two", "111111111111"),
	} {
		code, got := h.do("POST", "/api/v1/admin/aws-accounts", body)
		if code != http.StatusConflict || errorCode(t, got) != "conflict" {
			t.Errorf("%s: POST = %d %v, want 409 conflict", name, code, got)
		}
	}
	if n := h.auditCount("aws_account.create"); n != 1 {
		t.Errorf("aws_account.create audited %d times, want 1 (a duplicate rolls its audit entry back)", n)
	}
}

func TestSavingADeployRoleFromAnotherAccountIsRefused(t *testing.T) {
	h := newHarness(t)
	for _, which := range []string{"discovery", "deploy"} {
		body := awsRoleBody("wrong-"+which, "111111111111")
		body[which] = map[string]any{"method": "role", "role_arn": "arn:aws:iam::999999999999:role/yumdrake"}
		code, got := h.do("POST", "/api/v1/admin/aws-accounts", body)
		if code != http.StatusUnprocessableEntity || errorCode(t, got) != "invalid" {
			t.Fatalf("%s role of another account: POST = %d %v, want 422 invalid", which, code, got)
		}
		problems, _ := got["error"].(map[string]any)["problems"].([]any)
		first, _ := problems[0].(map[string]any)
		message, _ := first["message"].(string)
		if len(problems) != 1 || first["file"] != which+".role_arn" || !strings.Contains(message, "999999999999") || !strings.Contains(message, "111111111111") {
			t.Errorf("%s problems = %v, want one problem on %s.role_arn naming both accounts", which, problems, which)
		}
	}
	if n := h.auditCount("aws_account.create"); n != 0 {
		t.Errorf("refused saves were audited %d times", n)
	}
	// A role of the connection's own account is fine.
	h.mustDo("POST", "/api/v1/admin/aws-accounts", awsRoleBody("right", "111111111111"), http.StatusCreated)
}

func TestListAccountsNamesTheEnvironmentsUsingEach(t *testing.T) {
	h := newHarness(t)
	h.pushInventory("c1")
	h.setInventorySource()
	// The fixture inventory has environments on 111111111111, 222222222222 (two) and 333333333333.
	h.mustDo("POST", "/api/v1/admin/aws-accounts", awsRoleBody("shared", "222222222222"), http.StatusCreated)
	h.mustDo("POST", "/api/v1/admin/aws-accounts", awsRoleBody("unused", "444444444444"), http.StatusCreated)

	_, raw := h.doRaw("GET", "/api/v1/admin/aws-accounts", h.adminToken, nil)
	var list struct {
		Accounts []struct {
			Name         string   `json:"name"`
			Environments []string `json:"environments"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string][]string{}
	for _, a := range list.Accounts {
		got[a.Name] = a.Environments
	}
	if len(list.Accounts) != 2 || strings.Join(got["shared"], ",") != "dev-02,dev-03" || got["unused"] == nil || len(got["unused"]) != 0 {
		t.Errorf("accounts = %v, want shared: dev-02,dev-03 and unused: []", got)
	}
}

func TestTestConnectionOfAnUnknownAccountIsNotFound(t *testing.T) {
	h := newHarness(t)
	code, body := h.do("POST", "/api/v1/admin/aws-accounts/424242/test", nil)
	if code != http.StatusNotFound || errorCode(t, body) != "not_found" {
		t.Fatalf("test of an unknown account = %d %v, want 404", code, body)
	}
}

// A connection test that cannot reach AWS says so with a code, never with the
// secret it used, and is audited without it.
func TestTestConnectionFailureReportsACodeNotTheSecret(t *testing.T) {
	h := newHarness(t)
	const secret = "unreachable/Secret+Key/0123456789"
	cfg := aws.Config{Region: "us-east-1", BaseEndpoint: aws.String("http://127.0.0.1:1"), RetryMaxAttempts: 1,
		Credentials: credentials.NewStaticCredentialsProvider("base", "base", "")}
	h.rebuild(func(d *Deps) { d.AWSConfig = cfg })
	created := h.mustDo("POST", "/api/v1/admin/aws-accounts", map[string]any{
		"name": "down", "account_id": "123456789012",
		"discovery": awsKeyCredential("AKIAUNREACHABLE1234", secret), "deploy": awsKeyCredential("AKIADEPLOY0000005678", "other-secret-0000"),
	}, http.StatusCreated)

	result := h.mustDo("POST", "/api/v1/admin/aws-accounts/"+itoa(int64(created["id"].(float64)))+"/test", nil, http.StatusOK)
	if result["ok"] != false || result["error_code"] == "" || result["error_code"] == nil {
		t.Fatalf("result = %v, want ok false with an error code", result)
	}
	// The test records the outcome for the System health page: the code only.
	checks, err := store.ListConnectionChecks(context.Background(), h.pool)
	if err != nil {
		t.Fatalf("ListConnectionChecks: %v", err)
	}
	if len(checks) != 1 || checks[0].Kind != "aws" || checks[0].Ref != "123456789012" || checks[0].LastErrorAt == nil ||
		checks[0].LastErrorCode != result["error_code"] || checks[0].LastOKAt != nil {
		t.Errorf("connection checks = %+v, want one aws check for 123456789012 failed with %v", checks, result["error_code"])
	}
	// The discovery and the deployment credential are reported apart. The
	// orchestrator never holds deployment credentials, so it can only say what
	// the runner will check.
	discovery, _ := result["discovery"].(map[string]any)
	deploy, _ := result["deploy"].(map[string]any)
	if discovery["ok"] != false || discovery["error_code"] == "" || deploy["ok"] != true || deploy["message"] == "" {
		t.Errorf("discovery = %v, deploy = %v, want the discovery failure and a separate deploy result", discovery, deploy)
	}
	texts := append([]string{h.logs.String()}, h.bodies...)
	for _, e := range h.auditEntries() {
		texts = append(texts, string(e.Detail))
	}
	for i, text := range texts {
		for _, bad := range []string{secret, "AKIAUNREACHABLE1234", "other-secret-0000"} {
			if strings.Contains(text, bad) {
				t.Errorf("text %d contains %q: %s", i, bad, text)
			}
		}
	}
	if h.auditCount("aws_account.test") != 1 {
		t.Errorf("audit actions = %v, want one aws_account.test", h.auditActions())
	}
}

func TestAccountIDAndNameAreTrimmed(t *testing.T) {
	h := newHarness(t)
	body := awsRoleBody("  padded  ", "111111111111")
	body["account_id"] = " 111111111111 "
	got := h.mustDo("POST", "/api/v1/admin/aws-accounts", body, http.StatusCreated)
	if got["name"] != "padded" || got["account_id"] != "111111111111" {
		t.Errorf("created = %v, want trimmed name and account id", got)
	}
}
