package ecsdeploy

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// ignoredRegisterFields lists RegisterTaskDefinitionInput fields that NextTaskDefinition
// deliberately does not set, with the reason. Empty while the SDK has no such field.
var ignoredRegisterFields = map[string]string{}

const (
	testRepoURI = "123456789012.dkr.ecr.us-east-1.amazonaws.com/app"
	testDigest  = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testSidecar = "public.ecr.aws/example/sidecar:2"
)

var testStamp = Stamp{Version: "1.4.2", ManifestID: "manifest-1", RunID: 7, LegID: 3}

func TestImageRepository(t *testing.T) {
	cases := map[string]string{
		"repo":       "repo",
		"repo:1.4.2": "repo",
		"123.dkr.ecr.us-east-1.amazonaws.com/app:1.4.2":            "123.dkr.ecr.us-east-1.amazonaws.com/app",
		"123.dkr.ecr.us-east-1.amazonaws.com/app@sha256:abc":       "123.dkr.ecr.us-east-1.amazonaws.com/app",
		"123.dkr.ecr.us-east-1.amazonaws.com/app:1.4.2@sha256:abc": "123.dkr.ecr.us-east-1.amazonaws.com/app",
		"localhost:4577/123/us-east-1/app":                         "localhost:4577/123/us-east-1/app",
		"localhost:4577/123/us-east-1/app:1.4.2":                   "localhost:4577/123/us-east-1/app",
	}
	for image, want := range cases {
		if got := ImageRepository(image); got != want {
			t.Errorf("ImageRepository(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestNextTaskDefinitionRewritesOnlyMatchingContainers(t *testing.T) {
	prev := fullPreviousDefinition()

	out, err := NextTaskDefinition(prev, fullPreviousTags(), testRepoURI, testDigest, testStamp)
	if err != nil {
		t.Fatalf("NextTaskDefinition: %v", err)
	}
	if len(out.ContainerDefinitions) != 2 {
		t.Fatalf("got %d containers, want 2", len(out.ContainerDefinitions))
	}
	if got, want := aws.ToString(out.ContainerDefinitions[0].Image), testRepoURI+"@"+testDigest; got != want {
		t.Errorf("app image = %q, want %q", got, want)
	}
	if got := aws.ToString(out.ContainerDefinitions[1].Image); got != testSidecar {
		t.Errorf("sidecar image = %q, want it untouched (%q)", got, testSidecar)
	}
}

func TestNextTaskDefinitionKeepsEverythingElse(t *testing.T) {
	prev := fullPreviousDefinition()

	out, err := NextTaskDefinition(prev, fullPreviousTags(), testRepoURI, testDigest, testStamp)
	if err != nil {
		t.Fatalf("NextTaskDefinition: %v", err)
	}

	outValue := reflect.ValueOf(*out)
	inType := outValue.Type()
	for i := 0; i < inType.NumField(); i++ {
		name := inType.Field(i).Name
		if name == "ContainerDefinitions" || name == "Tags" || inType.Field(i).PkgPath != "" {
			continue
		}
		prevField := reflect.ValueOf(prev).FieldByName(name)
		if !prevField.IsValid() {
			t.Fatalf("TaskDefinition has no field %s to compare with", name)
		}
		if !reflect.DeepEqual(prevField.Interface(), outValue.Field(i).Interface()) {
			t.Errorf("field %s changed: got %v, want %v", name, outValue.Field(i).Interface(), prevField.Interface())
		}
	}

	if len(out.ContainerDefinitions) != len(prev.ContainerDefinitions) {
		t.Fatalf("got %d containers, want %d", len(out.ContainerDefinitions), len(prev.ContainerDefinitions))
	}
	app := out.ContainerDefinitions[0]
	app.Image = prev.ContainerDefinitions[0].Image
	if !reflect.DeepEqual(app, prev.ContainerDefinitions[0]) {
		t.Errorf("app container changed beyond its image: got %+v, want %+v", app, prev.ContainerDefinitions[0])
	}
	if !reflect.DeepEqual(out.ContainerDefinitions[1], prev.ContainerDefinitions[1]) {
		t.Errorf("sidecar container changed: got %+v, want %+v", out.ContainerDefinitions[1], prev.ContainerDefinitions[1])
	}
}

func TestEveryRegisterInputFieldIsCopiedOrDeliberatelyIgnored(t *testing.T) {
	prev := fullPreviousDefinition()

	out, err := NextTaskDefinition(prev, fullPreviousTags(), testRepoURI, testDigest, testStamp)
	if err != nil {
		t.Fatalf("NextTaskDefinition: %v", err)
	}

	outValue := reflect.ValueOf(*out)
	inType := outValue.Type()
	for i := 0; i < inType.NumField(); i++ {
		field := inType.Field(i)
		if field.PkgPath != "" {
			continue
		}
		if _, ignored := ignoredRegisterFields[field.Name]; ignored {
			continue
		}
		if outValue.Field(i).IsZero() {
			t.Errorf("RegisterTaskDefinitionInput.%s is not set by NextTaskDefinition; copy it from the previous definition or add it to ignoredRegisterFields with a reason", field.Name)
		}
	}
}

func TestTagsAreMergedAndStamped(t *testing.T) {
	prevTags := []ecstypes.Tag{
		{Key: aws.String("team"), Value: aws.String("payments")},
		{Key: aws.String(TagRun), Value: aws.String("6")},
		{Key: aws.String(TagLeg), Value: aws.String("1")},
	}

	out, err := NextTaskDefinition(fullPreviousDefinition(), prevTags, testRepoURI, testDigest, testStamp)
	if err != nil {
		t.Fatalf("NextTaskDefinition: %v", err)
	}

	want := map[string]string{
		"team":      "payments",
		TagRun:      "7",
		TagLeg:      "3",
		TagVersion:  "1.4.2",
		TagManifest: "manifest-1",
	}
	if len(out.Tags) != len(want) {
		t.Fatalf("got %d tags, want %d: %v", len(out.Tags), len(want), out.Tags)
	}
	seen := map[string]bool{}
	for _, tag := range out.Tags {
		key := aws.ToString(tag.Key)
		if seen[key] {
			t.Errorf("duplicate tag key %s", key)
		}
		seen[key] = true
		if got := aws.ToString(tag.Value); got != want[key] {
			t.Errorf("tag %s = %q, want %q", key, got, want[key])
		}
	}
	if aws.ToString(out.Tags[0].Key) != "team" || aws.ToString(out.Tags[1].Key) != TagRun || aws.ToString(out.Tags[2].Key) != TagLeg {
		t.Errorf("replaced tags must keep their place, got %v", out.Tags)
	}
	if aws.ToString(out.Tags[3].Key) != TagVersion || aws.ToString(out.Tags[4].Key) != TagManifest {
		t.Errorf("missing tags must be appended in order version, manifest, got %v", out.Tags)
	}
}

func TestNoMatchingContainerIsError(t *testing.T) {
	_, err := NextTaskDefinition(fullPreviousDefinition(), nil, "123456789012.dkr.ecr.us-east-1.amazonaws.com/other", testDigest, testStamp)
	if !errors.Is(err, ErrNoMatchingContainer) {
		t.Fatalf("err = %v, want ErrNoMatchingContainer", err)
	}
	for _, want := range []string{"123456789012.dkr.ecr.us-east-1.amazonaws.com/other", testSidecar, "app:1.4.2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestNextTaskDefinitionDoesNotChangePrevious(t *testing.T) {
	prev := fullPreviousDefinition()
	originalImage := aws.ToString(prev.ContainerDefinitions[0].Image)

	out, err := NextTaskDefinition(prev, nil, testRepoURI, testDigest, testStamp)
	if err != nil {
		t.Fatalf("NextTaskDefinition: %v", err)
	}
	out.ContainerDefinitions[0].Image = aws.String("changed")

	if got := aws.ToString(prev.ContainerDefinitions[0].Image); got != originalImage {
		t.Errorf("previous image changed to %q, want %q", got, originalImage)
	}
}

func TestNextTaskDefinitionRejectsABadDigest(t *testing.T) {
	_, err := NextTaskDefinition(fullPreviousDefinition(), nil, testRepoURI, "abc", testStamp)
	if err == nil {
		t.Fatal("expected an error for a digest that is not sha256")
	}
	if errors.Is(err, ErrNoMatchingContainer) {
		t.Errorf("bad digest must not be reported as a missing container: %v", err)
	}
}

// fullPreviousDefinition sets every optional block of a task definition, so a copy that
// drops any of them fails the tests above.
func fullPreviousDefinition() ecstypes.TaskDefinition {
	return ecstypes.TaskDefinition{
		Family:                  aws.String("app-family"),
		Cpu:                     aws.String("512"),
		Memory:                  aws.String("1024"),
		ExecutionRoleArn:        aws.String("arn:aws:iam::123456789012:role/exec"),
		TaskRoleArn:             aws.String("arn:aws:iam::123456789012:role/task"),
		NetworkMode:             ecstypes.NetworkMode("awsvpc"),
		IpcMode:                 ecstypes.IpcMode("host"),
		PidMode:                 ecstypes.PidMode("host"),
		EnableFaultInjection:    aws.Bool(true),
		EphemeralStorage:        &ecstypes.EphemeralStorage{SizeInGiB: 30},
		RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.Compatibility("FARGATE")},
		Compatibilities:         []ecstypes.Compatibility{ecstypes.Compatibility("FARGATE")},
		RuntimePlatform: &ecstypes.RuntimePlatform{
			CpuArchitecture:       ecstypes.CPUArchitecture("X86_64"),
			OperatingSystemFamily: ecstypes.OSFamily("LINUX"),
		},
		ProxyConfiguration: &ecstypes.ProxyConfiguration{
			Type:          ecstypes.ProxyConfigurationType("APPMESH"),
			ContainerName: aws.String("envoy"),
			Properties:    []ecstypes.KeyValuePair{{Name: aws.String("IgnoredUID"), Value: aws.String("1337")}},
		},
		InferenceAccelerators: []ecstypes.InferenceAccelerator{{DeviceName: aws.String("dev"), DeviceType: aws.String("eia2.medium")}}, //nolint:staticcheck // deprecated field, set so the copy is checked
		PlacementConstraints: []ecstypes.TaskDefinitionPlacementConstraint{
			{Type: ecstypes.TaskDefinitionPlacementConstraintType("memberOf"), Expression: aws.String("attribute:zone == a")},
		},
		TrustedExecutionConfiguration: &ecstypes.TrustedExecutionConfiguration{
			IsolationMode:     ecstypes.IsolationMode("NITRO"),
			AttestationPolicy: aws.String("policy"),
		},
		Volumes: []ecstypes.Volume{{Name: aws.String("data"), Host: &ecstypes.HostVolumeProperties{SourcePath: aws.String("/data")}}},
		ContainerDefinitions: []ecstypes.ContainerDefinition{
			{
				Name:        aws.String("app"),
				Image:       aws.String(testRepoURI + ":1.4.2"),
				Essential:   aws.Bool(true),
				Command:     []string{"serve"},
				Environment: []ecstypes.KeyValuePair{{Name: aws.String("MODE"), Value: aws.String("prod")}},
				Secrets:     []ecstypes.Secret{{Name: aws.String("DB_PASSWORD"), ValueFrom: aws.String("arn:aws:secretsmanager:us-east-1:123456789012:secret:db")}},
				HealthCheck: &ecstypes.HealthCheck{Command: []string{"CMD", "true"}, Interval: aws.Int32(30), Retries: aws.Int32(3), Timeout: aws.Int32(5), StartPeriod: aws.Int32(10)},
				LogConfiguration: &ecstypes.LogConfiguration{
					LogDriver: ecstypes.LogDriver("awslogs"),
					Options:   map[string]string{"awslogs-group": "/app"},
				},
			},
			{
				Name:      aws.String("sidecar"),
				Image:     aws.String(testSidecar),
				Essential: aws.Bool(false),
			},
		},
	}
}

func fullPreviousTags() []ecstypes.Tag {
	return []ecstypes.Tag{{Key: aws.String("owner"), Value: aws.String("platform")}}
}
