package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/jackc/pgx/v5"

	"github.com/rcss-labs/yamdrake/internal/audit"
	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
)

const awsTestTimeout = 20 * time.Second

var (
	awsAccountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)
	awsRoleARNPattern   = regexp.MustCompile(`^arn:aws(?:-[a-z]+)*:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]{1,512}$`)
	awsExternalIDRegexp = regexp.MustCompile(`^[A-Za-z0-9+=,.@:/_-]+$`)
	awsAccessKeyPattern = regexp.MustCompile(`^[A-Za-z0-9]{12,128}$`)
)

// awsCredentialRequest is what an administrator submits for one credential.
// secret_access_key is write-only: no response or audit entry has it.
type awsCredentialRequest struct {
	Method          string `json:"method"`
	RoleARN         string `json:"role_arn"`
	ExternalID      string `json:"external_id"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

type awsAccountRequest struct {
	Name      string               `json:"name"`
	AccountID string               `json:"account_id"`
	Discovery awsCredentialRequest `json:"discovery"`
	Deploy    awsCredentialRequest `json:"deploy"`
}

// check validates one credential and returns the secret to encrypt (nil for
// the role method). Messages never repeat a submitted value.
func (c awsCredentialRequest) check(label string) ([]byte, string) {
	switch c.Method {
	case "role":
		switch {
		case !awsRoleARNPattern.MatchString(c.RoleARN):
			return nil, label + ` needs role_arn, an IAM role ARN such as arn:aws:iam::123456789012:role/name`
		case c.ExternalID != "" && (len(c.ExternalID) > 1224 || !awsExternalIDRegexp.MatchString(c.ExternalID)):
			return nil, label + " external_id has characters AWS does not accept or is too long"
		case c.AccessKeyID != "" || c.SecretAccessKey != "":
			return nil, label + ` method "role" takes only role_arn and external_id`
		}
		return nil, ""
	case "key":
		switch {
		case !awsAccessKeyPattern.MatchString(c.AccessKeyID):
			return nil, label + " needs access_key_id (12 to 128 letters and digits)"
		case c.SecretAccessKey == "" || utf8.RuneCountInString(c.SecretAccessKey) > 512 || strings.ContainsAny(c.SecretAccessKey, " \t\r\n"):
			return nil, label + " needs secret_access_key (no spaces, at most 512 characters)"
		case c.RoleARN != "" || c.ExternalID != "":
			return nil, label + ` method "key" takes only access_key_id and secret_access_key`
		}
		return []byte(c.SecretAccessKey), ""
	default:
		return nil, label + ` method must be "role" or "key"`
	}
}

func (c awsCredentialRequest) credential() store.AWSCredential {
	return store.AWSCredential{Method: c.Method, RoleARN: c.RoleARN, ExternalID: c.ExternalID, AccessKeyID: c.AccessKeyID}
}

// awsCredentialView is what the API shows of a credential: never a secret, and
// a key id only by its last 4 characters.
type awsCredentialView struct {
	Method           string `json:"method"`
	RoleARN          string `json:"role_arn"`
	ExternalID       string `json:"external_id"`
	AccessKeyIDLast4 string `json:"access_key_id_last4"`
}

func credentialView(c store.AWSCredential) awsCredentialView {
	last4 := c.AccessKeyID
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	return awsCredentialView{Method: c.Method, RoleARN: c.RoleARN, ExternalID: c.ExternalID, AccessKeyIDLast4: last4}
}

type awsAccountResponse struct {
	ID           int64             `json:"id"`
	Name         string            `json:"name"`
	AccountID    string            `json:"account_id"`
	Discovery    awsCredentialView `json:"discovery"`
	Deploy       awsCredentialView `json:"deploy"`
	Environments []string          `json:"environments"`
	CreatedAt    time.Time         `json:"created_at"`
}

// environmentsByAccount maps an AWS account id to the names of the environments
// of the active inventory that use it.
func (s *server) environmentsByAccount(ctx context.Context) (map[string][]string, error) {
	_, inventory, _, active, err := s.inventoryView(ctx)
	if err != nil || !active {
		return nil, err
	}
	out := map[string][]string{}
	for _, environment := range inventory.Environments {
		out[environment.AWSAccount] = append(out[environment.AWSAccount], environment.Name)
	}
	return out, nil
}

func awsAccountView(a store.AWSAccount, environments map[string][]string) awsAccountResponse {
	names := append([]string{}, environments[a.AccountID]...)
	return awsAccountResponse{ID: a.ID, Name: a.Name, AccountID: a.AccountID,
		Discovery: credentialView(a.Discovery), Deploy: credentialView(a.Deploy), Environments: names, CreatedAt: a.CreatedAt}
}

func (s *server) listAWSAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := store.ListAWSAccounts(r.Context(), s.Pool)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	environments, err := s.environmentsByAccount(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]awsAccountResponse, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, awsAccountView(a, environments))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func (s *server) createAWSAccount(w http.ResponseWriter, r *http.Request) {
	var req awsAccountRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.AccountID = strings.TrimSpace(req.AccountID)
	switch {
	case req.Name == "" || utf8.RuneCountInString(req.Name) > 100:
		invalid(w, "name is required and at most 100 characters")
		return
	case !awsAccountIDPattern.MatchString(req.AccountID):
		invalid(w, "account_id must be 12 digits")
		return
	}
	discoverySecret, problem := req.Discovery.check("discovery")
	if problem == "" {
		var deploySecret []byte
		deploySecret, problem = req.Deploy.check("deploy")
		if problem == "" {
			if problems := req.roleAccountProblems(); len(problems) > 0 {
				var body problemsBody
				body.Error.Code, body.Error.Message, body.Error.Problems = codeInvalid, "a role is in another account than the connection", problems
				writeJSON(w, http.StatusUnprocessableEntity, body)
				return
			}
			s.storeAWSAccount(w, r, req, discoverySecret, deploySecret)
			return
		}
	}
	invalid(w, problem)
}

// roleARNAccount is the account in a role ARN that passed the role pattern.
func roleARNAccount(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return ""
	}
	return parts[4]
}

// roleAccountProblems names every role whose ARN is in another account than
// the connection's: such a role would sign in to, and deploy into, the wrong account.
func (r awsAccountRequest) roleAccountProblems() spec.Problems {
	var problems spec.Problems
	for _, c := range []struct {
		label string
		cred  awsCredentialRequest
	}{{"discovery", r.Discovery}, {"deploy", r.Deploy}} {
		if c.cred.Method != "role" {
			continue
		}
		if got := roleARNAccount(c.cred.RoleARN); got != r.AccountID {
			problems = append(problems, spec.Problem{File: c.label + ".role_arn",
				Message: fmt.Sprintf("the role is in account %s, but the connection is for account %s", got, r.AccountID)})
		}
	}
	return problems
}

func (s *server) storeAWSAccount(w http.ResponseWriter, r *http.Request, req awsAccountRequest, discoverySecret, deploySecret []byte) {
	var err error
	if discoverySecret, err = s.sealAWSSecret(r.Context(), awsx.PurposeDiscovery, discoverySecret); err != nil {
		s.internalError(w, r, fmt.Errorf("encrypt discovery secret: %w", err))
		return
	}
	if deploySecret, err = s.sealAWSSecret(r.Context(), awsx.PurposeDeploy, deploySecret); err != nil {
		s.internalError(w, r, fmt.Errorf("encrypt deployment secret: %w", err))
		return
	}
	var created store.AWSAccount
	err = s.auditedWrite(r, func(ctx context.Context, tx pgx.Tx) (audit.Event, error) {
		var err error
		created, err = store.CreateAWSAccount(ctx, tx, store.AWSAccount{
			Name: req.Name, AccountID: req.AccountID, Discovery: req.Discovery.credential(), Deploy: req.Deploy.credential(),
		}, discoverySecret, deploySecret)
		// Never a secret or a key id: only what identifies the account.
		return audit.Event{Action: "aws_account.create", TargetType: "aws_account", TargetID: strconv.FormatInt(created.ID, 10),
			Detail: map[string]any{"name": req.Name, "account_id": req.AccountID,
				"discovery_method": req.Discovery.Method, "deploy_method": req.Deploy.Method}}, err
	})
	switch {
	case errors.Is(err, store.ErrDuplicate):
		writeError(w, http.StatusConflict, codeConflict, "an account with that name or account id already exists")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	environments, err := s.environmentsByAccount(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, awsAccountView(created, environments))
}

// sealAWSSecret encrypts a secret under purpose; nil (the role method) stays nil.
func (s *server) sealAWSSecret(ctx context.Context, purpose string, plaintext []byte) ([]byte, error) {
	if plaintext == nil {
		return nil, nil
	}
	return s.Cipher.Encrypt(ctx, purpose, plaintext)
}

// awsTestResult is the answer of the connection test. ErrorCode is the AWS
// error code (or AccountMismatch), never a message from AWS or a secret.
//
// The top level is the verdict of the whole connection (ok only when both
// credentials are), with the first failure's code and message. Discovery and
// Deploy are the credentials' own results.
type awsTestResult struct {
	OK        bool                `json:"ok"`
	ErrorCode string              `json:"error_code"`
	Message   string              `json:"message"`
	Discovery *awsCredentialCheck `json:"discovery,omitempty"`
	Deploy    *awsCredentialCheck `json:"deploy,omitempty"`
}

// awsCredentialCheck is the result for one credential.
type awsCredentialCheck struct {
	OK        bool   `json:"ok"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
}

func (s *server) testAWSAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		notFound(w, "AWS account")
		return
	}
	ctx := r.Context()
	account, err := store.AWSAccountByID(ctx, s.Pool, id)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, "AWS account")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	result := combineAWSChecks(s.checkAWSDiscovery(ctx, account), checkAWSDeploy(account))
	if result.OK {
		connectionChecks{pool: s.Pool, log: s.Log}.OK(ctx, "aws", account.AccountID, s.Now())
	} else {
		connectionChecks{pool: s.Pool, log: s.Log}.Error(ctx, "aws", account.AccountID, result.ErrorCode, s.Now())
	}
	if err := s.recordAudit(ctx, s.Pool, r, audit.Event{Action: "aws_account.test", TargetType: "aws_account", TargetID: strconv.FormatInt(account.ID, 10),
		Detail: map[string]any{"account_id": account.AccountID, "ok": result.OK, "error_code": result.ErrorCode}}); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// awsTestSessionName is the STS session name of the connection test, so the
// calls it makes can be told apart in CloudTrail.
const awsTestSessionName = "yumdrake-orchestrator-test"

// checkAWSDiscovery signs in with the discovery credential, built by the
// discovery provider like for any other discovery call, and checks that
// GetCallerIdentity answers with the account on the row.
func (s *server) checkAWSDiscovery(ctx context.Context, account store.AWSAccount) awsTestResult {
	ctx, cancel := context.WithTimeout(ctx, awsTestTimeout)
	defer cancel()
	cfg, err := s.Discovery.ForUncached(ctx, account.AccountID, "", awsTestSessionName)
	if err != nil {
		return awsFailure(err)
	}
	identity, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return awsFailure(err)
	}
	if got := aws.ToString(identity.Account); got != account.AccountID {
		return awsTestResult{ErrorCode: "AccountMismatch", Message: "The credential belongs to another account than " + account.AccountID}
	}
	return awsTestResult{OK: true, Message: "Connected to account " + account.AccountID}
}

// awsFailure reports only the AWS error code: the text of an SDK error can carry
// request details.
func awsFailure(err error) awsTestResult {
	code := "RequestFailed"
	var apiErr smithy.APIError
	switch {
	case errors.As(err, &apiErr):
		code = apiErr.ErrorCode()
	case errors.Is(err, context.DeadlineExceeded):
		code = "Timeout"
	}
	return awsTestResult{ErrorCode: code, Message: "AWS did not accept the credential: " + code}
}

// checkAWSDeploy is what the orchestrator can say about the deployment
// credential. It never holds that credential (the runner does), so it checks
// the role ARN's account; the runner checks what the credential signs in to
// at the start of every plan.
func checkAWSDeploy(account store.AWSAccount) awsTestResult {
	if account.Deploy.Method == "role" {
		if got := roleARNAccount(account.Deploy.RoleARN); got != account.AccountID {
			return awsTestResult{ErrorCode: "AccountMismatch", Message: "The deployment role is in account " + got + ", not " + account.AccountID}
		}
		return awsTestResult{OK: true, Message: "The deployment role is in account " + account.AccountID + "; the runner checks that it can be assumed when it plans"}
	}
	return awsTestResult{OK: true, Message: "The deployment access key is checked by the runner, which confirms its account when it plans"}
}

func combineAWSChecks(discovery, deploy awsTestResult) awsTestResult {
	out := awsTestResult{OK: discovery.OK && deploy.OK,
		Discovery: &awsCredentialCheck{OK: discovery.OK, ErrorCode: discovery.ErrorCode, Message: discovery.Message},
		Deploy:    &awsCredentialCheck{OK: deploy.OK, ErrorCode: deploy.ErrorCode, Message: deploy.Message}}
	switch {
	case !discovery.OK:
		out.ErrorCode, out.Message = discovery.ErrorCode, "Discovery credential: "+discovery.Message
	case !deploy.OK:
		out.ErrorCode, out.Message = deploy.ErrorCode, "Deployment credential: "+deploy.Message
	default:
		out.Message = discovery.Message
	}
	return out
}
