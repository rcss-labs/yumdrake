//go:build e2e && floci

package e2e

// End-to-end proof of planning against the Floci AWS emulator. Each test seeds
// real ECR repositories, running ECS services and S3 buckets in its own random
// AWS accounts, starts one orchestrator (talking to AWS through a recording
// proxy) and one runner (talking to Floci directly, wired by
// runnerapp.RegisterExecutors exactly as cmd/runner wires it), connects the
// accounts and signs in through the API. Only copy-config and ecs-deploy are
// stand-ins: they record what the manifest tells them to deploy.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/rcss-labs/yamdrake/internal/config"
	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/logs"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/runner"
	"github.com/rcss-labs/yamdrake/internal/runnerapp"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/testutil/testdb"
	"github.com/rcss-labs/yamdrake/internal/wake"
	"github.com/rcss-labs/yamdrake/test/awstest"
	"github.com/rcss-labs/yamdrake/test/e2e/fakegithub"
)

const (
	flociRegion = "us-east-1"
	configRepo  = "acme/config"
	devEnv      = "dev-1"
	prodEnv     = "prod-1"
	// The version the tests plan, and the one the services run now.
	planVersion    = "1.4.2"
	runningVersion = "1.3.0"
)

var planServices = []string{"web", "api", "worker"}

// Config files. Their values are made to be found: no response, log line or
// plan may ever contain one.
var (
	webEnvNew     = []byte("API_KEY=e2e-secret-new-key\nLOG_LEVEL=info\nNEW_FLAG=1\n")
	webEnvOld     = []byte("API_KEY=e2e-secret-old-key\nLOG_LEVEL=info\nOLD_FLAG=1\n")
	webEnvDevOnly = []byte("API_KEY=e2e-secret-dev-key\nLOG_LEVEL=debug\nONLY_DEV=1\n")
	apiEnv        = []byte("PORT=8080\nDB_PASSWORD=e2e-secret-db-password\n")
	apiEnvDevOnly = []byte("PORT=9090\nDB_PASSWORD=e2e-secret-db-password\n")
	webToml       = []byte("token = \"e2e-secret-toml-token\"\n")
	webTomlLater  = []byte("token = \"e2e-secret-toml-token-later\"\n")
	secretValues  = []string{"e2e-secret-new-key", "e2e-secret-old-key", "e2e-secret-dev-key", "e2e-secret-db-password", "e2e-secret-toml-token"}
)

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---- seeding ----

// seededService is what one service of one environment really has in Floci.
type seededService struct {
	name, repo, repoURI, cluster string
	digest, serviceARN, taskDef  string
}

// seededEnv is one environment: an AWS account, its services and buckets. An
// environment that is not seeded still appears in the inventory, with
// coordinates that point nowhere, so the inventory always has both.
type seededEnv struct {
	name, label, suffix  string
	acct                 *awstest.Account
	seeded               bool
	srcBucket, dstBucket string
	services             map[string]*seededService
}

func (e *seededEnv) ecrURI(service string) string {
	return fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com/yd-%s-%s", e.acct.ID, flociRegion, service, e.suffix)
}

func (e *seededEnv) ecsARN(service string) string {
	if s, ok := e.services[service]; ok {
		return s.serviceARN
	}
	return fmt.Sprintf("arn:aws:ecs:%s:%s:service/yd-c-%s-%s/%s", flociRegion, e.acct.ID, service, e.suffix, service)
}

// seed creates, for every service, a repository holding the running and the
// requested tag (one image, so the two versions differ by tag only), and a
// running ECS service; and the config buckets with their current contents.
func (e *seededEnv) seed(t *testing.T) {
	t.Helper()
	e.seeded = true
	e.services = map[string]*seededService{}
	for _, name := range planServices {
		s := &seededService{name: name, repo: fmt.Sprintf("yd-%s-%s", name, e.suffix), cluster: fmt.Sprintf("yd-c-%s-%s", name, e.suffix)}
		s.repoURI, s.digest = e.acct.Repo(flociRegion, s.repo, runningVersion, planVersion)
		s.serviceARN, s.taskDef = e.acct.Service(flociRegion, s.cluster, name, s.repoURI+":"+runningVersion)
		e.services[name] = s
	}
	e.acct.Bucket(e.srcBucket, true)
	e.acct.Bucket(e.dstBucket, false)
	e.acct.PutObject(e.srcBucket, "web.toml", webToml)
	// The destinations hold what the services have now: web's env differs from
	// what will be deployed, its TOML and api's env are the same.
	e.acct.PutObject(e.dstBucket, "web.env", webEnvOld)
	e.acct.PutObject(e.dstBucket, "web.toml", webToml)
	e.acct.PutObject(e.dstBucket, "api.env", apiEnv)
}

// ---- the stand-ins for copy-config and ecs-deploy ----

// deployCall is what a stand-in step was told to do: the manifest entry of its
// service, as the run holds it.
type deployCall struct {
	use     string
	service manifest.Service
}

type deployRecorder struct {
	mu    sync.Mutex
	calls []deployCall
	// afterDeploy, when set, decides the outcome of an ecs-deploy stand-in
	// after it recorded its call (the visibility test fails one service and
	// writes the tagged task definition a real deploy would).
	afterDeploy func(task queue.Claimed, call deployCall) queue.Outcome
}

func (d *deployRecorder) setAfterDeploy(f func(task queue.Claimed, call deployCall) queue.Outcome) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.afterDeploy = f
}

type stepFunc func(ctx context.Context, env runner.Env, task queue.Claimed) queue.Outcome

func (f stepFunc) Execute(ctx context.Context, env runner.Env, task queue.Claimed) queue.Outcome {
	return f(ctx, env, task)
}

func (d *deployRecorder) executor(use string) runner.Executor {
	return stepFunc(func(_ context.Context, _ runner.Env, task queue.Claimed) queue.Outcome {
		var m manifest.Manifest
		if err := json.Unmarshal(task.Run.Manifest, &m); err != nil {
			return queue.Outcome{State: "failed", FailureShort: "the run holds no readable manifest"}
		}
		call := deployCall{use: use}
		for _, s := range m.Services {
			if s.Name == task.Leg.Service {
				call.service = s
			}
		}
		d.mu.Lock()
		d.calls = append(d.calls, call)
		after := d.afterDeploy
		d.mu.Unlock()
		if use == "ecs-deploy" && after != nil {
			return after(task, call)
		}
		return queue.Outcome{State: "succeeded"}
	})
}

func (d *deployRecorder) all() []deployCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

// ---- the stack ----

type flociOpts struct {
	seed []string // the environments to seed: devEnv, prodEnv
	// noVersionBranch leaves the config repository without a branch for the
	// planned version, so only "dev" exists.
	noVersionBranch bool
	// orchestratorEnv adds settings to the orchestrator's environment.
	orchestratorEnv map[string]string
}

type flociStack struct {
	t              *testing.T
	w              *awstest.World
	proxy          *awstest.Proxy
	gh             *fakegithub.Server
	api            client
	pool           *pgxpool.Pool
	jobID          int64
	platform       *awstest.Account
	artifactBucket string
	envs           map[string]*seededEnv
	rec            *deployRecorder
	// stopRunner stops the in-process runner (also run by the cleanup); it
	// returns once the runner has stopped.
	stopRunner func()
}

func startFloci(t *testing.T, o flociOpts) *flociStack {
	t.Helper()
	valkeyAddr := os.Getenv("YUMDRAKE_TEST_VALKEY_ADDR")
	if valkeyAddr == "" {
		t.Fatal("YUMDRAKE_TEST_VALKEY_ADDR must be set for end-to-end tests")
	}
	w := awstest.New(t)
	sfx := awstest.RandomSuffix()
	s := &flociStack{t: t, w: w, proxy: w.Proxy(t), rec: &deployRecorder{}, platform: w.Account(awstest.RandomAccountID()), artifactBucket: "yd-art-" + sfx}
	s.envs = map[string]*seededEnv{
		devEnv:  {name: devEnv, label: "dev", suffix: "d" + sfx, acct: w.Account(awstest.RandomAccountID())},
		prodEnv: {name: prodEnv, label: "prod", suffix: "p" + sfx, acct: w.Account(awstest.RandomAccountID())},
	}
	for _, e := range s.envs {
		e.srcBucket, e.dstBucket = "yd-src-"+e.suffix, "yd-dst-"+e.suffix
	}
	for _, name := range o.seed {
		s.envs[name].seed(t)
	}
	s.platform.Bucket(s.artifactBucket, false)

	s.gh = fakegithub.New(githubToken)
	t.Cleanup(s.gh.Close)
	s.loadGitHub(o)

	// The runner's and the orchestrator's own AWS identity is the platform
	// account: the runner writes the artifact copies with it.
	t.Setenv("AWS_ACCESS_KEY_ID", s.platform.ID)
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", flociRegion)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	databaseURL, keyPrefix := testdb.New(t), randomKeyPrefix(t)
	orchestratorEnv := map[string]string{"YUMDRAKE_AWS_ENDPOINT_URL": s.proxy.URL}
	for k, v := range o.orchestratorEnv {
		orchestratorEnv[k] = v
	}
	base := startEngineOrchestrator(t, databaseURL, keyPrefix, valkeyAddr, s.gh.URL(), true, orchestratorEnv)
	s.api = signIn(t, base, databaseURL)
	var err error
	if s.pool, err = db.Open(context.Background(), databaseURL); err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(s.pool.Close)

	for _, e := range s.envs {
		key := map[string]any{"method": "key", "access_key_id": e.acct.ID, "secret_access_key": "test"}
		s.api.call(http.MethodPost, "/api/v1/admin/aws-accounts", map[string]any{"name": "e2e-" + e.name, "account_id": e.acct.ID, "discovery": key, "deploy": key}, nil)
	}
	s.api.call(http.MethodPost, "/api/v1/admin/github-connections", map[string]any{"owner": "acme", "method": "token", "token": githubToken}, nil)
	var folder struct {
		ID int64 `json:"id"`
	}
	s.api.call(http.MethodPost, "/api/v1/folders", map[string]any{"name": "Everything"}, &folder)
	s.api.call(http.MethodPut, "/api/v1/admin/inventory-source", map[string]any{"repo": repo, "branch": branch}, nil)
	var job jobView
	s.api.call(http.MethodPost, "/api/v1/jobs", map[string]any{"folder_id": folder.ID, "repo": repo, "branch": branch, "path": "jobs/deploy.yaml"}, &job)
	if !s.api.job(job.ID).Runnable {
		t.Fatal("the job is not runnable")
	}
	s.jobID = job.ID

	s.startRunner(databaseURL, keyPrefix, valkeyAddr)
	return s
}

// loadGitHub puts the inventory and job on the platform repository, and the
// env files on the config repository: one commit on branch v<version> (unless
// the test leaves it out) and a different one on "dev".
func (s *flociStack) loadGitHub(o flociOpts) {
	t := s.t
	dir := t.TempDir()
	write := func(name, content string) {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("make %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	var environments strings.Builder
	environments.WriteString("apiVersion: yumdrake/v1\nkind: Environments\nenvironments:\n")
	for _, name := range []string{devEnv, prodEnv} {
		e := s.envs[name]
		fmt.Fprintf(&environments, "  - name: %s\n    label: %s\n    aws_account: %q\n", e.name, e.label, e.acct.ID)
	}
	write("environments.yaml", environments.String())
	for _, name := range planServices {
		var b strings.Builder
		fmt.Fprintf(&b, "apiVersion: yumdrake/v1\nkind: Service\nname: %s\nversions:\n  pattern: '^v?\\d+\\.\\d+\\.\\d+$'\n  sort: semver\n  limit: 100\nenvironments:\n", name)
		for _, envName := range []string{devEnv, prodEnv} {
			e := s.envs[envName]
			fmt.Fprintf(&b, "  %s:\n    ecs_service: %s\n    ecr_repository: %s\n", envName, e.ecsARN(name), e.ecrURI(name))
			if name == "web" || name == "api" {
				fmt.Fprintf(&b, "    env_files:\n      - name: %s.env\n        source_env_path: %s/envs/%s/%s.env\n        destination_env_path: s3://%s/%s.env\n",
					name, configRepo, envName, name, e.dstBucket, name)
			}
			if name == "web" {
				fmt.Fprintf(&b, "    tomls:\n      - name: web.toml\n        source_toml_path: s3://%s/web.toml\n        destination_toml_path: s3://%s/web.toml\n", e.srcBucket, e.dstBucket)
			}
		}
		write("services/"+name+".yaml", b.String())
	}
	write("jobs/deploy.yaml", `apiVersion: yumdrake/v1
kind: Job
job: deploy
title: "Deploy everything"
environment:
  labels: [dev, prod]
groups:
  all:
    label: Everything
    services: [web, api, worker]
steps:
  - name: config
    use: copy-config
    group: all
  - name: roll
    use: ecs-deploy
    group: all
    needs:
      - step: config
        scope: service
`)
	const platformCommit = "5555555555555555555555555555555555555555"
	if err := s.gh.LoadDir(repo, platformCommit, dir); err != nil {
		t.Fatalf("load the platform repository: %v", err)
	}
	s.gh.SetHead(repo, branch, platformCommit)

	files := func(web, api []byte) map[string][]byte {
		out := map[string][]byte{}
		for _, envName := range []string{devEnv, prodEnv} {
			out["envs/"+envName+"/web.env"] = web
			out["envs/"+envName+"/api.env"] = api
		}
		return out
	}
	s.gh.AddCommit(configRepo, "c0nf1g-dev", files(webEnvDevOnly, apiEnvDevOnly))
	s.gh.SetHead(configRepo, "dev", "c0nf1g-dev")
	if !o.noVersionBranch {
		s.gh.AddCommit(configRepo, "c0nf1g-v1", files(webEnvNew, apiEnv))
		s.gh.SetHead(configRepo, "v"+planVersion, "c0nf1g-v1")
	}
}

// startRunner starts one runner in-process, configured and wired the way
// cmd/runner is, with the stand-ins for copy-config and ecs-deploy.
func (s *flociStack) startRunner(databaseURL, keyPrefix, valkeyAddr string) {
	t := s.t
	env := map[string]string{
		"YUMDRAKE_DATABASE_URL":      databaseURL,
		"YUMDRAKE_VALKEY_ADDR":       valkeyAddr,
		"YUMDRAKE_LOCAL_SECRET_KEY":  localSecretKey,
		"YUMDRAKE_VALKEY_KEY_PREFIX": keyPrefix,
		"YUMDRAKE_ARTIFACT_BUCKET":   s.artifactBucket,
		"YUMDRAKE_GITHUB_API_URL":    s.gh.URL(),
		"YUMDRAKE_AWS_ENDPOINT_URL":  s.w.Endpoint, // the runner talks to Floci directly
		"YUMDRAKE_RUNNER_ID":         "runner-floci",
		"YUMDRAKE_LOG_LEVEL":         "error",
	}
	cfg, err := config.LoadRunner(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("load runner config: %v", err)
	}
	pool, err := db.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("open database for the runner: %v", err)
	}
	valkeyClient, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{valkeyAddr}, ForceSingleClient: true})
	if err != nil {
		t.Fatalf("open valkey: %v", err)
	}
	quiet := slog.New(slog.DiscardHandler)
	bus := wake.New(valkeyClient, quiet, wake.WithKeyPrefix(keyPrefix))
	sink := logs.NewWriter(pool, 100*time.Millisecond, 100, time.Now, func(runID int64) {
		bus.Publish(context.Background(), wake.TopicLogs, runID)
	}, quiet)
	r := runner.New(queue.New(pool, 10*time.Second, nil), sink, cfg.RunnerID, 8, 2*time.Second, 100*time.Millisecond, quiet).
		WithShutdownTimeout(10*time.Second).
		WithRegistry(pool, 500*time.Millisecond).
		WithSettledNotifier(func(runID int64) { bus.Publish(context.Background(), wake.TopicRuns, runID) })
	ctx, cancel := context.WithCancel(context.Background())
	if err := runnerapp.RegisterExecutors(ctx, quiet, cfg, pool, r); err != nil {
		cancel()
		t.Fatalf("register the runner's executors: %v", err)
	}
	r.Register("copy-config", s.rec.executor("copy-config"))
	r.Register("ecs-deploy", s.rec.executor("ecs-deploy"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx, nil)
	}()
	var stopOnce sync.Once
	s.stopRunner = func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("the runner did not stop within 30s")
			}
			closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = sink.Close(closeCtx)
			pool.Close()
			valkeyClient.Close()
		})
	}
	t.Cleanup(s.stopRunner)
}

// ---- API helpers ----

func (s *flociStack) createRun(env, version string, start bool) int64 {
	s.t.Helper()
	services := map[string]string{}
	for _, name := range planServices {
		services[name] = ""
	}
	var created struct {
		ID int64 `json:"id"`
	}
	s.api.call(http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/runs", s.jobID), map[string]any{
		"environment": env,
		"selection":   map[string]any{"all": map[string]any{"version": version, "services": services}},
		"start":       start,
	}, &created)
	return created.ID
}

func (s *flociStack) run(id int64) runInfo {
	s.t.Helper()
	var info runInfo
	s.api.call(http.MethodGet, fmt.Sprintf("/api/v1/runs/%d", id), nil, &info)
	return info
}

func (s *flociStack) waitRun(id int64, states ...string) runInfo {
	s.t.Helper()
	var last runInfo
	eventually(s.t, runWait, fmt.Sprintf("run %d to be %s", id, strings.Join(states, " or ")), func() bool {
		last = s.run(id)
		return slices.Contains(states, last.State)
	})
	return last
}

func (s *flociStack) start(id int64) {
	s.t.Helper()
	s.api.call(http.MethodPost, fmt.Sprintf("/api/v1/runs/%d/start", id), nil, nil)
}

// plan reads GET /api/v1/runs/{id}/plan and returns the decoded plan and the
// raw body.
func (s *flociStack) plan(id int64) (planDoc, string) {
	s.t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/runs/%d/plan", s.api.base, id), nil)
	if err != nil {
		s.t.Fatalf("build the plan request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "yumdrake_session", Value: s.api.session})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("GET plan of run %d: %v", id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("GET plan of run %d: status %d", id, resp.StatusCode)
	}
	var doc planDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		s.t.Fatalf("decode the plan of run %d: %v", id, err)
	}
	return doc, string(raw)
}

// planDoc is the plan as served: the field names here are the contract.
type planDoc struct {
	ID          string `json:"id"`
	RunID       int64  `json:"run_id"`
	Environment string `json:"environment"`
	Label       string `json:"label"`
	AWSAccount  string `json:"aws_account"`
	Services    []struct {
		Name                   string `json:"name"`
		Group                  string `json:"group"`
		Version                string `json:"version"`
		ImageDigest            string `json:"image_digest"`
		ECSService             string `json:"ecs_service"`
		PreviousTaskDefinition string `json:"previous_task_definition"`
		PreviousVersion        string `json:"previous_version"`
		Config                 []struct {
			Kind           string `json:"kind"`
			Name           string `json:"name"`
			Branch         string `json:"branch"`
			Commit         string `json:"commit"`
			Fallback       bool   `json:"fallback"`
			SHA256         string `json:"sha256"`
			Size           int64  `json:"size"`
			PreviousExists bool   `json:"previous_exists"`
			Change         string `json:"change"`
			Keys           *struct {
				Added   []string `json:"added"`
				Removed []string `json:"removed"`
				Changed []string `json:"changed"`
			} `json:"keys"`
		} `json:"config"`
	} `json:"services"`
	Checks []struct {
		Name    string `json:"name"`
		Service string `json:"service"`
		Status  string `json:"status"`
		Detail  string `json:"detail"`
	} `json:"checks"`
}

func (p planDoc) service(t *testing.T, name string) (idx int) {
	t.Helper()
	for i, s := range p.Services {
		if s.Name == name {
			return i
		}
	}
	t.Fatalf("the plan has no service %q", name)
	return -1
}

// ---- helpers that change the world behind the plan's back ----

// moveTag pushes different content under the planned tag of every service of
// env and returns the new digests.
func (s *flociStack) moveTag(e *seededEnv) map[string]string {
	s.t.Helper()
	moved := map[string]string{}
	for name, svc := range e.services {
		moved[name] = e.acct.RepoImage(flociRegion, svc.repo, planVersion, "moved-"+awstest.RandomSuffix())
		if moved[name] == svc.digest {
			s.t.Fatalf("moving the tag of %s did not change its digest", name)
		}
	}
	return moved
}

// deployByHand registers a new task definition revision and points the service
// at it, as a person with console access would.
func (s *flociStack) deployByHand(e *seededEnv, service string) {
	s.t.Helper()
	ctx := context.Background()
	svc := e.services[service]
	client := ecs.NewFromConfig(e.acct.AWSConfig(flociRegion))
	current, err := client.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(svc.taskDef)})
	if err != nil || current.TaskDefinition == nil {
		s.t.Fatalf("describe task definition %s: %v", svc.taskDef, err)
	}
	td := current.TaskDefinition
	revised, err := client.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family: td.Family, NetworkMode: td.NetworkMode, RequiresCompatibilities: td.RequiresCompatibilities,
		Cpu: td.Cpu, Memory: td.Memory, ContainerDefinitions: td.ContainerDefinitions,
		Tags: nil,
	})
	if err != nil || revised.TaskDefinition == nil {
		s.t.Fatalf("register a new revision of %s: %v", service, err)
	}
	if aws.ToString(revised.TaskDefinition.TaskDefinitionArn) == svc.taskDef {
		s.t.Fatalf("the new revision of %s has the old ARN", service)
	}
	if _, err := client.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster: aws.String(svc.cluster), Service: aws.String(service), TaskDefinition: revised.TaskDefinition.TaskDefinitionArn,
	}); err != nil {
		s.t.Fatalf("update service %s: %v", service, err)
	}
}

// assertNoStepRan fails when the run has a step leg that started or finished,
// or a stand-in step was called.
func (s *flociStack) assertNoStepRan(info runInfo) {
	s.t.Helper()
	for _, l := range info.Legs {
		if l.Kind == "step" && (l.StartedAt != nil || l.State == "succeeded" || l.State == "running") {
			s.t.Errorf("step leg %s/%s is %q (started %v), want it never to have run", l.Step, l.Service, l.State, l.StartedAt)
		}
	}
	if calls := s.rec.all(); len(calls) != 0 {
		s.t.Errorf("%d step calls reached the stand-in steps, want none", len(calls))
	}
}

// ---- the tests ----

// TestPlanShowsRealStateAndDiff plans against real ECR, ECS and S3 state and
// reads the plan as the web application does.
func TestPlanShowsRealStateAndDiff(t *testing.T) {
	s := startFloci(t, flociOpts{seed: []string{devEnv}})
	dev := s.envs[devEnv]

	var versions struct {
		Groups map[string]struct {
			Common   []string `json:"common"`
			Services map[string][]struct {
				Tag    string `json:"tag"`
				Digest string `json:"digest"`
			} `json:"services"`
			Unavailable map[string]string `json:"unavailable"`
		} `json:"groups"`
	}
	s.api.call(http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d/versions?environment=%s", s.jobID, url.QueryEscape(devEnv)), nil, &versions)
	group := versions.Groups["all"]
	if len(group.Unavailable) != 0 {
		t.Errorf("services unavailable in the version list: %v", group.Unavailable)
	}
	for _, tag := range []string{runningVersion, planVersion} {
		if !slices.Contains(group.Common, tag) {
			t.Errorf("version list common = %v, want it to offer %s", group.Common, tag)
		}
	}
	for _, name := range planServices {
		var tags []string
		for _, v := range group.Services[name] {
			tags = append(tags, v.Tag)
			if v.Digest != dev.services[name].digest {
				t.Errorf("version %s of %s has digest %s, want %s", v.Tag, name, v.Digest, dev.services[name].digest)
			}
		}
		slices.Sort(tags)
		if strings.Join(tags, ",") != runningVersion+","+planVersion {
			t.Errorf("tags offered for %s = %v, want %s and %s", name, tags, runningVersion, planVersion)
		}
	}

	id := s.createRun(devEnv, planVersion, false)
	planned := s.waitRun(id, "planned", "failed")
	if planned.State != "planned" {
		t.Fatalf("run state = %q (%s), want planned", planned.State, planned.EndReason)
	}
	plan, raw := s.plan(id)

	if plan.RunID != id || plan.Environment != devEnv || plan.Label != "dev" || plan.AWSAccount != dev.acct.ID || len(plan.ID) != 64 {
		t.Errorf("plan header = run %d, environment %q, label %q, account %q, id %q", plan.RunID, plan.Environment, plan.Label, plan.AWSAccount, plan.ID)
	}
	if len(plan.Services) != len(planServices) {
		t.Fatalf("plan has %d services, want %d", len(plan.Services), len(planServices))
	}
	for _, name := range planServices {
		svc, real := plan.Services[plan.service(t, name)], dev.services[name]
		if svc.Group != "all" || svc.Version != planVersion || svc.PreviousVersion != runningVersion {
			t.Errorf("%s: group %q, requested %q, current %q; want all, %s, %s", name, svc.Group, svc.Version, svc.PreviousVersion, planVersion, runningVersion)
		}
		if svc.ImageDigest != real.digest || svc.ECSService != real.serviceARN || svc.PreviousTaskDefinition != real.taskDef {
			t.Errorf("%s: digest %q, service %q, task definition %q; want what Floci holds (%q, %q, %q)",
				name, svc.ImageDigest, svc.ECSService, svc.PreviousTaskDefinition, real.digest, real.serviceARN, real.taskDef)
		}
	}

	web := plan.Services[plan.service(t, "web")]
	if len(web.Config) != 2 {
		t.Fatalf("web has %d config items, want an env file and a TOML", len(web.Config))
	}
	env, toml := web.Config[0], web.Config[1]
	if env.Kind != "env" || env.Change != "changed" || env.Branch != "v"+planVersion || env.Fallback || env.SHA256 != sha(webEnvNew) || env.Size != int64(len(webEnvNew)) || !env.PreviousExists {
		t.Errorf("web env item = %+v", env)
	}
	if env.Keys == nil || strings.Join(env.Keys.Added, ",") != "NEW_FLAG" || strings.Join(env.Keys.Removed, ",") != "OLD_FLAG" || strings.Join(env.Keys.Changed, ",") != "API_KEY" {
		t.Errorf("web env key diff = %+v, want added NEW_FLAG, removed OLD_FLAG, changed API_KEY", env.Keys)
	}
	if toml.Kind != "toml" || toml.Change != "unchanged" || toml.SHA256 != sha(webToml) || toml.Keys != nil {
		t.Errorf("web toml item = %+v, want unchanged and no key diff", toml)
	}
	api := plan.Services[plan.service(t, "api")]
	if len(api.Config) != 1 || api.Config[0].Change != "unchanged" || api.Config[0].SHA256 != sha(apiEnv) {
		t.Errorf("api config = %+v, want one unchanged env file", api.Config)
	}
	if worker := plan.Services[plan.service(t, "worker")]; len(worker.Config) != 0 {
		t.Errorf("worker has config %+v, want none", worker.Config)
	}

	notVerified := 0
	for _, c := range plan.Checks {
		switch {
		case c.Name == "write permission" && c.Status == "not_verified":
			notVerified++
		case c.Status != "passed":
			t.Errorf("check %s/%s = %s (%s), want passed", c.Service, c.Name, c.Status, c.Detail)
		}
	}
	if notVerified != len(planServices) {
		t.Errorf("%d write-permission checks are not_verified, want %d (one per service)", notVerified, len(planServices))
	}
	for _, secret := range secretValues {
		if strings.Contains(raw, secret) {
			t.Errorf("the plan contains the config value %q", secret)
		}
	}
	if strings.Contains(raw, "artifact_key") {
		t.Error("the plan serves artifact keys")
	}
}

// TestPinnedInputsSurviveLaterChanges moves the tag, adds a commit to the
// branch and overwrites the TOML source after the plan; the run still deploys
// exactly what the plan recorded. (Review Focus 1)
func TestPinnedInputsSurviveLaterChanges(t *testing.T) {
	s := startFloci(t, flociOpts{seed: []string{devEnv}})
	dev := s.envs[devEnv]

	id := s.createRun(devEnv, planVersion, false)
	if got := s.waitRun(id, "planned", "failed"); got.State != "planned" {
		t.Fatalf("run state = %q, want planned", got.State)
	}
	before, _ := s.plan(id)

	// The world moves on.
	moved := s.moveTag(dev)
	s.gh.AddCommit(configRepo, "c0nf1g-v1-later", map[string][]byte{
		"envs/" + devEnv + "/web.env":  []byte("API_KEY=e2e-secret-new-key\nLATER=1\n"),
		"envs/" + devEnv + "/api.env":  apiEnvDevOnly,
		"envs/" + prodEnv + "/web.env": webEnvNew, "envs/" + prodEnv + "/api.env": apiEnv,
	})
	s.gh.SetHead(configRepo, "v"+planVersion, "c0nf1g-v1-later")
	dev.acct.PutObject(dev.srcBucket, "web.toml", webTomlLater)

	s.start(id)
	done := s.waitRun(id, "succeeded", "failed", "invalidated")
	if done.State != "succeeded" {
		t.Fatalf("run state = %q (%s), want succeeded: the pinned image and copies still exist", done.State, done.EndReason)
	}

	calls := s.rec.all()
	if len(calls) != 2*len(planServices) {
		t.Fatalf("%d step calls, want %d (copy-config and ecs-deploy for each service)", len(calls), 2*len(planServices))
	}
	for _, call := range calls {
		name := call.service.Name
		planned := before.Services[before.service(t, name)]
		if call.service.ImageDigest != dev.services[name].digest || call.service.ImageDigest != planned.ImageDigest || call.service.ImageDigest == moved[name] {
			t.Errorf("%s %s digest = %s, want the planned %s (the tag now points at %s)", call.use, name, call.service.ImageDigest, dev.services[name].digest, moved[name])
		}
		if len(call.service.Config) != len(planned.Config) {
			t.Errorf("%s %s config items = %d, want %d", call.use, name, len(call.service.Config), len(planned.Config))
			continue
		}
		for i, item := range call.service.Config {
			if item.SHA256 != planned.Config[i].SHA256 {
				t.Errorf("%s %s config %s checksum = %s, want the planned %s", call.use, name, item.Name, item.SHA256, planned.Config[i].SHA256)
			}
			// The copy the deploy will push is the original bytes, not what the sources hold now.
			want := map[string][]byte{"web.env": webEnvNew, "web.toml": webToml, "api.env": apiEnv}[item.Name]
			got, ok := s.platform.Object(s.artifactBucket, item.ArtifactKey)
			if !ok || sha(got) != item.SHA256 || sha(want) != item.SHA256 {
				t.Errorf("%s %s artifact copy of %s: found %v, sha %s, want the original %s", call.use, name, item.Name, ok, sha(got), sha(want))
			}
		}
	}
	if after, _ := s.plan(id); after.ID != before.ID {
		t.Errorf("the plan's identity changed from %s to %s after the run", before.ID, after.ID)
	}
}

// TestHandDeployInvalidatesRun changes a service by hand between planning and
// starting; revalidation ends the run before anything is touched.
// (Review Focus 2)
func TestHandDeployInvalidatesRun(t *testing.T) {
	s := startFloci(t, flociOpts{seed: []string{devEnv}})
	dev := s.envs[devEnv]

	id := s.createRun(devEnv, planVersion, false)
	if got := s.waitRun(id, "planned", "failed"); got.State != "planned" {
		t.Fatalf("run state = %q, want planned", got.State)
	}
	s.deployByHand(dev, "api")

	s.start(id)
	done := s.waitRun(id, "invalidated", "succeeded", "failed")
	if done.State != "invalidated" {
		t.Fatalf("run state = %q (%s), want invalidated", done.State, done.EndReason)
	}
	for _, l := range done.Legs {
		if l.Kind == "revalidate" && (l.State != "failed" || l.FailureShort != "changed since planning") {
			t.Errorf("revalidate leg = %q %q, want failed with \"changed since planning\"", l.State, l.FailureShort)
		}
	}
	s.assertNoStepRan(done)

	run, err := store.GetRun(context.Background(), s.pool, id)
	if err != nil {
		t.Fatalf("read run %d: %v", id, err)
	}
	if len(run.LockResources) == 0 {
		t.Error("the run records no resources to lock, so \"locks released\" proves nothing")
	}
	held, err := store.LocksOfRun(context.Background(), s.pool, id)
	if err != nil || len(held) != 0 || len(done.Locks) != 0 {
		t.Errorf("locks held after the run ended = %v (API: %v, error %v), want none", held, done.Locks, err)
	}
}

// TestProdWithoutVersionBranchFailsPreflight: in prod a missing version
// branch is a failed check that names the branch, and nothing falls back to
// the dev branch even though it exists. (Review Focus 3)
func TestProdWithoutVersionBranchFailsPreflight(t *testing.T) {
	s := startFloci(t, flociOpts{seed: []string{prodEnv}, noVersionBranch: true})

	id := s.createRun(prodEnv, planVersion, false)
	done := s.waitRun(id, "failed", "planned")
	if done.State != "failed" {
		t.Fatalf("run state = %q, want failed", done.State)
	}
	plan, _ := s.plan(id)
	failed := 0
	for _, c := range plan.Checks {
		if c.Status != "failed" {
			continue
		}
		failed++
		if !strings.HasPrefix(c.Name, "config source") || !strings.Contains(c.Detail, "v"+planVersion) || !strings.Contains(c.Detail, configRepo) {
			t.Errorf("failed check %s/%s = %q, want a config source check naming branch v%s in %s", c.Service, c.Name, c.Detail, planVersion, configRepo)
		}
	}
	if failed != 2 { // web.env and api.env
		t.Errorf("%d failed checks, want 2 (the env files of web and api)", failed)
	}
	for _, svc := range plan.Services {
		for _, item := range svc.Config {
			if item.Fallback || item.Branch == "dev" || item.Commit == "c0nf1g-dev" {
				t.Errorf("%s config %s fell back to the dev branch: %+v", svc.Name, item.Name, item)
			}
		}
	}
	s.assertNoStepRan(done)
}

// TestDevFallsBackToDevBranchAndSaysSo: in a dev environment a missing version
// branch falls back to the "dev" branch, and the plan shows it. (Review Focus 3)
func TestDevFallsBackToDevBranchAndSaysSo(t *testing.T) {
	s := startFloci(t, flociOpts{seed: []string{devEnv}, noVersionBranch: true})

	id := s.createRun(devEnv, planVersion, false)
	if got := s.waitRun(id, "planned", "failed"); got.State != "planned" {
		t.Fatalf("run state = %q (%s), want planned", got.State, got.EndReason)
	}
	plan, _ := s.plan(id)
	web := plan.Services[plan.service(t, "web")]
	api := plan.Services[plan.service(t, "api")]
	if len(web.Config) == 0 || len(api.Config) == 0 {
		t.Fatalf("plan has no config items: web %d, api %d", len(web.Config), len(api.Config))
	}
	for _, item := range []struct {
		name   string
		branch string
		sha    string
	}{{"web env", web.Config[0].Branch, web.Config[0].SHA256}, {"api env", api.Config[0].Branch, api.Config[0].SHA256}} {
		if item.branch != "dev" {
			t.Errorf("%s branch = %q, want dev", item.name, item.branch)
		}
	}
	if !web.Config[0].Fallback || !api.Config[0].Fallback || web.Config[0].SHA256 != sha(webEnvDevOnly) || api.Config[0].SHA256 != sha(apiEnvDevOnly) {
		t.Errorf("web env %+v, api env %+v: want fallback true and the dev branch's content", web.Config[0], api.Config[0])
	}
	if web.Config[0].Keys == nil || strings.Join(web.Config[0].Keys.Added, ",") != "ONLY_DEV" {
		t.Errorf("web env key diff = %+v, want it computed from the dev branch (added ONLY_DEV)", web.Config[0].Keys)
	}
	said := 0
	for _, c := range plan.Checks {
		if c.Status != "passed" && c.Name != "write permission" {
			t.Errorf("check %s/%s = %s (%s), want passed", c.Service, c.Name, c.Status, c.Detail)
		}
		if strings.HasPrefix(c.Name, "config source") && strings.Contains(c.Detail, "v"+planVersion) && strings.Contains(c.Detail, "fell back to dev") {
			said++
		}
	}
	if said != 2 {
		t.Errorf("%d checks say that branch v%s was missing and dev was used, want 2", said, planVersion)
	}
}

// TestOrchestratorNeverSeesConfigContents plans and runs, listing versions
// along the way, with every call the orchestrator makes to AWS recorded by the
// proxy: none reads an object from a config bucket or the artifact bucket.
func TestOrchestratorNeverSeesConfigContents(t *testing.T) {
	s := startFloci(t, flociOpts{seed: []string{devEnv}})

	// The orchestrator does reach AWS for the version list; the run itself is
	// resolved and revalidated by the runner.
	var versions struct{}
	s.api.call(http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d/versions?environment=%s", s.jobID, url.QueryEscape(devEnv)), nil, &versions)
	id := s.createRun(devEnv, planVersion, true)
	if done := s.waitRun(id, "succeeded", "failed", "invalidated"); done.State != "succeeded" {
		t.Fatalf("run state = %q (%s), want succeeded", done.State, done.EndReason)
	}
	s.plan(id)

	requests := s.proxy.Requests()
	if len(requests) == 0 {
		t.Fatal("the proxy recorded no request from the orchestrator, so this test proves nothing")
	}
	describes := 0
	configBuckets := map[string]bool{s.artifactBucket: true}
	for _, e := range s.envs {
		configBuckets[e.srcBucket], configBuckets[e.dstBucket] = true, true
	}
	proxyURL, err := url.Parse(s.proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyHostname := proxyURL.Hostname()
	for _, r := range requests {
		if r.Service == "ecr" && r.Operation == "DescribeImages" {
			describes++
		}
		if r.Service != "s3" {
			continue
		}
		if objectOnBucket(r, proxyHostname, configBuckets) {
			t.Errorf("the orchestrator made S3 %s (%s %s) on a config or artifact bucket", r.Operation, r.Method, r.Path)
		}
	}
	if describes == 0 {
		t.Error("the proxy recorded no ECR DescribeImages call: the orchestrator's AWS traffic did not go through it")
	}
}

// objectOnBucket reports whether a recorded S3 request addresses an object in one
// of the buckets, whether it was path-style or virtual-host.
func objectOnBucket(r awstest.Request, proxyHostname string, buckets map[string]bool) bool {
	path := strings.TrimPrefix(r.Path, "/")
	hostname := r.Host
	if host, _, err := net.SplitHostPort(r.Host); err == nil {
		hostname = host
	}
	var bucket, key string
	if hostname != proxyHostname {
		bucket = strings.TrimSuffix(hostname, "."+proxyHostname)
		key = path
	} else {
		bucket, key, _ = strings.Cut(path, "/")
	}
	return r.Service == "s3" && buckets[bucket] && key != ""
}

func TestObjectOnBucketMatcher(t *testing.T) {
	cases := []struct {
		name string
		req  awstest.Request
		want bool
	}{
		{"path-style object", awstest.Request{Service: "s3", Path: "/b/k", Host: "127.0.0.1:1234"}, true},
		{"virtual-host object", awstest.Request{Service: "s3", Path: "/k", Host: "b.127.0.0.1:1234"}, true},
		{"path-style bucket listing", awstest.Request{Service: "s3", Path: "/b", Host: "127.0.0.1:1234"}, false},
		{"other bucket", awstest.Request{Service: "s3", Path: "/x/k", Host: "127.0.0.1:1234"}, false},
		{"virtual-host bucket listing", awstest.Request{Service: "s3", Path: "/", Host: "b.127.0.0.1:1234"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := objectOnBucket(c.req, "127.0.0.1", map[string]bool{"b": true}); got != c.want {
				t.Errorf("objectOnBucket(%s %q, host %q) = %v, want %v", c.req.Service, c.req.Path, c.req.Host, got, c.want)
			}
		})
	}
}
