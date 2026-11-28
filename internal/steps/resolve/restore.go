package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/rcss-labs/yamdrake/internal/artifacts"
	"github.com/rcss-labs/yamdrake/internal/engine"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// Check names of a restore.
const (
	checkTaskDefinition = "task definition exists"
	checkConfigCopy     = "config copy" // followed by " <file name>"
)

// restoreGroup is the selection group of a restore run (engine.CreateRestore).
const restoreGroup = "restore"

const configCopyExpired = "config copy expired; only the task definition is restored"

// ArtifactReader reads a stored copy and checks its SHA-256. The artifact
// store satisfies it; Deps.Artifacts must also implement it for a restore.
type ArtifactReader interface {
	Get(ctx context.Context, key, wantSHA256 string) ([]byte, error)
}

// restoreOf is what a restore target carries from the run it restores.
type restoreOf struct {
	run     int64
	service manifest.Service
}

// restoreHeader is what the manifest of a restore takes from the source run's.
type restoreHeader struct {
	run                        int64
	inventoryCommit, jobCommit string
}

// restoreTargets lists the services the restore run chose, from the plan of
// the run it restores. Their versions are the versions that run replaced.
func (x *executor) restoreTargets(ctx context.Context, run store.Run) (restoreHeader, []target, error) {
	if run.ParentRunID == nil {
		return restoreHeader{}, nil, errors.New("the restore run does not say which run it restores")
	}
	if x.d.SourceRun == nil {
		return restoreHeader{}, nil, errors.New("this runner cannot read the run to restore")
	}
	source, err := x.d.SourceRun(ctx, *run.ParentRunID)
	if err != nil {
		return restoreHeader{}, nil, fmt.Errorf("read run #%d: %w", *run.ParentRunID, err)
	}
	var sourcePlan manifest.Manifest
	if len(source.Manifest) == 0 || json.Unmarshal(source.Manifest, &sourcePlan) != nil {
		return restoreHeader{}, nil, fmt.Errorf("run #%d has no plan that can be read", *run.ParentRunID)
	}
	var selection engine.Selection
	if err := json.Unmarshal(run.Selection, &selection); err != nil {
		return restoreHeader{}, nil, fmt.Errorf("decode the selection: %w", err)
	}
	names := make([]string, 0, len(selection[restoreGroup].Services))
	for name := range selection[restoreGroup].Services {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return restoreHeader{}, nil, errors.New("no service is selected")
	}
	byName := map[string]manifest.Service{}
	for _, s := range sourcePlan.Services {
		byName[s.Name] = s
	}
	targets := make([]target, 0, len(names))
	for _, name := range names {
		service, ok := byName[name]
		if !ok {
			return restoreHeader{}, nil, fmt.Errorf("run #%d did not deploy service %q", *run.ParentRunID, name)
		}
		targets = append(targets, target{
			name: name, group: service.Group, version: service.PreviousVersion,
			env:     spec.ServiceEnv{ECSService: service.ECSService, ECRRepository: service.ECRRepository},
			restore: &restoreOf{run: *run.ParentRunID, service: service},
		})
	}
	return restoreHeader{run: *run.ParentRunID, inventoryCommit: sourcePlan.InventoryCommit, jobCommit: sourcePlan.JobCommit}, targets, nil
}

// restore resolves one service of a restore: the target is the task
// definition and the config copies the source run replaced. What the service
// and its destinations hold now is recorded as the previous state, so the
// restore can itself be restored.
func (s *serviceRun) restore(ctx context.Context) {
	source := s.t.restore.service
	s.svc.TargetTaskDefinition = source.PreviousTaskDefinition

	s.checkService(ctx) // the current task definition and version, and the region
	s.checkTargetTaskDefinition(ctx, source.PreviousTaskDefinition)
	for _, item := range source.Config {
		s.restoreItem(ctx, item)
	}
	s.add(&s.dest, checkWrite, "not_verified", "cannot be proven without writing; the first write is the proof")
}

// checkTargetTaskDefinition requires the task definition to restore to to be
// registered and ACTIVE, and takes the digest of the service's image from it.
func (s *serviceRun) checkTargetTaskDefinition(ctx context.Context, target string) {
	if target == "" {
		s.add(&s.core, checkTaskDefinition, "failed", fmt.Sprintf("run #%d did not record the task definition the service ran before", s.t.restore.run))
		return
	}
	if s.region == "" {
		s.add(&s.core, checkTaskDefinition, "failed", "the region is unknown")
		return
	}
	clients, err := s.r.clientsFor(ctx, s.region)
	if err != nil {
		s.add(&s.core, checkTaskDefinition, "failed", ErrorDetail("sign-in", err))
		return
	}
	var out *ecs.DescribeTaskDefinitionOutput
	err = s.call(ctx, "ecs:DescribeTaskDefinition", target, func(ctx context.Context) (callErr error) {
		out, callErr = clients.ECS.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(target)})
		return callErr
	})
	switch {
	case ErrorCode(err) == "ClientException": // what ECS answers for a task definition it does not know
		s.add(&s.core, checkTaskDefinition, "failed", fmt.Sprintf("the previous task definition %s no longer exists", target))
		return
	case err != nil:
		s.add(&s.core, checkTaskDefinition, "failed", ErrorDetail("ecs:DescribeTaskDefinition", err))
		return
	case out.TaskDefinition == nil:
		s.add(&s.core, checkTaskDefinition, "failed", fmt.Sprintf("the previous task definition %s no longer exists", target))
		return
	}
	if status := string(out.TaskDefinition.Status); status != "ACTIVE" {
		if status == "" {
			status = "not ACTIVE"
		}
		s.add(&s.core, checkTaskDefinition, "failed", fmt.Sprintf("the previous task definition %s is %s", target, status))
		return
	}
	s.svc.ImageDigest = digestOfRepository(out.TaskDefinition.ContainerDefinitions, s.t.env.ECRRepository)
	s.add(&s.core, checkTaskDefinition, "passed", "ACTIVE, "+target)
}

// digestOfRepository is the digest of the first container whose image comes
// from the repository and is pinned by digest; "" if there is none.
func digestOfRepository(containers []ecstypes.ContainerDefinition, repositoryURI string) string {
	repoName := repositoryURI
	if i := strings.Index(repositoryURI, "/"); i >= 0 {
		repoName = repositoryURI[i+1:]
	}
	for _, container := range containers {
		image, digest, pinned := strings.Cut(aws.ToString(container.Image), "@")
		if pinned && strings.HasPrefix(digest, "sha256:") && strings.HasSuffix(image, "/"+repoName) {
			return digest
		}
	}
	return ""
}

// restoreItem works out one config item of a restore.
func (s *serviceRun) restoreItem(ctx context.Context, source manifest.ConfigItem) {
	run := s.t.restore.run
	item := manifest.ConfigItem{Kind: source.Kind, Name: source.Name, Source: source.Source, Destination: source.Destination}
	name := checkConfigCopy + " " + source.Name
	var content fileFact
	switch {
	case source.NotRestored != "":
		item.NotRestored = fmt.Sprintf("was not restored by run #%d; left in place", run)
	case !source.PreviousExists:
		item.NotRestored = fmt.Sprintf("did not exist before run #%d; left in place", run)
	default:
		var result copyResult
		content, result = s.previousCopy(ctx, source, name)
		switch result {
		case copyExpired:
			item.NotRestored = configCopyExpired
		case copyFound:
			s.storeSource(ctx, &item, content, name)
		}
	}
	s.destination(ctx, &item, content)
	if item.NotRestored != "" {
		item.Change = "unchanged"
	}
	s.svc.Config = append(s.svc.Config, item)
}

type copyResult int

const (
	copyFound   copyResult = iota
	copyExpired            // the copy is gone (retention); a warning check says so
	copyFailed             // the copy could not be read; a failed check says why
)

// previousCopy reads the copy the source run made of the file before it
// replaced it.
func (s *serviceRun) previousCopy(ctx context.Context, source manifest.ConfigItem, name string) (fileFact, copyResult) {
	reader, ok := s.r.x.d.Artifacts.(ArtifactReader)
	if !ok {
		s.add(&s.sources, name, "failed", "the artifact store cannot read stored copies")
		return fileFact{}, copyFailed
	}
	if source.PreviousArtifactKey == "" {
		s.add(&s.sources, name, "failed", fmt.Sprintf("run #%d recorded no stored copy of %s", s.t.restore.run, source.Name))
		return fileFact{}, copyFailed
	}
	var data []byte
	err := s.call(ctx, "artifacts:Get", source.Name+" previous copy", func(ctx context.Context) (callErr error) {
		data, callErr = reader.Get(ctx, source.PreviousArtifactKey, source.PreviousSHA256)
		return callErr
	})
	switch {
	case err == nil:
		return factOf(data), copyFound
	case isMissing(err):
		s.add(&s.sources, name, "not_verified", configCopyExpired)
		return fileFact{}, copyExpired
	case errors.Is(err, artifacts.ErrCorrupt):
		s.add(&s.sources, name, "failed", "the stored copy no longer matches its checksum")
	case errors.Is(err, artifacts.ErrTooLarge):
		s.add(&s.sources, name, "failed", fmt.Sprintf("the stored copy is larger than the %d byte limit", artifacts.MaxObjectBytes))
	default:
		s.add(&s.sources, name, "failed", ErrorDetail("reading the stored copy of "+source.Name, err))
	}
	return fileFact{}, copyFailed
}
