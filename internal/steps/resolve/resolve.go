// Package resolve is the "resolve" executor: it turns a planned run into a
// pinned manifest and runs the preflight checks, reading ECS, ECR, GitHub and
// S3 and storing copies of the config files in the artifact bucket. It makes
// no change to any environment, so it needs no Env.Do: a rerun reads the same
// inputs, writes the same artifact keys and gives the same identity.
//
// The executor never writes a file's content, or any AWS error message, to a
// log line, a check or an outcome. Files are named with their size and the
// first 12 characters of their SHA-256; AWS failures with their error code and
// request id.
package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/rcss-labs/yamdrake/internal/artifacts"
	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/engine"
	"github.com/rcss-labs/yamdrake/internal/github"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/runner"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
)

const defaultParallel = 8

// Check names. The revalidate step and the web app read these.
const (
	checkServiceExists = "service exists"
	checkImageExists   = "image exists"
	checkConfigSource  = "config source"        // followed by " <file name>"
	checkDestination   = "destination readable" // followed by " <file name>"
	checkWrite         = "write permission"
)

// Source reads the env-file repository. Plan 2's *github.Client satisfies it
// with repo written "owner/name".
type Source interface {
	BranchExists(ctx context.Context, repo, branch string) (bool, error)
	HeadCommit(ctx context.Context, repo, branch string) (string, error)
	ReadFile(ctx context.Context, repo, commit, path string) ([]byte, error)
}

// ArtifactStore keeps the config copies. *artifacts.S3Store satisfies it.
type ArtifactStore interface {
	Put(ctx context.Context, runID int64, service, kind, name string, previous bool, data []byte) (key, sha256hex string, err error)
}

// Deps is what the executor needs.
type Deps struct {
	// AWS returns the deployment configuration for an account and region;
	// deploy.Provider.For satisfies it. The session name given is
	// "yumdrake-runner-run-<run id>".
	AWS func(ctx context.Context, accountID, region, session string) (aws.Config, error)
	// Clients builds the clients over a configuration. Nil means NewClients(cfg, Options).
	Clients func(cfg aws.Config) Clients
	// GitHub returns the source for a repository owner.
	GitHub func(ctx context.Context, owner string) (Source, error)
	// Artifacts stores the copies, with the runner's own identity.
	Artifacts ArtifactStore
	// Inventory returns the inventory snapshot a run was planned against, by
	// id (store.SnapshotByID), not the active one.
	Inventory func(ctx context.Context, snapshotID int64) (spec.Inventory, error)
	// Options are the deployment-wide AWS settings (emulator endpoint); they
	// decide how ECR repository URIs are parsed and S3 is addressed.
	Options awsx.Options
	// Parallel bounds how many services are resolved at once. Default 8.
	Parallel int
	// RetryBase is the first wait before revalidate repeats a transient AWS
	// call; each retry doubles it. Default 500 ms.
	RetryBase time.Duration
}

type executor struct{ d Deps }

// New returns the "resolve" executor.
func New(d Deps) runner.Executor {
	if d.Parallel < 1 {
		d.Parallel = defaultParallel
	}
	if d.Clients == nil {
		options := d.Options
		d.Clients = func(cfg aws.Config) Clients { return NewClients(cfg, options) }
	}
	return &executor{d: d}
}

// target is one service to resolve.
type target struct {
	name, group, version string
	env                  spec.ServiceEnv
}

// Execute resolves the run's selection into a manifest. See the package comment.
func (x *executor) Execute(ctx context.Context, env runner.Env, task queue.Claimed) queue.Outcome {
	run := task.Run
	if env.CancelRequested(ctx) {
		return queue.Outcome{State: "cancelled"}
	}
	inv, err := x.d.Inventory(ctx, run.InventorySnapshotID)
	if err != nil {
		env.Log("error", fmt.Sprintf("Could not read inventory snapshot %d: %v", run.InventorySnapshotID, err))
		return failed("could not read the inventory snapshot", err.Error())
	}
	targets, err := targetsOf(run.Selection, inv, run.Environment)
	if err != nil {
		env.Log("error", "Could not work out the services of the run: "+err.Error())
		return failed("the run's selection does not match its inventory snapshot", err.Error())
	}
	env.Log("info", fmt.Sprintf("Resolving %d services for %s (%s) in account %s.", len(targets), run.Environment, run.Label, run.AWSAccount))

	r := &resolution{
		x: x, env: env, run: run,
		session: fmt.Sprintf("yumdrake-runner-run-%d", run.ID),
		clients: map[string]*clientsEntry{}, sources: map[string]*sourceEntry{},
	}
	results := make([]serviceResult, len(targets))
	slots := make(chan struct{}, x.d.Parallel)
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			results[i] = r.resolveService(ctx, targets[i])
		}()
	}
	wg.Wait()

	if ctx.Err() != nil {
		return failed("resolving was interrupted", ctx.Err().Error())
	}
	if env.CancelRequested(ctx) {
		return queue.Outcome{State: "cancelled"}
	}

	m := manifest.Manifest{
		RunID: run.ID, Environment: run.Environment, Label: run.Label, AWSAccount: run.AWSAccount,
		InventoryCommit: run.InventoryCommit, JobCommit: run.JobCommit,
		Services: make([]manifest.Service, 0, len(results)), Checks: []manifest.Check{},
	}
	for _, res := range results {
		m.Services = append(m.Services, res.service)
		m.Checks = append(m.Checks, res.checks...)
	}
	m.ID = manifest.Identity(m)
	encoded, err := json.Marshal(m)
	if err != nil {
		return failed("could not encode the manifest", err.Error())
	}

	failedChecks := m.Failed()
	if len(failedChecks) == 0 {
		env.Log("ok", "Manifest "+m.ID[:12]+" resolved; every check passed or is not verifiable.")
		return queue.Outcome{State: "succeeded", Result: encoded}
	}
	lines := make([]string, len(failedChecks))
	for i, c := range failedChecks {
		lines[i] = fmt.Sprintf("%s: %s: %s", c.Service, c.Name, c.Detail)
	}
	env.Log("error", fmt.Sprintf("Preflight failed: %d checks.", len(failedChecks)))
	return queue.Outcome{
		State:         "failed",
		FailureShort:  fmt.Sprintf("preflight failed: %d checks", len(failedChecks)),
		FailureDetail: strings.Join(lines, "\n"),
		Result:        encoded,
	}
}

func failed(short, detail string) queue.Outcome {
	return queue.Outcome{State: "failed", FailureShort: short, FailureDetail: detail}
}

// targetsOf lists the selected services, sorted by name, with the version
// each gets (the service's override, else its group's) and its entry for the
// environment in the inventory snapshot.
func targetsOf(rawSelection []byte, inv spec.Inventory, environment string) ([]target, error) {
	var selection engine.Selection
	if err := json.Unmarshal(rawSelection, &selection); err != nil {
		return nil, fmt.Errorf("decode the selection: %w", err)
	}
	groups := make([]string, 0, len(selection))
	for group := range selection {
		groups = append(groups, group)
	}
	sort.Strings(groups)

	chosen := map[string]target{}
	for _, group := range groups {
		choice := selection[group]
		for name, override := range choice.Services {
			version := override
			if version == "" {
				version = choice.Version
			}
			if version == "" {
				return nil, fmt.Errorf("service %q has no version", name)
			}
			if prior, dup := chosen[name]; dup {
				if prior.version != version {
					return nil, fmt.Errorf("service %q is selected in groups %q and %q with different versions", name, prior.group, group)
				}
				continue
			}
			entry, ok := inv.Services[name].Environments[environment]
			if !ok {
				return nil, fmt.Errorf("service %q has no entry for environment %q in the inventory snapshot", name, environment)
			}
			chosen[name] = target{name: name, group: group, version: version, env: entry}
		}
	}
	if len(chosen) == 0 {
		return nil, errors.New("no service is selected")
	}
	targets := make([]target, 0, len(chosen))
	for _, t := range chosen {
		targets = append(targets, t)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
	return targets, nil
}

// resolution is the state shared by the services of one attempt.
type resolution struct {
	x       *executor
	env     runner.Env
	run     store.Run
	session string

	mu      sync.Mutex
	clients map[string]*clientsEntry // by region
	sources map[string]*sourceEntry  // by repository owner
}

type clientsEntry struct {
	once sync.Once
	c    Clients
	err  error
}

type sourceEntry struct {
	once sync.Once
	s    Source
	err  error
}

// clientsFor returns the clients of the run's account in region. The
// credential request is made, and logged, once per region.
func (r *resolution) clientsFor(ctx context.Context, region string) (Clients, error) {
	r.mu.Lock()
	entry, ok := r.clients[region]
	if !ok {
		entry = &clientsEntry{}
		r.clients[region] = entry
	}
	r.mu.Unlock()
	entry.once.Do(func() {
		callCtx, cancel := context.WithTimeout(ctx, CallTimeout)
		defer cancel()
		cfg, err := r.x.d.AWS(callCtx, r.run.AWSAccount, region, r.session)
		if err == nil {
			clients := r.x.d.Clients(cfg)
			// A credential of another account would read that account's services.
			if err = CheckCallerAccount(callCtx, clients, r.run.AWSAccount); err == nil {
				entry.c = clients
			}
		}
		if err != nil {
			entry.err = err
			r.env.Log("error", "Credentials for account "+r.run.AWSAccount+" in "+region+": "+ErrorDetail("sign-in", err))
			return
		}
		r.env.Log("info", "Credentials for account "+r.run.AWSAccount+" in "+region+": ok.")
	})
	return entry.c, entry.err
}

func (r *resolution) sourceFor(ctx context.Context, owner string) (Source, error) {
	r.mu.Lock()
	entry, ok := r.sources[owner]
	if !ok {
		entry = &sourceEntry{}
		r.sources[owner] = entry
	}
	r.mu.Unlock()
	entry.once.Do(func() {
		entry.s, entry.err = r.x.d.GitHub(ctx, owner)
		if entry.err != nil {
			// The factory's error text is not echoed: only its own wiring knows
			// whether it is free of credentials, and the log must not depend on that.
			r.env.Log("error", "GitHub access for "+owner+" failed.")
		}
	})
	return entry.s, entry.err
}

type serviceResult struct {
	service manifest.Service
	checks  []manifest.Check
}

// serviceRun resolves one service and collects its checks.
type serviceRun struct {
	r      *resolution
	t      target
	region string // where the service runs; S3 is read there too

	svc                 manifest.Service
	core, sources, dest []manifest.Check
}

func (r *resolution) resolveService(ctx context.Context, t target) (result serviceResult) {
	s := &serviceRun{r: r, t: t}
	s.svc = manifest.Service{
		Name: t.name, Group: t.group, Version: t.version,
		ECSService: t.env.ECSService, ECRRepository: t.env.ECRRepository, Config: []manifest.ConfigItem{},
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logf("error", "internal error while resolving this service; the details are not logged")
			s.core = append(s.core, manifest.Check{Name: "internal error", Service: t.name, Status: "failed", Detail: "the resolver stopped unexpectedly"})
			result = s.result()
		}
	}()

	s.checkService(ctx)
	s.checkImage(ctx)
	for _, f := range t.env.EnvFiles {
		s.envFile(ctx, f)
	}
	for _, f := range t.env.Tomls {
		s.toml(ctx, f)
	}
	s.add(&s.dest, checkWrite, "not_verified", "cannot be proven without writing; the first write is the proof")
	return s.result()
}

func (s *serviceRun) result() serviceResult {
	checks := append(append(append([]manifest.Check{}, s.core...), s.sources...), s.dest...)
	return serviceResult{service: s.svc, checks: checks}
}

func (s *serviceRun) add(into *[]manifest.Check, name, status, detail string) {
	*into = append(*into, manifest.Check{Name: name, Service: s.t.name, Status: status, Detail: detail})
	level := map[string]string{"passed": "ok", "failed": "error", "not_verified": "warn"}[status]
	s.logf(level, "Check %q: %s. %s", name, strings.ReplaceAll(status, "_", " "), detail)
}

func (s *serviceRun) logf(level, format string, args ...any) {
	s.r.env.Log(level, "["+s.t.name+"] "+fmt.Sprintf(format, args...))
}

// call logs an AWS or GitHub call with its outcome and returns its error.
// The call runs under CallTimeout, derived from the leg's context.
func (s *serviceRun) call(ctx context.Context, operation, subject string, fn func(ctx context.Context) error) error {
	callCtx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	err := fn(callCtx)
	if err != nil {
		s.logf("error", "%s %s: %s", operation, subject, ErrorDetail(operation, err))
		return err
	}
	s.logf("info", "%s %s: ok", operation, subject)
	return nil
}

// --- ECS --------------------------------------------------------------------

func (s *serviceRun) checkService(ctx context.Context) {
	ref, err := awsx.ParseECSService(s.t.env.ECSService)
	if err != nil {
		s.add(&s.core, checkServiceExists, "failed", err.Error())
		return
	}
	s.region = ref.Region
	if ref.Account != s.r.run.AWSAccount {
		s.add(&s.core, checkServiceExists, "failed", fmt.Sprintf("the service is in account %s, not the environment's account %s", ref.Account, s.r.run.AWSAccount))
		return
	}
	clients, err := s.r.clientsFor(ctx, ref.Region)
	if err != nil {
		s.add(&s.core, checkServiceExists, "failed", ErrorDetail("sign-in", err))
		return
	}

	var described *ecs.DescribeServicesOutput
	err = s.call(ctx, "ecs:DescribeServices", ref.Service, func(ctx context.Context) (callErr error) {
		described, callErr = clients.ECS.DescribeServices(ctx, &ecs.DescribeServicesInput{
			Cluster: aws.String(ref.Cluster), Services: []string{ref.Service},
		})
		return callErr
	})
	if err != nil {
		s.add(&s.core, checkServiceExists, "failed", ErrorDetail("ecs:DescribeServices", err))
		return
	}
	// Only a service of the environment's account and region counts; a name
	// alone never matches. ECS answers with the service asked for, so when
	// exactly one in-scope service comes back it is that one.
	var candidates []*ecstypes.Service
	var outOfScope []string
	for i := range described.Services {
		candidate := &described.Services[i]
		if problem := ARNScopeProblem(aws.ToString(candidate.ServiceArn), s.r.run.AWSAccount, ref.Region); problem != "" {
			outOfScope = append(outOfScope, problem)
			continue
		}
		candidates = append(candidates, candidate)
	}
	var found *ecstypes.Service
	for _, candidate := range candidates {
		if aws.ToString(candidate.ServiceArn) == s.t.env.ECSService || aws.ToString(candidate.ServiceName) == ref.Service {
			found = candidate
			break
		}
	}
	if found == nil && len(candidates) == 1 && len(outOfScope) == 0 {
		found = candidates[0]
	}
	if found == nil && len(outOfScope) > 0 {
		s.add(&s.core, checkServiceExists, "failed", fmt.Sprintf("service %s in cluster %s was refused: %s", ref.Service, ref.Cluster, outOfScope[0]))
		return
	}
	if found == nil {
		reason := "not found"
		if len(described.Failures) > 0 {
			reason = strings.ToLower(aws.ToString(described.Failures[0].Reason))
		}
		s.add(&s.core, checkServiceExists, "failed", fmt.Sprintf("service %s in cluster %s: %s", ref.Service, ref.Cluster, reason))
		return
	}
	if status := aws.ToString(found.Status); status != "ACTIVE" {
		s.add(&s.core, checkServiceExists, "failed", fmt.Sprintf("service %s is %s, not ACTIVE", ref.Service, status))
		return
	}
	taskDefinition := aws.ToString(found.TaskDefinition)
	if taskDefinition == "" {
		s.add(&s.core, checkServiceExists, "failed", "the service reports no task definition")
		return
	}

	var td *ecs.DescribeTaskDefinitionOutput
	err = s.call(ctx, "ecs:DescribeTaskDefinition", taskDefinition, func(ctx context.Context) (callErr error) {
		td, callErr = clients.ECS.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
			TaskDefinition: aws.String(taskDefinition), Include: []ecstypes.TaskDefinitionField{ecstypes.TaskDefinitionFieldTags},
		})
		return callErr
	})
	if err != nil {
		s.add(&s.core, checkServiceExists, "failed", ErrorDetail("ecs:DescribeTaskDefinition", err))
		return
	}
	s.svc.PreviousTaskDefinition = taskDefinition
	s.svc.PreviousVersion = previousVersion(td, s.t.env.ECRRepository)
	s.add(&s.core, checkServiceExists, "passed", "ACTIVE, running "+taskDefinition)
}

// previousVersion is the version the service runs now: the "version" tag of
// its task definition, else the tag of the container image that comes from the
// service's repository, else the first tagged container image. "" if unknown.
func previousVersion(td *ecs.DescribeTaskDefinitionOutput, repositoryURI string) string {
	for _, tag := range td.Tags {
		if aws.ToString(tag.Key) == "version" || aws.ToString(tag.Key) == "yumdrake:version" {
			return aws.ToString(tag.Value)
		}
	}
	if td.TaskDefinition == nil {
		return ""
	}
	repoName := repositoryURI
	if i := strings.Index(repositoryURI, "/"); i >= 0 {
		repoName = repositoryURI[i+1:]
	}
	first := ""
	for _, container := range td.TaskDefinition.ContainerDefinitions {
		image := aws.ToString(container.Image)
		tag := imageTag(image)
		if tag == "" {
			continue
		}
		if strings.HasSuffix(strings.TrimSuffix(image, ":"+tag), "/"+repoName) {
			return tag
		}
		if first == "" {
			first = tag
		}
	}
	return first
}

// imageTag returns the tag of an image reference, "" for none or a digest.
func imageTag(image string) string {
	if strings.Contains(image, "@") {
		return ""
	}
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		return image[colon+1:]
	}
	return ""
}

// --- ECR --------------------------------------------------------------------

func (s *serviceRun) checkImage(ctx context.Context) {
	ref, err := awsx.ParseECRRepository(s.t.env.ECRRepository, s.r.x.d.Options)
	if err != nil {
		s.add(&s.core, checkImageExists, "failed", err.Error())
		return
	}
	if s.region == "" {
		s.region = ref.Region // the service ARN was unusable; S3 follows the repository
	}
	clients, err := s.r.clientsFor(ctx, ref.Region)
	if err != nil {
		s.add(&s.core, checkImageExists, "failed", ErrorDetail("sign-in", err))
		return
	}
	var out *ecr.DescribeImagesOutput
	err = s.call(ctx, "ecr:DescribeImages", ref.Name+":"+s.t.version, func(ctx context.Context) (callErr error) {
		out, callErr = clients.ECR.DescribeImages(ctx, &ecr.DescribeImagesInput{
			RegistryId: aws.String(ref.Account), RepositoryName: aws.String(ref.Name),
			ImageIds: []ecrtypes.ImageIdentifier{{ImageTag: aws.String(s.t.version)}},
		})
		return callErr
	})
	if err != nil {
		s.add(&s.core, checkImageExists, "failed", fmt.Sprintf("tag %s in %s: %s", s.t.version, ref.Name, ErrorDetail("ecr:DescribeImages", err)))
		return
	}
	// One digest can come back as several entries (one per tag); group them.
	digests := map[string]bool{}
	for _, detail := range out.ImageDetails {
		if digest := aws.ToString(detail.ImageDigest); digest != "" {
			digests[digest] = true
		}
	}
	switch len(digests) {
	case 0:
		s.add(&s.core, checkImageExists, "failed", fmt.Sprintf("no image with tag %s in repository %s", s.t.version, ref.Name))
	case 1:
		for digest := range digests {
			s.svc.ImageDigest = digest
			s.add(&s.core, checkImageExists, "passed", fmt.Sprintf("tag %s is %s", s.t.version, digest))
		}
	default:
		s.add(&s.core, checkImageExists, "failed", fmt.Sprintf("tag %s in repository %s names %d different images", s.t.version, ref.Name, len(digests)))
	}
}

// --- config -----------------------------------------------------------------

// fileFact is a file read from a source or a destination.
type fileFact struct {
	data []byte
	sum  string
}

func factOf(data []byte) fileFact {
	sum := sha256.Sum256(data)
	return fileFact{data: data, sum: hex.EncodeToString(sum[:])}
}

// describe is how a file is named in logs and checks: size and a short checksum.
func (f fileFact) describe() string {
	return fmt.Sprintf("%d bytes, sha256 %s", len(f.data), f.sum[:12])
}

func (s *serviceRun) envFile(ctx context.Context, f spec.EnvFile) {
	item := manifest.ConfigItem{Kind: "env", Name: f.Name, Source: f.Source, Destination: f.Destination, Change: "unknown"}
	name := checkConfigSource + " " + f.Name
	content, detail := s.readEnvSource(ctx, f, &item)
	if detail != "" {
		s.add(&s.sources, name, "failed", detail)
	} else if s.storeSource(ctx, &item, content, name) {
		s.add(&s.sources, name, "passed", s.envDetail(item, content))
	}
	s.destination(ctx, &item, content)
	s.svc.Config = append(s.svc.Config, item)
}

func (s *serviceRun) envDetail(item manifest.ConfigItem, content fileFact) string {
	if item.Fallback {
		return fmt.Sprintf("branch %s does not exist; fell back to %s at commit %s (%s)", BranchFor(s.t.version), item.Branch, item.Commit, content.describe())
	}
	return fmt.Sprintf("branch %s at commit %s (%s)", item.Branch, item.Commit, content.describe())
}

// readEnvSource decides the branch, then reads the file at the branch's head
// commit. A non-empty detail is the failure.
func (s *serviceRun) readEnvSource(ctx context.Context, f spec.EnvFile, item *manifest.ConfigItem) (fileFact, string) {
	owner, rest, _ := strings.Cut(f.Source, "/")
	repoName, path, found := strings.Cut(rest, "/")
	if owner == "" || repoName == "" || !found || path == "" {
		return fileFact{}, fmt.Sprintf("source %q is not <owner>/<repo>/<path>", f.Source)
	}
	repo := owner + "/" + repoName
	source, err := s.r.sourceFor(ctx, owner)
	if err != nil {
		return fileFact{}, "no GitHub access for " + owner
	}

	versionBranch := BranchFor(s.t.version)
	var exists bool
	if err := s.call(ctx, "github:BranchExists", repo+"@"+versionBranch, func(ctx context.Context) (callErr error) {
		exists, callErr = source.BranchExists(ctx, repo, versionBranch)
		return callErr
	}); err != nil {
		return fileFact{}, "could not look up branch " + versionBranch + ": " + githubDetail(err)
	}
	branch, fallback, ok := ChooseBranch(exists, s.r.run.Label)
	if !ok {
		return fileFact{}, fmt.Sprintf("branch %s does not exist in %s and label %q has no fallback branch", versionBranch, repo, s.r.run.Label)
	}
	if branch == "" {
		branch = versionBranch
	}
	item.Branch, item.Fallback = branch, fallback

	var commit string
	if err := s.call(ctx, "github:HeadCommit", repo+"@"+branch, func(ctx context.Context) (callErr error) {
		commit, callErr = source.HeadCommit(ctx, repo, branch)
		return callErr
	}); err != nil {
		return fileFact{}, "could not read the head of branch " + branch + ": " + githubDetail(err)
	}
	item.Commit = commit

	var data []byte
	if err := s.call(ctx, "github:ReadFile", fmt.Sprintf("%s %s@%s", f.Name, repo, commit), func(ctx context.Context) (callErr error) {
		data, callErr = source.ReadFile(ctx, repo, commit, path)
		return callErr
	}); err != nil {
		return fileFact{}, fmt.Sprintf("could not read %s on branch %s at commit %s: %s", path, branch, commit, githubDetail(err))
	}
	fact := factOf(data)
	s.logf("info", "Env file %s (source): %s", f.Name, fact.describe())
	return fact, ""
}

func githubDetail(err error) string {
	var limited *github.RateLimitError
	switch {
	case errors.Is(err, github.ErrNotFound):
		return "not found"
	case errors.As(err, &limited):
		return "rate limited"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "interrupted"
	default:
		return err.Error() // the client's errors carry the method, path and status only
	}
}

func (s *serviceRun) toml(ctx context.Context, f spec.Toml) {
	item := manifest.ConfigItem{Kind: "toml", Name: f.Name, Source: f.Source, Destination: f.Destination, Change: "unknown"}
	name := checkConfigSource + " " + f.Name
	var content fileFact
	ref, err := awsx.ParseS3(f.Source)
	switch {
	case err != nil:
		s.add(&s.sources, name, "failed", err.Error())
	default:
		data, version, found, err := s.getObject(ctx, ref, f.Name+" source")
		switch {
		case err != nil:
			s.add(&s.sources, name, "failed", "could not read "+f.Source+": "+err.Error())
		case !found:
			s.add(&s.sources, name, "failed", f.Source+" does not exist")
		default:
			content = factOf(data)
			item.SourceVersionID = version
			s.logf("info", "TOML %s (source): %s", f.Name, content.describe())
			if s.storeSource(ctx, &item, content, name) {
				detail := content.describe()
				if version != "" {
					detail += ", version " + version
				}
				s.add(&s.sources, name, "passed", detail)
			}
		}
	}
	s.destination(ctx, &item, content)
	s.svc.Config = append(s.svc.Config, item)
}

// storeSource records the file's checksum and size and stores its copy. On a
// failure it adds the failed check under name and reports false.
func (s *serviceRun) storeSource(ctx context.Context, item *manifest.ConfigItem, content fileFact, name string) bool {
	item.SHA256, item.Size = content.sum, int64(len(content.data))
	key, err := s.put(ctx, item, false, content)
	if err != nil {
		s.add(&s.sources, name, "failed", err.Error())
		return false
	}
	item.ArtifactKey = key
	return true
}

// put stores one copy and checks the store's checksum against ours.
func (s *serviceRun) put(ctx context.Context, item *manifest.ConfigItem, previous bool, content fileFact) (string, error) {
	var key, sum string
	label := item.Name + " copy"
	if previous {
		label = item.Name + " previous copy"
	}
	err := s.call(ctx, "artifacts:Put", label, func(ctx context.Context) (callErr error) {
		key, sum, callErr = s.r.x.d.Artifacts.Put(ctx, s.r.run.ID, s.t.name, item.Kind, item.Name, previous, content.data)
		return callErr
	})
	switch {
	case errors.Is(err, artifacts.ErrTooLarge):
		return "", fmt.Errorf("%s is larger than the %d byte limit", item.Name, artifacts.MaxObjectBytes)
	case err != nil:
		return "", errors.New(ErrorDetail("storing the copy of "+item.Name, err))
	case sum != content.sum:
		return "", fmt.Errorf("the artifact store computed a different checksum for %s", item.Name)
	}
	return key, nil
}

// destination reads what the destination holds now, keeps a copy of it and
// sets the item's change and key diff against content (the new file, which may
// be absent when its source failed).
func (s *serviceRun) destination(ctx context.Context, item *manifest.ConfigItem, content fileFact) {
	name := checkDestination + " " + item.Name
	ref, err := awsx.ParseS3(item.Destination)
	if err != nil {
		s.add(&s.dest, name, "failed", err.Error())
		return
	}
	data, _, found, err := s.getObject(ctx, ref, item.Name+" destination")
	if err != nil {
		s.add(&s.dest, name, "failed", "could not read "+item.Destination+": "+err.Error())
		return
	}
	if !found {
		item.PreviousExists = false
		if content.sum != "" {
			item.Change = "new"
		}
		s.add(&s.dest, name, "passed", "does not exist yet")
		return
	}
	previous := factOf(data)
	s.logf("info", "%s %s (destination, previous): %s", strings.ToUpper(item.Kind), item.Name, previous.describe())
	item.PreviousExists, item.PreviousSHA256 = true, previous.sum
	key, err := s.put(ctx, item, true, previous)
	if err != nil {
		s.add(&s.dest, name, "failed", err.Error())
		return
	}
	item.PreviousArtifactKey = key
	switch content.sum {
	case "":
		// The new file is unknown; the source check has failed already.
	case previous.sum:
		item.Change = "unchanged"
	default:
		item.Change = "changed"
		if item.Kind == "env" {
			diff := manifest.EnvKeyDiff(previous.data, content.data)
			item.Keys = &diff
		}
	}
	s.add(&s.dest, name, "passed", "readable, "+previous.describe())
}

// getObject reads an S3 object, at most artifacts.MaxObjectBytes of it. A
// missing object is found == false, not an error. The error text is safe to
// show: it carries the AWS code and request id and nothing else.
func (s *serviceRun) getObject(ctx context.Context, ref awsx.S3Ref, label string) (data []byte, versionID string, found bool, err error) {
	if s.region == "" {
		return nil, "", false, errors.New("the region is unknown")
	}
	clients, err := s.r.clientsFor(ctx, s.region)
	if err != nil {
		return nil, "", false, errors.New(ErrorDetail("sign-in", err))
	}
	// The body is read inside the call: its context must outlive the read.
	found = true
	var readErr error
	callErr := s.call(ctx, "s3:GetObject", fmt.Sprintf("s3://%s/%s (%s)", ref.Bucket, ref.Key, label), func(ctx context.Context) error {
		out, err := clients.S3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(ref.Bucket), Key: aws.String(ref.Key)})
		if isMissing(err) {
			found = false
			return nil
		}
		if err != nil {
			return err
		}
		defer func() { _ = out.Body.Close() }()
		versionID = aws.ToString(out.VersionId)
		data, readErr = readLimited(out.Body)
		return nil
	})
	switch {
	case callErr != nil:
		return nil, "", false, errors.New(ErrorDetail("s3:GetObject", callErr))
	case !found:
		s.logf("info", "s3:GetObject %s: the object does not exist", label)
		return nil, "", false, nil
	case readErr != nil:
		return nil, "", false, readErr
	}
	return data, versionID, true, nil
}

func isMissing(err error) bool {
	if err == nil {
		return false
	}
	var noKey *s3types.NoSuchKey
	var notFound *s3types.NotFound
	if errors.As(err, &noKey) || errors.As(err, &notFound) {
		return true
	}
	switch ErrorCode(err) {
	case "NoSuchKey", "NotFound":
		return true
	}
	return false
}

// readLimited reads a body of at most artifacts.MaxObjectBytes.
func readLimited(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, artifacts.MaxObjectBytes+1))
	if err != nil {
		return nil, errors.New("reading the object failed")
	}
	if len(data) > artifacts.MaxObjectBytes {
		return nil, fmt.Errorf("the object is larger than the %d byte limit", artifacts.MaxObjectBytes)
	}
	return data, nil
}
