//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	sdkauth "github.com/scttfrdmn/globus-go-sdk/v3/pkg/services/auth"
	"github.com/scttfrdmn/globus-go-sdk/v3/pkg/services/transfer"
)

// Well-known IDs used across all integration tests.
const (
	collection1000Genomes = "28daf55e-07a4-4a65-9f54-292b0b4b8739"
	destEndpoint          = "31ce9ba0-176d-45a5-add3-f37d233ba47d" // Globus Tutorial Endpoint 2
	testSourceFile        = "/1000genomes/release/20130502/20140625_related_individuals.txt"
	testDestFile          = "/~/elastic-gcs-inttest.txt"
)

// testEnv holds shared AWS and Globus clients for integration tests.
type testEnv struct {
	region          string
	asgName         string
	secretsPrefix   string
	controlNodeParam string

	cfg       aws.Config
	asgClient *autoscaling.Client
	ssmClient *ssm.Client

	transferClient *transfer.Client
}

// staticBearerToken satisfies the Globus transfer.Authorizer interface.
type staticBearerToken struct{ token string }

func (a *staticBearerToken) GetAuthorizationHeader(_ ...context.Context) (string, error) {
	return "Bearer " + a.token, nil
}

// newTestEnv initialises shared clients from environment variables.
// Required env vars: ELASTIC_GCS_REGION, GLOBUS_CLIENT_ID, GLOBUS_CLIENT_SECRET.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	region := os.Getenv("ELASTIC_GCS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	clientID := requireEnv(t, "GLOBUS_CLIENT_ID")
	clientSecret := requireEnv(t, "GLOBUS_CLIENT_SECRET")

	asgName := getEnvOrDefault("ELASTIC_GCS_ASG_NAME", fmt.Sprintf("elastic-gcs-dtn-%s", region))
	secretsPrefix := getEnvOrDefault("ELASTIC_GCS_SECRETS_PREFIX", fmt.Sprintf("elastic-gcs/%s", region))
	controlNodeParam := fmt.Sprintf("/elastic-gcs/%s/control-node-id", region)

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region))
	if err != nil {
		t.Fatalf("AWS config: %v", err)
	}

	authClient, err := sdkauth.NewClient(
		sdkauth.WithClientID(clientID),
		sdkauth.WithClientSecret(clientSecret),
	)
	if err != nil {
		t.Fatalf("Globus auth client: %v", err)
	}
	tok, err := authClient.GetClientCredentialsToken(context.Background(),
		"urn:globus:auth:scope:transfer.api.globus.org:all")
	if err != nil {
		t.Fatalf("GetClientCredentialsToken: %v", err)
	}
	tc, err := transfer.NewClient(transfer.WithAuthorizer(&staticBearerToken{token: tok.AccessToken}))
	if err != nil {
		t.Fatalf("transfer client: %v", err)
	}

	return &testEnv{
		region:           region,
		asgName:          asgName,
		secretsPrefix:    secretsPrefix,
		controlNodeParam: controlNodeParam,
		cfg:              cfg,
		asgClient:        autoscaling.NewFromConfig(cfg),
		ssmClient:        ssm.NewFromConfig(cfg),
		transferClient:   tc,
	}
}

// requireEnv returns the value of an env var or calls t.Skip if unset.
func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("skipping: %s not set", key)
	}
	return v
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// controlNodeID reads the control node EC2 instance ID from SSM.
func (e *testEnv) controlNodeID(ctx context.Context, t *testing.T) string {
	t.Helper()
	out, err := e.ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(e.controlNodeParam),
	})
	if err != nil {
		t.Fatalf("GetParameter %s: %v", e.controlNodeParam, err)
	}
	return aws.ToString(out.Parameter.Value)
}

// scaleASG sets the ASG desired capacity.
func (e *testEnv) scaleASG(ctx context.Context, t *testing.T, desired int32) {
	t.Helper()
	_, err := e.asgClient.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: aws.String(e.asgName),
		DesiredCapacity:      aws.Int32(desired),
		HonorCooldown:        aws.Bool(false),
	})
	if err != nil {
		t.Fatalf("SetDesiredCapacity(%d): %v", desired, err)
	}
}

// waitForASGInService polls until >= minCount InService instances exist, or deadline passes.
// Returns the InService instance IDs.
func (e *testEnv) waitForASGInService(ctx context.Context, t *testing.T, minCount int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		out, err := e.asgClient.DescribeAutoScalingGroups(ctx,
			&autoscaling.DescribeAutoScalingGroupsInput{
				AutoScalingGroupNames: []string{e.asgName},
			})
		if err != nil || len(out.AutoScalingGroups) == 0 {
			continue
		}
		var ids []string
		for _, inst := range out.AutoScalingGroups[0].Instances {
			if inst.LifecycleState == astypes.LifecycleStateInService {
				ids = append(ids, aws.ToString(inst.InstanceId))
			}
		}
		if len(ids) >= minCount {
			return ids
		}
	}
	t.Fatalf("timed out waiting for %d InService instances in %s after %s", minCount, e.asgName, timeout)
	return nil
}

// waitForWarmPool polls until >= minCount Warmed:Stopped instances exist in the warm pool.
// Returns the instance IDs.
func (e *testEnv) waitForWarmPool(ctx context.Context, t *testing.T, minCount int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		wp, err := e.asgClient.DescribeWarmPool(ctx,
			&autoscaling.DescribeWarmPoolInput{AutoScalingGroupName: aws.String(e.asgName)})
		if err != nil {
			continue
		}
		var ids []string
		for _, inst := range wp.Instances {
			if inst.LifecycleState == astypes.LifecycleStateWarmedStopped {
				ids = append(ids, aws.ToString(inst.InstanceId))
			}
		}
		if len(ids) >= minCount {
			return ids
		}
	}
	t.Fatalf("timed out waiting for %d Warmed:Stopped instances after %s", minCount, timeout)
	return nil
}

// inServiceIDs returns the EC2 instance IDs currently InService in the ASG.
func (e *testEnv) inServiceIDs(ctx context.Context) []string {
	out, err := e.asgClient.DescribeAutoScalingGroups(ctx,
		&autoscaling.DescribeAutoScalingGroupsInput{
			AutoScalingGroupNames: []string{e.asgName},
		})
	if err != nil || len(out.AutoScalingGroups) == 0 {
		return nil
	}
	var ids []string
	for _, inst := range out.AutoScalingGroups[0].Instances {
		if inst.LifecycleState == astypes.LifecycleStateInService {
			ids = append(ids, aws.ToString(inst.InstanceId))
		}
	}
	return ids
}

// warmPoolCount returns the current number of Warmed:Stopped instances.
func (e *testEnv) warmPoolCount(ctx context.Context) int {
	wp, err := e.asgClient.DescribeWarmPool(ctx,
		&autoscaling.DescribeWarmPoolInput{AutoScalingGroupName: aws.String(e.asgName)})
	if err != nil {
		return 0
	}
	n := 0
	for _, inst := range wp.Instances {
		if inst.LifecycleState == astypes.LifecycleStateWarmedStopped {
			n++
		}
	}
	return n
}

// runSSMCommand executes a shell command on an EC2 instance via SSM RunCommand
// and returns stdout. Fails the test on error or non-zero exit.
func (e *testEnv) runSSMCommand(ctx context.Context, t *testing.T, instanceID, cmd string) string {
	t.Helper()
	out, err := e.ssmClient.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:  []string{instanceID},
		DocumentName: aws.String("AWS-RunShellScript"),
		Parameters: map[string][]string{
			"commands":         {cmd},
			"executionTimeout": {"60"},
		},
	})
	if err != nil {
		t.Fatalf("SSM SendCommand on %s: %v", instanceID, err)
	}
	cmdID := aws.ToString(out.Command.CommandId)

	// Poll for result.
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		inv, err := e.ssmClient.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(cmdID),
			InstanceId: aws.String(instanceID),
		})
		if err != nil {
			continue
		}
		switch inv.Status {
		case ssmtypes.CommandInvocationStatusSuccess:
			return strings.TrimSpace(aws.ToString(inv.StandardOutputContent))
		case ssmtypes.CommandInvocationStatusFailed,
			ssmtypes.CommandInvocationStatusTimedOut,
			ssmtypes.CommandInvocationStatusCancelled:
			t.Fatalf("SSM command %q on %s: status=%s stderr=%s",
				cmd, instanceID, inv.Status,
				aws.ToString(inv.StandardErrorContent))
		}
	}
	t.Fatalf("timed out waiting for SSM command %q on %s", cmd, instanceID)
	return ""
}

// gcsNodeStatus holds the fields we care about from `node list -F json`.
type gcsNodeStatus struct {
	ID          string   `json:"id"`
	Status      string   `json:"status"` // "active", or maintenance message
	IPAddresses []string `json:"ip_addresses"`
}

// listGCSNodes runs `globus-connect-server node list -F json` on the control node
// and returns the parsed node list.
func (e *testEnv) listGCSNodes(ctx context.Context, t *testing.T) []gcsNodeStatus {
	t.Helper()
	controlID := e.controlNodeID(ctx, t)
	raw := e.runSSMCommand(ctx, t, controlID,
		"sudo globus-connect-server node list -F json 2>/dev/null || echo '[]'")
	var nodes []gcsNodeStatus
	if err := json.Unmarshal([]byte(raw), &nodes); err != nil {
		t.Fatalf("parse node list JSON: %v\nraw: %s", err, raw)
	}
	return nodes
}

// hasActiveGCSNode returns true if any node in the list has status "active".
func hasActiveGCSNode(nodes []gcsNodeStatus) bool {
	for _, n := range nodes {
		if strings.EqualFold(n.Status, "active") {
			return true
		}
	}
	return false
}

// submitTestTransfer submits a small file transfer from the 1000 Genomes collection
// to Globus Tutorial Endpoint 2 and returns the task ID.
func (e *testEnv) submitTestTransfer(ctx context.Context, t *testing.T) string {
	t.Helper()
	srcCollection := getEnvOrDefault("ELASTIC_GCS_COLLECTION_ID", collection1000Genomes)
	dstEndpoint := getEnvOrDefault("ELASTIC_GCS_DEST_ENDPOINT", destEndpoint)

	resp, err := e.transferClient.SubmitTransfer(ctx,
		srcCollection, testSourceFile,
		dstEndpoint, testDestFile,
		fmt.Sprintf("elastic-gcs integration test %s", time.Now().Format("20060102-150405")),
		nil,
	)
	if err != nil {
		t.Fatalf("SubmitTransfer: %v", err)
	}
	t.Logf("submitted transfer task %s", resp.TaskID)
	return resp.TaskID
}

// waitForTransferSucceeded polls until the task reaches SUCCEEDED (or fails/cancels).
// Returns the completed task.
func (e *testEnv) waitForTransferSucceeded(ctx context.Context, t *testing.T, taskID string, timeout time.Duration) *transfer.Task {
	t.Helper()
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	task, err := e.transferClient.WaitForTaskCompletion(ctx2, taskID, 10*time.Second)
	if err != nil {
		t.Fatalf("WaitForTaskCompletion %s: %v", taskID, err)
	}
	if task.Status != "SUCCEEDED" {
		t.Fatalf("transfer %s: expected SUCCEEDED, got %s", taskID, task.Status)
	}
	return task
}
