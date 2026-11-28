//go:build integration && floci

package running

import (
	"context"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/test/awstest"
)

// flociDiscovery hands out each account's own configuration, as the discovery
// provider does for connected accounts.
type flociDiscovery map[string]*awstest.Account

func (d flociDiscovery) For(_ context.Context, account, region, _ string) (aws.Config, error) {
	return d[account].AWSConfig(region), nil
}

func TestMatrixAgainstFloci(t *testing.T) {
	const region = "us-east-1"
	w := awstest.New(t)
	suffix := awstest.RandomSuffix()
	euID, usID := awstest.RandomAccountID(), awstest.RandomAccountID()
	eu, us := w.Account(euID), w.Account(usID)
	f := newDBFixture(t)

	// eu: one service started outside Yumdrake (no tags) and one that points at
	// its image by digest. us: one deployed by a Yumdrake run, with its tags.
	euRepo, _ := eu.Repo(region, "api-"+suffix, "1.4.1", "1.4.2")
	usRepo, _ := us.Repo(region, "api-"+suffix, "1.4.1", "1.4.2")
	euWebRepo, webDigest := eu.Repo(region, "web-"+suffix, "2.0.0")
	api, web := "api-"+suffix, "web-"+suffix
	eu.Service(region, "main", api, euRepo+":1.4.2")
	eu.Service(region, "main", web, euWebRepo+":2.0.0")
	eu.Redeploy(region, "main", web, euWebRepo+"@"+webDigest, nil)
	us.Service(region, "main", api, usRepo+":1.4.2")

	runID, deployStep, _ := f.run("us")
	legID := f.leg(runID, &deployStep, "step", api, "succeeded")
	us.Redeploy(region, "main", api, usRepo+":1.4.1", map[string]string{
		"yumdrake:version": "1.4.1",
		"yumdrake:run":     strconv.FormatInt(runID, 10),
		"yumdrake:leg":     strconv.FormatInt(legID, 10),
	})

	envEUF := spec.Environment{Name: "eu", Label: "prod", AWSAccount: euID}
	envUSF := spec.Environment{Name: "us", Label: "prod", AWSAccount: usID}
	apiSpec := spec.Service{Name: api, Versions: spec.VersionRule{Pattern: `^\d+\.\d+\.\d+$`, Sort: "semver", Limit: 50},
		Environments: map[string]spec.ServiceEnv{
			"eu": {ECSService: serviceARNIn(euID, region, "main", api), ECRRepository: euRepo},
			"us": {ECSService: serviceARNIn(usID, region, "main", api), ECRRepository: usRepo},
		}}
	webSpec := spec.Service{Name: web, Versions: spec.VersionRule{Pattern: `^\d+\.\d+\.\d+$`, Sort: "semver", Limit: 50},
		Environments: map[string]spec.ServiceEnv{"eu": {ECSService: serviceARNIn(euID, region, "main", web), ECRRepository: euWebRepo}}}
	inv := inventoryOf([]spec.Environment{envEUF, envUSF}, apiSpec, webSpec)

	svc := New(Config{Discovery: flociDiscovery{euID: eu, usID: us}, AWS: awsx.Options{EndpointURL: w.Endpoint}, DB: f.pool})
	m, err := svc.Matrix(context.Background(), inv)
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}

	euAPI, usAPI := cellOf(t, m, api, "eu"), cellOf(t, m, api, "us")
	if euAPI.State != "ok" || euAPI.Version != "1.4.2" || euAPI.ByYumdrake || euAPI.RunID != 0 || euAPI.TaskDefinition != api+":1" {
		t.Errorf("eu api = %+v, want ok 1.4.2 from the image tag, not by Yumdrake, %s:1", euAPI, api)
	}
	if usAPI.State != "ok" || usAPI.Version != "1.4.1" || !usAPI.ByYumdrake || usAPI.RunID != runID || usAPI.TaskDefinition != api+":2" {
		t.Errorf("us api = %+v, want ok 1.4.1 from the tag, by Yumdrake run %d, %s:2", usAPI, runID, api)
	}
	if !usAPI.Behind || euAPI.Behind {
		t.Errorf("behind: us %v eu %v, want us behind eu within the label prod", usAPI.Behind, euAPI.Behind)
	}
	// Floci's DescribeImages by digest answers with no tags (AWS lists them), so
	// the digest-to-tag step is covered by TestVersionFromTagThenDigestThenImage
	// and here the cell only has to read cleanly, without a version.
	if c := cellOf(t, m, web, "eu"); c.State != "ok" || c.Version != "" || c.ByYumdrake || c.TaskDefinition != web+":2" {
		t.Errorf("eu web = %+v, want ok, no version (a digest reference), %s:2", c, web)
	}
	if c := cellOf(t, m, web, "us"); c.State != "absent" {
		t.Errorf("us web = %+v, want absent", c)
	}
}

func serviceARNIn(account, rgn, cluster, name string) string {
	return "arn:aws:ecs:" + rgn + ":" + account + ":service/" + cluster + "/" + name
}
