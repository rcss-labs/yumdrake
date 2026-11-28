// Package running reads which version each service runs in each environment,
// from ECS, with the facts that come from AWS cached in Valkey and the facts
// that come from the database computed on every request.
package running

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/awsx/discovery"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/versions"
)

const (
	// maxConcurrentCalls bounds the AWS calls in flight in one Matrix call.
	maxConcurrentCalls = 8
	// describeServicesBatch is the most services DescribeServices takes.
	describeServicesBatch = 10
	// describeImagesBatch is the most image ids DescribeImages takes.
	describeImagesBatch = 100
	// cacheTTL keeps an environment that read cleanly; failedTTL keeps one
	// with an error, so it is retried soon.
	cacheTTL  = 2 * time.Minute
	failedTTL = 20 * time.Second
	// valkeyTimeout bounds each Valkey call: against a dead server valkey-go
	// keeps redialling until its context ends.
	valkeyTimeout = 2 * time.Second
	// envReadTimeout bounds one environment's whole read, so one account that
	// does not answer cannot hold the call slots or fail the page.
	envReadTimeout = 10 * time.Second
	// sessionName names the assumed-role session in AWS's logs.
	sessionName = "yumdrake-orchestrator-running"

	// The tags a Yumdrake deploy puts on the task definition it registers.
	tagVersion = "yumdrake:version"
	tagRun     = "yumdrake:run"
	tagLeg     = "yumdrake:leg"

	// Cell states.
	StateOK     = "ok"
	StateAbsent = "absent"
	StateError  = "error"

	codeNotConnected    = "NotConnected"
	codeServiceNotFound = "ServiceNotFound"
	codeInvalidConfig   = "InvalidConfiguration"
	connectionKindAWS   = "aws"
)

// ErrUnknownService means the inventory has no such service.
var ErrUnknownService = errors.New("running: unknown service")

// Discovery builds the discovery configuration of an account;
// *discovery.Provider satisfies it.
type Discovery interface {
	For(ctx context.Context, accountID, region, sessionName string) (aws.Config, error)
}

// ECS is the part of the ECS client that Service uses.
type ECS interface {
	DescribeServices(ctx context.Context, in *ecs.DescribeServicesInput, o ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
	DescribeTaskDefinition(ctx context.Context, in *ecs.DescribeTaskDefinitionInput, o ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error)
}

// ECR is the part of the ECR client that Service uses.
type ECR interface {
	DescribeImages(ctx context.Context, in *ecr.DescribeImagesInput, o ...func(*ecr.Options)) (*ecr.DescribeImagesOutput, error)
}

// Checks records whether an AWS account could be read. Both methods are
// best-effort: the manager wires them to the connection-check store.
type Checks interface {
	OK(ctx context.Context, kind, ref string, at time.Time)
	Error(ctx context.Context, kind, ref, code string, at time.Time)
}

// Config holds the collaborators of a Service.
type Config struct {
	// Discovery supplies the read-only credentials of each account.
	Discovery Discovery
	// NewECS and NewECR build clients from a configuration; the defaults are
	// the SDK's. Tests replace them.
	NewECS func(aws.Config) ECS
	NewECR func(aws.Config) ECR
	// Valkey caches each environment's AWS facts. Optional: without it, or
	// while it is down, every call reads AWS.
	Valkey valkey.Client
	// KeyPrefix goes in front of every Valkey key.
	KeyPrefix string
	// AWS is the deployment-wide AWS options (the emulator endpoint in tests).
	AWS awsx.Options
	// DB answers the questions the database knows: who deployed a version and
	// whether the last deploy failed. Optional: without it those are false.
	DB store.DBTX
	// Checks records connection outcomes. Optional.
	Checks Checks
	// Now is the clock; the default is time.Now.
	Now func() time.Time
	// EnvTimeout bounds one environment's whole read (credentials, waiting for
	// a call slot, every AWS call); the default is 10 seconds. Cells still
	// unread when it ends become error Timeout and the others are untouched.
	EnvTimeout time.Duration
}

// Cell is one service in one environment. Error is an AWS error code, never
// an AWS message.
type Cell struct {
	// State is "ok", "absent" (the service is not in this environment) or "error".
	State string `json:"state"`
	// Version comes from the task definition's yumdrake:version tag; else a tag
	// ECR has for the image digest; else the image reference's tag; else "".
	Version        string `json:"version"`
	TaskDefinition string `json:"task_definition"` // family:revision
	ByYumdrake     bool   `json:"by_yumdrake"`
	// RunID is the run that applied it, when ByYumdrake.
	RunID int64 `json:"run_id"`
	// LastDeployFailed: the newest finished ecs-deploy leg for this service in
	// this environment failed.
	LastDeployFailed bool `json:"last_deploy_failed"`
	// Behind: an environment with the same label runs a newer version by the
	// service's rule.
	Behind bool      `json:"behind"`
	Error  string    `json:"error"`
	ReadAt time.Time `json:"read_at"`

	pushedAt time.Time // when the running image was pushed; zero when unknown
}

// Row is one service across the environments, cells in environment order.
type Row struct {
	Service string `json:"service"`
	Cells   []Cell `json:"cells"`
}

// Matrix is every inventory service by every environment; rows are sorted by
// service name. Neither list is ever nil.
type Matrix struct {
	Environments []spec.Environment `json:"environments"`
	Rows         []Row              `json:"rows"`
}

// Service reads running versions.
type Service struct{ cfg Config }

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.NewECS == nil {
		cfg.NewECS = func(c aws.Config) ECS { return ecs.NewFromConfig(c) }
	}
	if cfg.NewECR == nil {
		cfg.NewECR = func(c aws.Config) ECR { return ecr.NewFromConfig(c) }
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg}
}

// Matrix returns the matrix, reading AWS only for environments that are not
// cached. An environment that cannot be read marks only its own cells.
func (s *Service) Matrix(ctx context.Context, inv spec.Inventory) (Matrix, error) {
	return s.matrix(ctx, inv, false)
}

// MatrixFresh is Matrix without reading the cache; it still writes it.
func (s *Service) MatrixFresh(ctx context.Context, inv spec.Inventory) (Matrix, error) {
	return s.matrix(ctx, inv, true)
}

// ForService returns the cells of one service in environment order, or
// ErrUnknownService.
func (s *Service) ForService(ctx context.Context, inv spec.Inventory, name string) ([]Cell, error) {
	if _, ok := inv.Services[name]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownService, name)
	}
	m, err := s.matrix(ctx, inv, false)
	if err != nil {
		return nil, err
	}
	for _, row := range m.Rows {
		if row.Service == name {
			return row.Cells, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownService, name)
}

// fact is what AWS said about one service in one environment: the part of a
// cell that is cached. Run and Leg are the raw yumdrake:run and yumdrake:leg
// tags.
type fact struct {
	Service        string    `json:"service"`
	State          string    `json:"state"`
	Version        string    `json:"version"`
	TaskDefinition string    `json:"task_definition"`
	Run            string    `json:"run"`
	Leg            string    `json:"leg"`
	PushedAt       time.Time `json:"pushed_at"`
	Error          string    `json:"error"`
	ReadAt         time.Time `json:"read_at"`
}

func (s *Service) matrix(ctx context.Context, inv spec.Inventory, fresh bool) (Matrix, error) {
	envs := inv.Environments
	sem := make(chan struct{}, maxConcurrentCalls)

	facts := make([]map[string]fact, len(envs))
	var wg sync.WaitGroup
	for i, env := range envs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			byService := map[string]fact{}
			for _, f := range s.environmentFacts(ctx, sem, inv, env, fresh) {
				byService[f.Service] = f
			}
			facts[i] = byService
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return Matrix{}, err
	}

	names := inv.ServiceNames()
	now := s.cfg.Now()
	m := Matrix{Environments: append([]spec.Environment{}, envs...), Rows: make([]Row, 0, len(names))}
	for _, name := range names {
		row := Row{Service: name, Cells: make([]Cell, len(envs))}
		for i := range envs {
			f, ok := facts[i][name]
			if !ok {
				row.Cells[i] = Cell{State: StateAbsent, ReadAt: now}
				continue
			}
			row.Cells[i] = Cell{State: f.State, Version: f.Version, TaskDefinition: f.TaskDefinition,
				Error: f.Error, ReadAt: f.ReadAt, pushedAt: f.PushedAt}
		}
		m.Rows = append(m.Rows, row)
	}

	if s.cfg.DB != nil {
		if err := s.applyDatabaseFacts(ctx, envs, facts, &m); err != nil {
			return Matrix{}, err
		}
	}
	for i := range m.Rows {
		markBehind(envs, inv.Services[m.Rows[i].Service].Versions.Sort, m.Rows[i].Cells)
	}
	return m, nil
}

// markBehind sets Behind on the cells that run an older version than another
// cell of the same environment label. Different labels never compare.
func markBehind(envs []spec.Environment, sortRule string, cells []Cell) {
	for i := range cells {
		if cells[i].State != StateOK || cells[i].Version == "" {
			continue
		}
		for j := range cells {
			if i == j || cells[j].State != StateOK || cells[j].Version == "" || envs[i].Label != envs[j].Label {
				continue
			}
			if sortRule == "semver" {
				if versions.CompareSemver(cells[j].Version, cells[i].Version) > 0 {
					cells[i].Behind = true
				}
			} else if !cells[i].pushedAt.IsZero() && cells[j].pushedAt.After(cells[i].pushedAt) && cells[j].Version != cells[i].Version {
				cells[i].Behind = true
			}
		}
	}
}

// ---- cache ----

// environmentFacts returns the AWS facts of every service that has a block in
// env: from Valkey, or else from AWS (and then cached and reported to Checks).
func (s *Service) environmentFacts(ctx context.Context, sem chan struct{}, inv spec.Inventory, env spec.Environment, fresh bool) []fact {
	key := s.cacheKey(inv, env)
	if !fresh {
		if cached, ok := s.cacheGet(ctx, key); ok {
			return cached
		}
	}
	timeout := s.cfg.EnvTimeout
	if timeout <= 0 {
		timeout = envReadTimeout
	}
	envCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	facts := (&reader{s: s, sem: sem, env: env}).read(envCtx, inv)
	if ctx.Err() != nil { // the request, not the environment's own deadline
		return facts // cancelled: never cache or report half a read
	}
	s.cacheSet(ctx, key, facts)
	s.recordCheck(ctx, env, facts)
	return facts
}

// cacheKey is <prefix>running:<environment>:<16 hex of a hash over the
// account and the sorted (service, ecs_service, ecr_repository) triples>, so a
// changed inventory is not served an old read.
func (s *Service) cacheKey(inv spec.Inventory, env spec.Environment) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00", env.AWSAccount)
	for _, name := range inv.ServiceNames() {
		if block, ok := inv.Services[name].Environments[env.Name]; ok {
			_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00", name, block.ECSService, block.ECRRepository)
		}
	}
	return fmt.Sprintf("%srunning:%s:%s", s.cfg.KeyPrefix, env.Name, hex.EncodeToString(h.Sum(nil)[:8]))
}

// cacheGet and cacheSet ignore every Valkey failure: the cache only saves
// calls to AWS.
func (s *Service) cacheGet(ctx context.Context, key string) ([]fact, bool) {
	if s.cfg.Valkey == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, valkeyTimeout)
	defer cancel()
	raw, err := s.cfg.Valkey.Do(ctx, s.cfg.Valkey.B().Get().Key(key).Build()).AsBytes()
	if err != nil {
		return nil, false
	}
	var facts []fact
	if json.Unmarshal(raw, &facts) != nil {
		return nil, false
	}
	return facts, true
}

func (s *Service) cacheSet(ctx context.Context, key string, facts []fact) {
	if s.cfg.Valkey == nil {
		return
	}
	ttl := cacheTTL
	for _, f := range facts {
		if f.State == StateError {
			ttl = failedTTL
			break
		}
	}
	raw, err := json.Marshal(facts)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, valkeyTimeout)
	defer cancel()
	s.cfg.Valkey.Do(ctx, s.cfg.Valkey.B().Set().Key(key).Value(string(raw)).Ex(ttl).Build())
}

// recordCheck tells Checks how reading the environment's account went: an AWS
// or credential error is recorded as such; a missing service, an account that
// is not connected and a bad inventory entry are not about the connection.
func (s *Service) recordCheck(ctx context.Context, env spec.Environment, facts []fact) {
	if s.cfg.Checks == nil {
		return
	}
	var worked bool
	for _, f := range facts {
		switch {
		case f.State == StateOK:
			worked = true
		case f.State == StateError && f.Error != codeServiceNotFound && f.Error != codeNotConnected && f.Error != codeInvalidConfig:
			s.cfg.Checks.Error(ctx, connectionKindAWS, env.AWSAccount, f.Error, s.cfg.Now())
			return
		}
	}
	if worked {
		s.cfg.Checks.OK(ctx, connectionKindAWS, env.AWSAccount, s.cfg.Now())
	}
}

// ---- reading one environment from AWS ----

// target is one service of the environment being read.
type target struct {
	name string
	spec spec.ServiceEnv
	ecs  awsx.ECSServiceRef
	f    fact

	// Filled by the ECS phase.
	taskDefARN string
	image      string // the image of the container that runs this service's repository
	tagVersion string

	// Set by the ECR phase.
	ecrVersion string
}

type clusterKey struct{ account, region, cluster string }

type reader struct {
	s   *Service
	sem chan struct{}
	env spec.Environment
}

// call runs fn while holding one of the slots that bound the AWS calls in
// flight; it gives up when ctx ends first.
func (r *reader) call(ctx context.Context, fn func()) error {
	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.sem }()
	fn()
	return nil
}

func (r *reader) discover(ctx context.Context, account, region string) (cfg aws.Config, err error) {
	if callErr := r.call(ctx, func() { cfg, err = r.s.cfg.Discovery.For(ctx, account, region, sessionName) }); callErr != nil {
		return aws.Config{}, callErr
	}
	return cfg, err
}

func (t *target) fail(code string, at time.Time) {
	t.f.State, t.f.Error, t.f.ReadAt = StateError, code, at
}

func (r *reader) read(ctx context.Context, inv spec.Inventory) []fact {
	now := r.s.cfg.Now()
	var targets []*target
	for _, name := range inv.ServiceNames() {
		block, ok := inv.Services[name].Environments[r.env.Name]
		if !ok {
			continue
		}
		t := &target{name: name, spec: block, f: fact{Service: name, State: StateOK, ReadAt: now}}
		ref, err := awsx.ParseECSService(block.ECSService)
		if err != nil {
			t.fail(codeInvalidConfig, now)
		}
		t.ecs = ref
		targets = append(targets, t)
	}

	// Phase 1: per cluster, the services and then their task definitions.
	clusters := map[clusterKey][]*target{}
	for _, t := range targets {
		if t.f.State == StateOK {
			key := clusterKey{t.ecs.Account, t.ecs.Region, t.ecs.Cluster}
			clusters[key] = append(clusters[key], t)
		}
	}
	var wg sync.WaitGroup
	for key, group := range clusters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.readCluster(ctx, key, group)
		}()
	}
	wg.Wait()

	// Phase 2: per repository, the ECR facts for the services that read.
	r.readImages(ctx, targets)

	facts := make([]fact, len(targets))
	for i, t := range targets {
		if t.f.State == StateOK && t.f.Version == "" {
			t.f.Version = t.ecrVersion
			if t.f.Version == "" {
				_, t.f.Version, _ = splitImage(t.image)
			}
		}
		facts[i] = t.f
	}
	return facts
}

func (r *reader) readCluster(ctx context.Context, key clusterKey, group []*target) {
	now := r.s.cfg.Now()
	failAll := func(code string) {
		for _, t := range group {
			t.fail(code, now)
		}
	}
	cfg, err := r.discover(ctx, key.account, key.region)
	if errors.Is(err, discovery.ErrNotConnected) {
		failAll(codeNotConnected)
		return
	}
	if err != nil {
		failAll(errorCode(err))
		return
	}
	client := r.s.cfg.NewECS(cfg)

	var wg sync.WaitGroup
	for start := 0; start < len(group); start += describeServicesBatch {
		batch := group[start:min(start+describeServicesBatch, len(group))]
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.describeServices(ctx, client, key, batch)
		}()
	}
	wg.Wait()

	// One DescribeTaskDefinition per distinct task definition.
	byARN := map[string][]*target{}
	for _, t := range group {
		if t.f.State == StateOK && t.taskDefARN != "" {
			byARN[t.taskDefARN] = append(byARN[t.taskDefARN], t)
		}
	}
	for arn, users := range byARN {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.describeTaskDefinition(ctx, client, arn, users)
		}()
	}
	wg.Wait()
}

func (r *reader) describeServices(ctx context.Context, client ECS, key clusterKey, batch []*target) {
	now := r.s.cfg.Now()
	names := make([]string, len(batch))
	for i, t := range batch {
		names[i] = t.ecs.Service
	}
	var out *ecs.DescribeServicesOutput
	var err error
	if callErr := r.call(ctx, func() {
		out, err = client.DescribeServices(ctx, &ecs.DescribeServicesInput{Cluster: aws.String(key.cluster), Services: names})
	}); callErr != nil {
		err = callErr
	}
	if err != nil {
		for _, t := range batch {
			t.fail(errorCode(err), now)
		}
		return
	}
	found := map[string]ecstypes.Service{}
	for _, svc := range out.Services {
		found[aws.ToString(svc.ServiceName)] = svc
	}
	for _, t := range batch {
		svc, ok := found[t.ecs.Service]
		if !ok || aws.ToString(svc.Status) == "INACTIVE" {
			// Listed under failures, or neither: the service is not there.
			t.fail(codeServiceNotFound, now)
			continue
		}
		t.taskDefARN = aws.ToString(svc.TaskDefinition)
	}
}

func (r *reader) describeTaskDefinition(ctx context.Context, client ECS, arn string, users []*target) {
	now := r.s.cfg.Now()
	var out *ecs.DescribeTaskDefinitionOutput
	var err error
	if callErr := r.call(ctx, func() {
		out, err = client.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
			TaskDefinition: aws.String(arn), Include: []ecstypes.TaskDefinitionField{ecstypes.TaskDefinitionFieldTags}})
	}); callErr != nil {
		err = callErr
	}
	if err != nil || out.TaskDefinition == nil {
		code := "RequestFailed"
		if err != nil {
			code = errorCode(err)
		}
		for _, t := range users {
			t.fail(code, now)
		}
		return
	}
	tags := map[string]string{}
	for _, tag := range out.Tags {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	for _, t := range users {
		t.f.TaskDefinition = aws.ToString(out.TaskDefinition.Family) + ":" + strconv.Itoa(int(out.TaskDefinition.Revision))
		t.f.Run, t.f.Leg = tags[tagRun], tags[tagLeg]
		t.tagVersion = tags[tagVersion]
		t.f.Version = t.tagVersion
		t.image = imageOf(out.TaskDefinition.ContainerDefinitions, t.spec.ECRRepository)
	}
}

// imageOf is the image of the container whose repository is repository, or
// else of the first container.
func imageOf(containers []ecstypes.ContainerDefinition, repository string) string {
	for _, c := range containers {
		if repo, _, _ := splitImage(aws.ToString(c.Image)); repo == repository {
			return aws.ToString(c.Image)
		}
	}
	if len(containers) > 0 {
		return aws.ToString(containers[0].Image)
	}
	return ""
}

// splitImage splits repo[:tag][@digest].
func splitImage(ref string) (repo, tag, digest string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref, digest = ref[:i], ref[i+1:]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref, tag = ref[:i], ref[i+1:]
	}
	return ref, tag, digest
}

// imageNeed is what one service needs from ECR: its image by digest when the
// reference has one, else by the tag it was resolved to.
type imageNeed struct {
	t           *target
	digest, tag string
}

func (r *reader) readImages(ctx context.Context, targets []*target) {
	byRepo := map[awsx.ECRRepoRef][]imageNeed{}
	for _, t := range targets {
		if t.f.State != StateOK || t.image == "" {
			continue
		}
		repo, err := awsx.ParseECRRepository(t.spec.ECRRepository, r.s.cfg.AWS)
		if err != nil {
			continue
		}
		_, imageTag, digest := splitImage(t.image)
		need := imageNeed{t: t, digest: digest}
		if digest == "" {
			need.tag = t.tagVersion
			if need.tag == "" {
				need.tag = imageTag
			}
			if need.tag == "" {
				continue
			}
		}
		byRepo[repo] = append(byRepo[repo], need)
	}
	var wg sync.WaitGroup
	for repo, needs := range byRepo {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.readRepository(ctx, repo, needs)
		}()
	}
	wg.Wait()
}

// readRepository makes the DescribeImages calls of one repository. A failure
// here leaves the versions as the task definition and image reference gave
// them: ECR only adds to the answer.
func (r *reader) readRepository(ctx context.Context, repo awsx.ECRRepoRef, needs []imageNeed) {
	cfg, err := r.discover(ctx, repo.Account, repo.Region)
	if err != nil {
		return
	}
	client := r.s.cfg.NewECR(cfg)

	var ids []ecrtypes.ImageIdentifier
	seen := map[string]bool{}
	for _, n := range needs {
		id := ecrtypes.ImageIdentifier{}
		switch {
		case n.digest != "":
			id.ImageDigest = aws.String(n.digest)
		default:
			id.ImageTag = aws.String(n.tag)
		}
		if key := n.digest + "|" + n.tag; !seen[key] {
			seen[key] = true
			ids = append(ids, id)
		}
	}
	var details []ecrtypes.ImageDetail
	for start := 0; start < len(ids); start += describeImagesBatch {
		details = append(details, r.describeImages(ctx, client, repo, ids[start:min(start+describeImagesBatch, len(ids))])...)
	}

	for _, n := range needs {
		for _, d := range details {
			switch {
			case n.digest != "" && aws.ToString(d.ImageDigest) == n.digest:
				n.t.f.PushedAt = aws.ToTime(d.ImagePushedAt)
				n.t.ecrVersion = pickTag(d.ImageTags)
			case n.digest == "" && containsTag(d.ImageTags, n.tag):
				n.t.f.PushedAt = aws.ToTime(d.ImagePushedAt)
			default:
				continue
			}
			break
		}
	}
}

// describeImages asks for the ids. ECR fails the whole call when any id is
// unknown, so then each id is asked for alone and the unknown ones skipped.
func (r *reader) describeImages(ctx context.Context, client ECR, repo awsx.ECRRepoRef, ids []ecrtypes.ImageIdentifier) []ecrtypes.ImageDetail {
	ask := func(ids []ecrtypes.ImageIdentifier) ([]ecrtypes.ImageDetail, error) {
		var out *ecr.DescribeImagesOutput
		var err error
		if callErr := r.call(ctx, func() {
			out, err = client.DescribeImages(ctx, &ecr.DescribeImagesInput{
				RegistryId: aws.String(repo.Account), RepositoryName: aws.String(repo.Name), ImageIds: ids})
		}); callErr != nil {
			return nil, callErr
		}
		if err != nil {
			return nil, err
		}
		return out.ImageDetails, nil
	}
	details, err := ask(ids)
	if err == nil {
		return details
	}
	if errorCode(err) != "ImageNotFoundException" || len(ids) < 2 {
		return nil
	}
	var all []ecrtypes.ImageDetail
	for _, id := range ids {
		if one, err := ask([]ecrtypes.ImageIdentifier{id}); err == nil {
			all = append(all, one...)
		}
	}
	return all
}

func containsTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

// pickTag chooses the tag to show for an image that has several: the highest
// semantic version, never "latest" when there is another, and the first in
// sort order among non-versions.
func pickTag(tags []string) string {
	var best string
	for _, tag := range tags {
		switch {
		case best == "":
			best = tag
		case best == "latest" && tag != "latest":
			best = tag
		case tag == "latest":
		case versions.CompareSemver(tag, best) > 0 || (versions.CompareSemver(tag, best) == 0 && tag < best):
			best = tag
		}
	}
	return best
}

// errorCode is the AWS error code of err, and never its message, which can
// carry request details. The same rule as the version lists.
func errorCode(err error) string {
	var coded interface{ ErrorCode() string }
	switch {
	case errors.As(err, &coded) && coded.ErrorCode() != "":
		return coded.ErrorCode()
	case errors.Is(err, context.DeadlineExceeded):
		return "Timeout"
	default:
		return "RequestFailed"
	}
}

// ---- facts from the database ----

type cellKey struct{ env, service string }

// applyDatabaseFacts sets ByYumdrake, RunID and LastDeployFailed with one
// query each, for all cells at once. They are never cached: a deploy that
// finishes a second after a read must show at once.
func (s *Service) applyDatabaseFacts(ctx context.Context, envs []spec.Environment, facts []map[string]fact, m *Matrix) error {
	// Candidates for ByYumdrake: ok cells whose task definition carries both ids.
	var legIDs, runIDs []int64
	var services, envNames []string
	claimedRun := map[cellKey]int64{}
	for i, env := range envs {
		for service, f := range facts[i] {
			runID, runErr := strconv.ParseInt(f.Run, 10, 64)
			legID, legErr := strconv.ParseInt(f.Leg, 10, 64)
			if f.State != StateOK || runErr != nil || legErr != nil || runID <= 0 || legID <= 0 {
				continue
			}
			legIDs, runIDs = append(legIDs, legID), append(runIDs, runID)
			services, envNames = append(services, service), append(envNames, env.Name)
			claimedRun[cellKey{env.Name, service}] = runID
		}
	}
	proven := map[cellKey]bool{}
	if len(legIDs) > 0 {
		// The leg must exist, belong to the tagged run, be an ecs-deploy step
		// leg of this service, and the run must be of this environment.
		rows, err := s.cfg.DB.Query(ctx, `
			SELECT c.environment, c.service
			FROM unnest($1::bigint[], $2::bigint[], $3::text[], $4::text[]) AS c(leg_id, run_id, service, environment)
			JOIN legs l ON l.id = c.leg_id AND l.run_id = c.run_id AND l.service = c.service AND l.kind = 'step'
			JOIN run_steps st ON st.id = l.step_id AND st.use = 'ecs-deploy'
			JOIN runs r ON r.id = l.run_id AND r.environment = c.environment`,
			legIDs, runIDs, services, envNames)
		if err != nil {
			return fmt.Errorf("running: deploy provenance: %w", err)
		}
		for rows.Next() {
			var key cellKey
			if err := rows.Scan(&key.env, &key.service); err != nil {
				rows.Close()
				return fmt.Errorf("running: deploy provenance: %w", err)
			}
			proven[key] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("running: deploy provenance: %w", err)
		}
	}

	// LastDeployFailed: the newest finished ecs-deploy step leg per service and environment.
	envList := make([]string, len(envs))
	for i, env := range envs {
		envList[i] = env.Name
	}
	serviceList := make([]string, len(m.Rows))
	for i, row := range m.Rows {
		serviceList[i] = row.Service
	}
	failed := map[cellKey]bool{}
	rows, err := s.cfg.DB.Query(ctx, `
		SELECT DISTINCT ON (r.environment, l.service) r.environment, l.service, l.state
		FROM legs l
		JOIN run_steps st ON st.id = l.step_id AND st.use = 'ecs-deploy'
		JOIN runs r ON r.id = l.run_id
		WHERE l.kind = 'step' AND l.finished_at IS NOT NULL
		  AND r.environment = ANY($1) AND l.service = ANY($2)
		ORDER BY r.environment, l.service, l.id DESC`, envList, serviceList)
	if err != nil {
		return fmt.Errorf("running: last deploys: %w", err)
	}
	for rows.Next() {
		var key cellKey
		var state string
		if err := rows.Scan(&key.env, &key.service, &state); err != nil {
			rows.Close()
			return fmt.Errorf("running: last deploys: %w", err)
		}
		failed[key] = state == "failed"
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("running: last deploys: %w", err)
	}

	for r := range m.Rows {
		for i, env := range envs {
			key := cellKey{env.Name, m.Rows[r].Service}
			cell := &m.Rows[r].Cells[i]
			if cell.State == StateAbsent {
				continue
			}
			if proven[key] {
				cell.ByYumdrake, cell.RunID = true, claimedRun[key]
			}
			cell.LastDeployFailed = failed[key]
		}
	}
	return nil
}
