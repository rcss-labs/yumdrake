package running

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/smithy-go"

	"github.com/rcss-labs/yamdrake/internal/awsx/discovery"
	"github.com/rcss-labs/yamdrake/internal/spec"
)

const (
	acctEU    = "111111111111"
	acctUS    = "222222222222"
	acctStage = "333333333333"
	region    = "eu-west-1"
)

// taskdef is what the stub returns for one task definition ARN.
type taskdef struct {
	family     string
	revision   int32
	containers []ecstypes.ContainerDefinition
	tags       map[string]string
}

// world is a stub of ECS, ECR and discovery for the whole fleet. The account
// id travels in aws.Config.AppID.
type world struct {
	mu          sync.Mutex
	services    map[string]ecstypes.Service // account|cluster|name
	taskdefs    map[string]taskdef          // account|arn
	images      map[string][]ecrtypes.ImageDetail
	failAccount map[string]error // discovery fails for the account
	failCluster map[string]error // DescribeServices fails for account|cluster
	hangAccount map[string]bool  // DescribeServices blocks until its context ends
	inactive    map[string]bool  // account|cluster|name reports INACTIVE
	delay       time.Duration

	sessions      []string
	batches       []int
	ecsCalls      atomic.Int64
	ecrCalls      atomic.Int64
	discoveryCall atomic.Int64
	inflight      atomic.Int64
	maxInflight   atomic.Int64
}

func newWorld() *world {
	return &world{
		services: map[string]ecstypes.Service{}, taskdefs: map[string]taskdef{},
		images: map[string][]ecrtypes.ImageDetail{}, failAccount: map[string]error{},
		failCluster: map[string]error{}, inactive: map[string]bool{}, hangAccount: map[string]bool{},
	}
}

func (w *world) enter() func() {
	n := w.inflight.Add(1)
	for {
		m := w.maxInflight.Load()
		if n <= m || w.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	if w.delay > 0 {
		time.Sleep(w.delay)
	}
	return func() { w.inflight.Add(-1) }
}

func (w *world) For(_ context.Context, account, rgn, session string) (aws.Config, error) {
	defer w.enter()()
	w.discoveryCall.Add(1)
	w.mu.Lock()
	w.sessions = append(w.sessions, session)
	err := w.failAccount[account]
	w.mu.Unlock()
	if err != nil {
		return aws.Config{}, err
	}
	return aws.Config{Region: rgn, AppID: account}, nil
}

func (w *world) config(cfg Config) Config {
	cfg.Discovery = w
	cfg.NewECS = func(c aws.Config) ECS { return acctECS{w, c.AppID} }
	cfg.NewECR = func(c aws.Config) ECR { return acctECR{w, c.AppID} }
	return cfg
}

func taskDefARN(account, family string, rev int32) string {
	return fmt.Sprintf("arn:aws:ecs:%s:%s:task-definition/%s:%d", region, account, family, rev)
}

func serviceARN(account, cluster, name string) string {
	return fmt.Sprintf("arn:aws:ecs:%s:%s:service/%s/%s", region, account, cluster, name)
}

func repoURI(account, name string) string {
	return fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com/%s", account, region, name)
}

// run makes the service run a task definition with one container.
func (w *world) run(account, cluster, name string, image string, tags map[string]string) {
	w.runContainers(account, cluster, name, tags, ecstypes.ContainerDefinition{Name: aws.String("app"), Image: aws.String(image)})
}

func (w *world) runContainers(account, cluster, name string, tags map[string]string, containers ...ecstypes.ContainerDefinition) {
	w.mu.Lock()
	defer w.mu.Unlock()
	arn := taskDefARN(account, name, 7)
	w.taskdefs[account+"|"+arn] = taskdef{family: name, revision: 7, containers: containers, tags: tags}
	w.services[account+"|"+cluster+"|"+name] = ecstypes.Service{
		ServiceName: aws.String(name), ServiceArn: aws.String(serviceARN(account, cluster, name)),
		Status: aws.String("ACTIVE"), TaskDefinition: aws.String(arn),
	}
}

func (w *world) addImage(account, repo, digest string, pushed time.Time, tags ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.images[account+"|"+repo] = append(w.images[account+"|"+repo], ecrtypes.ImageDetail{
		ImageDigest: aws.String(digest), ImageTags: tags, ImagePushedAt: aws.Time(pushed),
	})
}

type acctECS struct {
	w       *world
	account string
}

func (a acctECS) DescribeServices(ctx context.Context, in *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	if a.w.hangAccount[a.account] {
		<-ctx.Done() // a blackholed endpoint: only the deadline ends the call
		return nil, ctx.Err()
	}
	defer a.w.enter()()
	a.w.ecsCalls.Add(1)
	cluster := aws.ToString(in.Cluster)
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	a.w.batches = append(a.w.batches, len(in.Services))
	if err := a.w.failCluster[a.account+"|"+cluster]; err != nil {
		return nil, err
	}
	out := &ecs.DescribeServicesOutput{}
	for _, name := range in.Services {
		key := a.account + "|" + cluster + "|" + name
		svc, ok := a.w.services[key]
		if !ok {
			out.Failures = append(out.Failures, ecstypes.Failure{Arn: aws.String(serviceARN(a.account, cluster, name)), Reason: aws.String("MISSING")})
			continue
		}
		if a.w.inactive[key] {
			svc.Status = aws.String("INACTIVE")
		}
		out.Services = append(out.Services, svc)
	}
	return out, nil
}

func (a acctECS) DescribeTaskDefinition(_ context.Context, in *ecs.DescribeTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	defer a.w.enter()()
	a.w.ecsCalls.Add(1)
	if len(in.Include) != 1 || in.Include[0] != ecstypes.TaskDefinitionFieldTags {
		return nil, fmt.Errorf("DescribeTaskDefinition must ask for TAGS, got %v", in.Include)
	}
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	td, ok := a.w.taskdefs[a.account+"|"+aws.ToString(in.TaskDefinition)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "ClientException", Message: "unknown task definition"}
	}
	out := &ecs.DescribeTaskDefinitionOutput{TaskDefinition: &ecstypes.TaskDefinition{
		Family: aws.String(td.family), Revision: td.revision, ContainerDefinitions: td.containers,
	}}
	for k, v := range td.tags {
		out.Tags = append(out.Tags, ecstypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}

type acctECR struct {
	w       *world
	account string
}

func (a acctECR) DescribeImages(_ context.Context, in *ecr.DescribeImagesInput, _ ...func(*ecr.Options)) (*ecr.DescribeImagesOutput, error) {
	defer a.w.enter()()
	a.w.ecrCalls.Add(1)
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	out := &ecr.DescribeImagesOutput{}
	for _, detail := range a.w.images[aws.ToString(in.RegistryId)+"|"+aws.ToString(in.RepositoryName)] {
		for _, id := range in.ImageIds {
			if (id.ImageDigest != nil && aws.ToString(id.ImageDigest) == aws.ToString(detail.ImageDigest)) ||
				(id.ImageTag != nil && containsString(detail.ImageTags, aws.ToString(id.ImageTag))) {
				out.ImageDetails = append(out.ImageDetails, detail)
				break
			}
		}
	}
	return out, nil
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// recordedChecks remembers the connection checks.
type recordedChecks struct {
	mu     sync.Mutex
	oks    []string
	errors map[string]string // account -> code
}

func (c *recordedChecks) OK(_ context.Context, kind, ref string, _ time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.oks = append(c.oks, kind+":"+ref)
}

func (c *recordedChecks) Error(_ context.Context, kind, ref, code string, _ time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.errors == nil {
		c.errors = map[string]string{}
	}
	c.errors[kind+":"+ref] = code
}

var (
	envEU    = spec.Environment{Name: "prod-eu", Label: "prod", AWSAccount: acctEU}
	envUS    = spec.Environment{Name: "prod-us", Label: "prod", AWSAccount: acctUS}
	envStage = spec.Environment{Name: "stage", Label: "stage", AWSAccount: acctStage}
)

// serviceSpec is a service with a block in each of the given environments;
// cluster is "main" and the repository has the service's name.
func serviceSpec(name, sortRule string, envs ...spec.Environment) spec.Service {
	s := spec.Service{Name: name, Versions: spec.VersionRule{Pattern: `^\d+\.\d+\.\d+$`, Sort: sortRule, Limit: 50},
		Environments: map[string]spec.ServiceEnv{}}
	for _, e := range envs {
		s.Environments[e.Name] = spec.ServiceEnv{
			ECSService: serviceARN(e.AWSAccount, "main", name), ECRRepository: repoURI(e.AWSAccount, name),
		}
	}
	return s
}

func inventoryOf(envs []spec.Environment, services ...spec.Service) spec.Inventory {
	inv := spec.Inventory{Environments: envs, Services: map[string]spec.Service{}}
	for _, s := range services {
		inv.Services[s.Name] = s
	}
	return inv
}

func cellOf(t *testing.T, m Matrix, service, env string) Cell {
	t.Helper()
	col := -1
	for i, e := range m.Environments {
		if e.Name == env {
			col = i
		}
	}
	for _, row := range m.Rows {
		if row.Service == service && col >= 0 {
			return row.Cells[col]
		}
	}
	t.Fatalf("no cell for %s in %s", service, env)
	return Cell{}
}

func newService(t *testing.T, w *world, cfg Config) *Service {
	t.Helper()
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC) }
	}
	return New(w.config(cfg))
}

func TestVersionFromTagThenDigestThenImage(t *testing.T) {
	w := newWorld()
	pushed := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	// a: the tag wins over the image tag.
	w.run(acctEU, "main", "a", repoURI(acctEU, "a")+":9.9.9", map[string]string{"yumdrake:version": "2.0.0"})
	// b: no tag; the image is a digest, ECR has tags for it.
	w.run(acctEU, "main", "b", repoURI(acctEU, "b")+"@sha256:bbbb", nil)
	w.addImage(acctEU, "b", "sha256:bbbb", pushed, "latest", "1.5.0", "1.4.0")
	// c: no tag, no digest match; the image reference's tag.
	w.run(acctEU, "main", "c", repoURI(acctEU, "c")+":3.1.0", nil)
	// d: nothing to read a version from.
	w.run(acctEU, "main", "d", repoURI(acctEU, "d"), nil)
	// e: a sidecar comes first; the container of the service's repository counts.
	w.runContainers(acctEU, "main", "e", nil,
		ecstypes.ContainerDefinition{Name: aws.String("xray"), Image: aws.String("public.ecr.aws/xray/daemon:3.0.0")},
		ecstypes.ContainerDefinition{Name: aws.String("app"), Image: aws.String(repoURI(acctEU, "e") + ":4.2.0")})
	// f: a digest ECR knows nothing about, and no tag in the reference.
	w.run(acctEU, "main", "f", repoURI(acctEU, "f")+"@sha256:ffff", nil)

	inv := inventoryOf([]spec.Environment{envEU},
		serviceSpec("a", "semver", envEU), serviceSpec("b", "semver", envEU), serviceSpec("c", "semver", envEU),
		serviceSpec("d", "semver", envEU), serviceSpec("e", "semver", envEU), serviceSpec("f", "semver", envEU))
	m, err := newService(t, w, Config{}).Matrix(context.Background(), inv)
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}

	for service, want := range map[string]string{"a": "2.0.0", "b": "1.5.0", "c": "3.1.0", "d": "", "e": "4.2.0", "f": ""} {
		cell := cellOf(t, m, service, "prod-eu")
		if cell.State != "ok" || cell.Version != want {
			t.Errorf("%s: state %q version %q, want ok %q", service, cell.State, cell.Version, want)
		}
	}
	if got := cellOf(t, m, "a", "prod-eu").TaskDefinition; got != "a:7" {
		t.Errorf("task definition = %q, want a:7", got)
	}
	for _, session := range w.sessions {
		if session != "yumdrake-orchestrator-running" {
			t.Errorf("session name = %q, want yumdrake-orchestrator-running", session)
		}
	}
}

func TestBehindWithinLabelOnly(t *testing.T) {
	w := newWorld()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	later := old.Add(48 * time.Hour)
	envs := []spec.Environment{envEU, envUS, envStage}

	// semver: prod-eu 1.0.0 is behind prod-us 1.2.0; stage runs 3.0.0, which
	// never makes a prod environment behind, and is not behind either.
	w.run(acctEU, "main", "sem", repoURI(acctEU, "sem")+":1.0.0", nil)
	w.run(acctUS, "main", "sem", repoURI(acctUS, "sem")+":1.2.0", nil)
	w.run(acctStage, "main", "sem", repoURI(acctStage, "sem")+":3.0.0", nil)
	// pushed: the tag names say nothing about order; push time does.
	w.run(acctEU, "main", "psh", repoURI(acctEU, "psh")+":blue", nil)
	w.run(acctUS, "main", "psh", repoURI(acctUS, "psh")+":green", nil)
	w.run(acctStage, "main", "psh", repoURI(acctStage, "psh")+":red", nil)
	w.addImage(acctEU, "psh", "sha256:1", later, "blue")
	w.addImage(acctUS, "psh", "sha256:2", old, "green")
	w.addImage(acctStage, "psh", "sha256:3", later.Add(time.Hour), "red")
	// same: the same version in both prod environments is not behind.
	w.run(acctEU, "main", "same", repoURI(acctEU, "same")+":v1", nil)
	w.run(acctUS, "main", "same", repoURI(acctUS, "same")+":v1", nil)
	w.addImage(acctEU, "same", "sha256:4", later, "v1")
	w.addImage(acctUS, "same", "sha256:5", old, "v1")

	inv := inventoryOf(envs,
		serviceSpec("sem", "semver", envs...), serviceSpec("psh", "pushed", envs...), serviceSpec("same", "pushed", envEU, envUS))
	m, err := newService(t, w, Config{}).Matrix(context.Background(), inv)
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}
	for _, tc := range []struct {
		service, env string
		behind       bool
	}{
		{"sem", "prod-eu", true}, {"sem", "prod-us", false}, {"sem", "stage", false},
		{"psh", "prod-eu", false}, {"psh", "prod-us", true}, {"psh", "stage", false},
		{"same", "prod-eu", false}, {"same", "prod-us", false},
	} {
		if got := cellOf(t, m, tc.service, tc.env).Behind; got != tc.behind {
			t.Errorf("%s in %s: behind = %v, want %v", tc.service, tc.env, got, tc.behind)
		}
	}
}

func TestOneEnvironmentFailingMarksOnlyItsCells(t *testing.T) {
	w := newWorld()
	envs := []spec.Environment{envEU, envUS, envStage}
	for _, name := range []string{"api", "web"} {
		w.run(acctEU, "main", name, repoURI(acctEU, name)+":1.0.0", nil)
		w.run(acctUS, "main", name, repoURI(acctUS, name)+":1.0.0", nil)
		w.run(acctStage, "main", name, repoURI(acctStage, name)+":1.0.0", nil)
	}
	// prod-eu: web is gone, api is fine.
	delete(w.services, acctEU+"|main|web")
	// prod-us: AWS refuses; its message must never reach a cell.
	w.failCluster[acctUS+"|main"] = &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "arn:aws:iam::222222222222:role/secret-detail is not authorized"}
	// stage: the account is not connected.
	w.failAccount[acctStage] = fmt.Errorf("lookup: %w", discovery.ErrNotConnected)

	checks := &recordedChecks{}
	inv := inventoryOf(envs, serviceSpec("api", "semver", envs...), serviceSpec("web", "semver", envs...))
	m, err := newService(t, w, Config{Checks: checks}).Matrix(context.Background(), inv)
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}

	if c := cellOf(t, m, "api", "prod-eu"); c.State != "ok" || c.Version != "1.0.0" {
		t.Errorf("api prod-eu = %+v, want ok 1.0.0", c)
	}
	if c := cellOf(t, m, "web", "prod-eu"); c.State != "error" || c.Error != "ServiceNotFound" {
		t.Errorf("web prod-eu = %+v, want error ServiceNotFound", c)
	}
	for _, name := range []string{"api", "web"} {
		if c := cellOf(t, m, name, "prod-us"); c.State != "error" || c.Error != "AccessDeniedException" {
			t.Errorf("%s prod-us = %+v, want error AccessDeniedException", name, c)
		}
		if c := cellOf(t, m, name, "stage"); c.State != "error" || c.Error != "NotConnected" {
			t.Errorf("%s stage = %+v, want error NotConnected", name, c)
		}
	}
	// A missing service is not an AWS problem; a refusal is; an account that is
	// not connected has no connection to record on.
	if want := []string{"aws:" + acctEU}; fmt.Sprint(checks.oks) != fmt.Sprint(want) {
		t.Errorf("connection ok checks = %v, want %v", checks.oks, want)
	}
	if len(checks.errors) != 1 || checks.errors["aws:"+acctUS] != "AccessDeniedException" {
		t.Errorf("connection error checks = %v, want only %s: AccessDeniedException", checks.errors, acctUS)
	}
}

func TestInactiveServiceIsNotFoundAndTimeoutIsACode(t *testing.T) {
	w := newWorld()
	w.run(acctEU, "main", "old", repoURI(acctEU, "old")+":1.0.0", nil)
	w.inactive[acctEU+"|main|old"] = true
	w.run(acctUS, "main", "slow", repoURI(acctUS, "slow")+":1.0.0", nil)
	w.failCluster[acctUS+"|main"] = context.DeadlineExceeded
	envs := []spec.Environment{envEU, envUS}
	m, err := newService(t, w, Config{}).Matrix(context.Background(),
		inventoryOf(envs, serviceSpec("old", "semver", envEU), serviceSpec("slow", "semver", envUS)))
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}
	if c := cellOf(t, m, "old", "prod-eu"); c.State != "error" || c.Error != "ServiceNotFound" {
		t.Errorf("inactive service = %+v, want error ServiceNotFound", c)
	}
	if c := cellOf(t, m, "slow", "prod-us"); c.State != "error" || c.Error != "Timeout" {
		t.Errorf("timed out service = %+v, want error Timeout", c)
	}
}

func TestAbsentService(t *testing.T) {
	w := newWorld()
	w.run(acctEU, "main", "orders", repoURI(acctEU, "orders")+":1.0.0", nil)
	w.run(acctEU, "main", "api", repoURI(acctEU, "api")+":2.0.0", nil)
	w.run(acctStage, "main", "api", repoURI(acctStage, "api")+":2.0.0", nil)
	envs := []spec.Environment{envEU, envStage}
	inv := inventoryOf(envs, serviceSpec("orders", "semver", envEU), serviceSpec("api", "semver", envEU, envStage))
	m, err := newService(t, w, Config{}).Matrix(context.Background(), inv)
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}
	if c := cellOf(t, m, "orders", "stage"); c.State != "absent" || c.Error != "" || c.Version != "" {
		t.Errorf("orders in stage = %+v, want absent", c)
	}
	if got := m.Rows[0].Service + " " + m.Rows[1].Service; got != "api orders" {
		t.Errorf("rows = %s, want sorted by service name", got)
	}
	for _, row := range m.Rows {
		if len(row.Cells) != len(envs) {
			t.Errorf("row %s has %d cells, want one per environment", row.Service, len(row.Cells))
		}
	}
	if len(m.Environments) != 2 || m.Environments[0].Name != "prod-eu" {
		t.Errorf("environments = %+v, want the inventory's in order", m.Environments)
	}
}

func TestBatchesOfTenAndBoundedConcurrency(t *testing.T) {
	w := newWorld()
	w.delay = 15 * time.Millisecond
	envs := []spec.Environment{envEU, envUS}
	var services []spec.Service
	for i := range 25 {
		name := fmt.Sprintf("svc-%02d", i)
		w.run(acctEU, "main", name, repoURI(acctEU, name)+":1.0.0", nil)
		w.run(acctUS, "main", name, repoURI(acctUS, name)+":1.0.0", nil)
		services = append(services, serviceSpec(name, "semver", envs...))
	}
	m, err := newService(t, w, Config{}).Matrix(context.Background(), inventoryOf(envs, services...))
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}
	if len(m.Rows) != 25 {
		t.Fatalf("rows = %d, want 25", len(m.Rows))
	}
	sort.Ints(w.batches)
	if want := []int{5, 5, 10, 10, 10, 10}; fmt.Sprint(w.batches) != fmt.Sprint(want) {
		t.Errorf("DescribeServices batch sizes = %v, want %v (25 services per cluster, two clusters)", w.batches, want)
	}
	if got := w.maxInflight.Load(); got > 8 {
		t.Errorf("max AWS calls in flight = %d, want at most 8 across the whole matrix", got)
	}
	if got := w.maxInflight.Load(); got < 2 {
		t.Errorf("max AWS calls in flight = %d, want the calls to overlap", got)
	}
}

func TestClusterFailureMarksOnlyItsServices(t *testing.T) {
	w := newWorld()
	w.run(acctEU, "red", "a", repoURI(acctEU, "a")+":1.0.0", nil)
	w.run(acctEU, "blue", "b", repoURI(acctEU, "b")+":1.0.0", nil)
	w.failCluster[acctEU+"|blue"] = &smithy.GenericAPIError{Code: "ClusterNotFoundException"}
	a := serviceSpec("a", "semver", envEU)
	a.Environments["prod-eu"] = spec.ServiceEnv{ECSService: serviceARN(acctEU, "red", "a"), ECRRepository: repoURI(acctEU, "a")}
	b := serviceSpec("b", "semver", envEU)
	b.Environments["prod-eu"] = spec.ServiceEnv{ECSService: serviceARN(acctEU, "blue", "b"), ECRRepository: repoURI(acctEU, "b")}
	m, err := newService(t, w, Config{}).Matrix(context.Background(), inventoryOf([]spec.Environment{envEU}, a, b))
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}
	if c := cellOf(t, m, "a", "prod-eu"); c.State != "ok" {
		t.Errorf("a = %+v, want ok: only the blue cluster failed", c)
	}
	if c := cellOf(t, m, "b", "prod-eu"); c.State != "error" || c.Error != "ClusterNotFoundException" {
		t.Errorf("b = %+v, want error ClusterNotFoundException", c)
	}
}

func TestForServiceReturnsTheRowsCells(t *testing.T) {
	w := newWorld()
	w.run(acctEU, "main", "api", repoURI(acctEU, "api")+":1.0.0", nil)
	envs := []spec.Environment{envEU, envStage}
	svc := newService(t, w, Config{})
	inv := inventoryOf(envs, serviceSpec("api", "semver", envEU))
	cells, err := svc.ForService(context.Background(), inv, "api")
	if err != nil {
		t.Fatalf("ForService: %v", err)
	}
	if len(cells) != 2 || cells[0].State != "ok" || cells[1].State != "absent" {
		t.Errorf("cells = %+v, want ok then absent", cells)
	}
	if _, err := svc.ForService(context.Background(), inv, "nope"); err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Errorf("unknown service error = %v", err)
	}
}

func TestHangingEnvironmentTimesOutAloneAndPageStillRenders(t *testing.T) {
	w := newWorld()
	envs := []spec.Environment{envEU, envUS}
	w.run(acctEU, "main", "api", repoURI(acctEU, "api")+":1.0.0", nil)
	w.run(acctUS, "main", "api", repoURI(acctUS, "api")+":1.0.0", nil)
	w.hangAccount[acctUS] = true

	svc := newService(t, w, Config{EnvTimeout: 200 * time.Millisecond})
	start := time.Now()
	m, err := svc.Matrix(context.Background(), inventoryOf(envs, serviceSpec("api", "semver", envs...)))
	if err != nil {
		t.Fatalf("Matrix: %v, want the page to render with the hanging environment marked", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("Matrix took %v, want it bounded by the environment timeout", took)
	}
	if c := cellOf(t, m, "api", "prod-us"); c.State != "error" || c.Error != "Timeout" {
		t.Errorf("hanging environment cell = %+v, want error Timeout", c)
	}
	if c := cellOf(t, m, "api", "prod-eu"); c.State != "ok" || c.Version != "1.0.0" {
		t.Errorf("healthy environment cell = %+v, want ok 1.0.0", c)
	}

	// A cancelled request is still an error, not a page of timeouts.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.Matrix(ctx, inventoryOf(envs, serviceSpec("api", "semver", envs...))); err == nil {
		t.Errorf("Matrix with a cancelled request context returned no error")
	}
}
