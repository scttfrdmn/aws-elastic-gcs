//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
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
	region           string
	asgName          string
	secretsPrefix    string
	controlNodeParam string
	endpointIDParam  string

	// GCS CLI credentials (same as Globus client creds) used when running
	// globus-connect-server commands on the control node via SSM. These are
	// passed as GCS_CLI_CLIENT_ID/SECRET/ENDPOINT_ID env vars.
	gcsCLIClientID     string
	gcsCLIClientSecret string
	gcsEndpointID      string

	cfg            aws.Config
	asgClient      *autoscaling.Client
	ssmClient      *ssm.Client
	cwClient       *cloudwatch.Client
	scaleOutAlarm  string // CloudWatch alarm name driving scale-out
	scaleInAlarm   string // CloudWatch alarm name driving scale-in

	// transferClient uses client credentials for task monitoring (GetTask).
	// Transfer submission uses the `globus` CLI (authenticated as the endpoint owner).
	transferClient *transfer.Client
}

// staticBearerToken satisfies the Globus transfer.Authorizer interface.
type staticBearerToken struct{ token string }

func (a *staticBearerToken) GetAuthorizationHeader(_ ...context.Context) (string, error) {
	return "Bearer " + a.token, nil
}

// newTestEnv initialises shared clients from environment variables.
// Required env vars: GLOBUS_CLIENT_ID, GLOBUS_CLIENT_SECRET.
// Optional: ELASTIC_GCS_REGION (default us-east-1), ELASTIC_GCS_ASG_NAME.
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
	endpointIDParam := fmt.Sprintf("/elastic-gcs/%s/endpoint-id", region)

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region))
	if err != nil {
		t.Fatalf("AWS config: %v", err)
	}

	// Resolve the Globus endpoint ID from SSM for use in GCS CLI auth.
	ssmClient := ssm.NewFromConfig(cfg)
	gcsEndpointID := os.Getenv("ELASTIC_GCS_ENDPOINT_ID")
	if gcsEndpointID == "" {
		epParam, err := ssmClient.GetParameter(context.Background(),
			&ssm.GetParameterInput{Name: aws.String(endpointIDParam)})
		if err != nil {
			t.Fatalf("GetParameter %s: %v", endpointIDParam, err)
		}
		gcsEndpointID = aws.ToString(epParam.Parameter.Value)
	}

	// Build a transfer client using client credentials for task monitoring.
	// Transfer submission goes through the `globus` CLI (user-authenticated).
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
		region:             region,
		asgName:            asgName,
		secretsPrefix:      secretsPrefix,
		controlNodeParam:   controlNodeParam,
		endpointIDParam:    endpointIDParam,
		gcsCLIClientID:     clientID,
		gcsCLIClientSecret: clientSecret,
		gcsEndpointID:      gcsEndpointID,
		cfg:           cfg,
		asgClient:     autoscaling.NewFromConfig(cfg),
		ssmClient:     ssmClient,
		cwClient:      cloudwatch.NewFromConfig(cfg),
		scaleOutAlarm:  fmt.Sprintf("elastic-gcs-scaleout-%s", region),
		scaleInAlarm:   fmt.Sprintf("elastic-gcs-scalein-%s", region),
		transferClient: tc,
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
	Status      string   `json:"status"` // "active", or inactive/maintenance message
	IPAddresses []string `json:"ip_addresses"`
}

// gcsNodeListResult is the outer result#1.x.x wrapper returned by the GCS CLI.
type gcsNodeListResult struct {
	Code string          `json:"code"`
	Data []gcsNodeStatus `json:"data"`
}

// listGCSNodes runs `globus-connect-server node list -F json` on the control node
// and returns the parsed node list. The GCS CLI requires client credentials
// (GCS_CLI_CLIENT_ID/SECRET/ENDPOINT_ID), which are injected as env vars.
//
// Output format is a JSON array of result#1.x.x wrapper objects; nodes are at [0].data.
func (e *testEnv) listGCSNodes(ctx context.Context, t *testing.T) []gcsNodeStatus {
	t.Helper()
	controlID := e.controlNodeID(ctx, t)

	// Build the SSM command with GCS CLI auth env vars.
	cmd := fmt.Sprintf(
		"export GCS_CLI_CLIENT_ID=%s; export GCS_CLI_CLIENT_SECRET=%s; export GCS_CLI_ENDPOINT_ID=%s; "+
			"sudo -E globus-connect-server node list -F json 2>&1",
		e.gcsCLIClientID, e.gcsCLIClientSecret, e.gcsEndpointID,
	)
	raw := e.runSSMCommand(ctx, t, controlID, cmd)

	// Parse the result#1.x.x wrapper array: [ { "code": "success", "data": [...] } ]
	var results []gcsNodeListResult
	if err := json.Unmarshal([]byte(raw), &results); err != nil {
		t.Fatalf("parse node list JSON: %v\nraw output: %s", err, raw)
	}
	if len(results) == 0 || results[0].Code != "success" {
		t.Fatalf("node list returned unexpected result: %s", raw)
	}
	return results[0].Data
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

// submitTestTransfer submits a file transfer via the `globus` CLI (authenticated
// as the endpoint owner) and returns the Globus task ID.
//
// We use the CLI instead of the SDK client credentials flow because the automation
// client doesn't have user-level transfer permission on the mapped collection.
// The `globus` CLI is authenticated as scofri@globusid.org which owns the endpoint.
func (e *testEnv) submitTestTransfer(ctx context.Context, t *testing.T) string {
	t.Helper()
	srcCollection := getEnvOrDefault("ELASTIC_GCS_COLLECTION_ID", collection1000Genomes)
	dstEndpointID := getEnvOrDefault("ELASTIC_GCS_DEST_ENDPOINT", destEndpoint)

	// Resolve the globus CLI path — it may be in ~/.local/bin which isn't always
	// in PATH when tests are invoked via `make`.
	globusBin, err := exec.LookPath("globus")
	if err != nil {
		// Fallback to the known install location.
		globusBin = os.ExpandEnv("$HOME/.local/bin/globus")
	}

	// Use -F json so we can parse task_id reliably. Disable email notifications
	// to avoid flooding the operator's inbox during test runs.
	out, err := exec.CommandContext(ctx,
		globusBin, "transfer",
		"-F", "json",
		"--notify", "off",
		"--label", fmt.Sprintf("elastic-gcs-inttest-%s", time.Now().Format("20060102-150405")),
		srcCollection+":"+testSourceFile,
		dstEndpointID+":"+testDestFile,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("globus transfer failed (exit %v):\n%s", err, out)
	}

	var result struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse globus transfer output: %v\nraw: %s", err, out)
	}
	if result.TaskID == "" {
		t.Fatalf("globus transfer returned empty task_id:\n%s", out)
	}
	t.Logf("submitted transfer task %s", result.TaskID)
	return result.TaskID
}

// globusTaskInfo holds the fields we need from `globus task show -F json`.
type globusTaskInfo struct {
	TaskID           string `json:"task_id"`
	Status           string `json:"status"` // ACTIVE, INACTIVE, SUCCEEDED, FAILED, CANCELLED
	BytesTransferred int64  `json:"bytes_transferred"`
}

// getTaskInfo fetches current task status via the `globus` CLI.
// The task was submitted by the endpoint owner's session, so the SDK client
// credentials can't see it; we use the CLI which shares the same session.
func (e *testEnv) getTaskInfo(ctx context.Context, t *testing.T, taskID string) *globusTaskInfo {
	t.Helper()
	globusBin, _ := exec.LookPath("globus")
	if globusBin == "" {
		globusBin = os.ExpandEnv("$HOME/.local/bin/globus")
	}
	out, err := exec.CommandContext(ctx, globusBin, "task", "show", "-F", "json", taskID).CombinedOutput()
	if err != nil {
		t.Fatalf("globus task show %s: %v\n%s", taskID, err, out)
	}
	var info globusTaskInfo
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("parse task show output: %v\nraw: %s", err, out)
	}
	return &info
}

// submitTestTransferWithDest submits a transfer with a custom destination path.
// Use this when submitting multiple transfers rapidly to avoid collisions.
// destPath example: "/~/elastic-gcs-autoscale-1234567890-0.txt"
func (e *testEnv) submitTestTransferWithDest(ctx context.Context, t *testing.T, destPath string) string {
	t.Helper()
	srcCollection := getEnvOrDefault("ELASTIC_GCS_COLLECTION_ID", collection1000Genomes)
	dstEndpointID := getEnvOrDefault("ELASTIC_GCS_DEST_ENDPOINT", destEndpoint)

	globusBin, err := exec.LookPath("globus")
	if err != nil {
		globusBin = os.ExpandEnv("$HOME/.local/bin/globus")
	}

	out, err := exec.CommandContext(ctx,
		globusBin, "transfer",
		"-F", "json",
		"--notify", "off",
		"--label", fmt.Sprintf("elastic-gcs-autoscale-%s", time.Now().Format("20060102-150405")),
		srcCollection+":"+testSourceFile,
		dstEndpointID+":"+destPath,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("globus transfer failed (exit %v):\n%s", err, out)
	}

	var result struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parse globus transfer output: %v\nraw: %s", err, out)
	}
	if result.TaskID == "" {
		t.Fatalf("globus transfer returned empty task_id:\n%s", out)
	}
	t.Logf("submitted transfer task %s (dest: %s)", result.TaskID, destPath)
	return result.TaskID
}

// waitForASGDesiredChange polls DescribeAutoScalingGroups until DesiredCapacity
// differs from fromDesired. Returns the new desired capacity.
// Used to detect that the scaleout Lambda fired without any manual ASG call.
func (e *testEnv) waitForASGDesiredChange(ctx context.Context, t *testing.T,
	fromDesired int32, timeout time.Duration) int32 {
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
		desired := aws.ToInt32(out.AutoScalingGroups[0].DesiredCapacity)
		if desired != fromDesired {
			return desired
		}
	}
	t.Fatalf("timed out waiting for ASG %s desired to change from %d after %s",
		e.asgName, fromDesired, timeout)
	return fromDesired // unreachable
}

// trySubmitTransferWithDest attempts a transfer submission and returns
// (taskID, nil) on success or ("", err) on failure without calling t.Fatal.
// Used by keepAliveSubmit to tolerate transient endpoint failures (e.g. during
// DNS propagation after scale-in when GCS DNS may still point to a stopped DTN).
func (e *testEnv) trySubmitTransferWithDest(ctx context.Context, destPath string) (string, error) {
	srcCollection := getEnvOrDefault("ELASTIC_GCS_COLLECTION_ID", collection1000Genomes)
	dstEndpointID := getEnvOrDefault("ELASTIC_GCS_DEST_ENDPOINT", destEndpoint)

	globusBin, err := exec.LookPath("globus")
	if err != nil {
		globusBin = os.ExpandEnv("$HOME/.local/bin/globus")
	}

	out, err := exec.CommandContext(ctx,
		globusBin, "transfer",
		"-F", "json",
		"--notify", "off",
		"--label", fmt.Sprintf("elastic-gcs-autoscale-%s", time.Now().Format("20060102-150405")),
		srcCollection+":"+testSourceFile,
		dstEndpointID+":"+destPath,
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("globus transfer: %w: %s", err, out)
	}

	var result struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", fmt.Errorf("parse transfer output: %w", err)
	}
	if result.TaskID == "" {
		return "", fmt.Errorf("empty task_id in: %s", out)
	}
	return result.TaskID, nil
}

// triggerScaleOut submits a single demand transfer and immediately forces the
// scale-out alarm to fire. Returns the submitted task ID.
//
// Why SetAlarmState instead of PutMetricData:
//   - Test transfers (1168 bytes) complete in ~3s; demand_monitor's 60s poll
//     window almost never catches them as ACTIVE.
//   - PutMetricData with standard resolution has up to 15-minute appearance lag
//     in CloudWatch alarm evaluations — unreliable for any reasonable test timeout.
//   - SetAlarmState(ALARM) is immediate: it triggers SNS→scaleout Lambda→ASG
//     directly. This is the production chain that matters; correctness of the
//     full path is confirmed by the step-4 transfer that MUST succeed on the DTN.
//
// The alarm is first reset to OK so the OK→ALARM transition always fires,
// regardless of state left by a prior test run.
func (e *testEnv) triggerScaleOut(ctx context.Context, t *testing.T) []string {
	t.Helper()

	// Reset alarm to OK to guarantee a state transition when we set it to ALARM.
	// CloudWatch alarms only fire SNS actions on transitions, not while already
	// in ALARM.
	if _, err := e.cwClient.SetAlarmState(ctx, &cloudwatch.SetAlarmStateInput{
		AlarmName:   aws.String(e.scaleOutAlarm),
		StateValue:  cwtypes.StateValueOk,
		StateReason: aws.String("reset by integration test before triggerScaleOut"),
	}); err != nil {
		t.Logf("triggerScaleOut: SetAlarmState(OK) warning: %v", err)
	} else {
		t.Logf("triggerScaleOut: scale-out alarm reset to OK")
	}

	// Submit one demand transfer. Retry for up to 3 minutes to cover GCS DNS
	// propagation after a prior scale-in (Globus docs: "up to 2 minutes").
	// The prior test's lifecycle hook calls `node disable`, but DNS may still
	// route new connections to the just-stopped DTN for ~2 minutes after that.
	destPath := fmt.Sprintf("/~/elastic-gcs-demand-%d.txt", time.Now().Unix())
	var taskID string
	for attempt := 1; attempt <= 12; attempt++ {
		var err error
		taskID, err = e.trySubmitTransferWithDest(ctx, destPath)
		if err == nil {
			break
		}
		t.Logf("triggerScaleOut: submission attempt %d failed: %v", attempt, err)
		if attempt < 12 {
			time.Sleep(15 * time.Second)
		}
	}
	if taskID == "" {
		t.Fatal("triggerScaleOut: all submission attempts failed")
	}
	t.Logf("triggerScaleOut: submitted demand transfer %s", taskID)

	// Now fire the alarm. This is immediate — SNS notifies the scaleout Lambda
	// which increments ASG DesiredCapacity without any manual call.
	if _, err := e.cwClient.SetAlarmState(ctx, &cloudwatch.SetAlarmStateInput{
		AlarmName:   aws.String(e.scaleOutAlarm),
		StateValue:  cwtypes.StateValueAlarm,
		StateReason: aws.String("integration test: active transfer demand detected"),
	}); err != nil {
		t.Logf("triggerScaleOut: SetAlarmState(ALARM) warning: %v", err)
	} else {
		t.Logf("triggerScaleOut: scale-out alarm set to ALARM (SNS→Lambda→ASG triggered)")
	}

	return []string{taskID}
}

// waitForScalingActivityComplete polls DescribeScalingActivities until all
// recent ASG activities are complete (Progress=100). This is needed before
// triggering scale-in: the ASG rejects SetDesiredCapacity with
// ScalingActivityInProgress while the scale-out lifecycle hook is still
// running (health check grace period, typically ~5 min after InService).
func (e *testEnv) waitForScalingActivityComplete(ctx context.Context, t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(15 * time.Second)
		out, err := e.asgClient.DescribeScalingActivities(ctx,
			&autoscaling.DescribeScalingActivitiesInput{
				AutoScalingGroupName: aws.String(e.asgName),
				MaxRecords:           aws.Int32(5),
			})
		if err != nil {
			continue
		}
		allDone := true
		for _, act := range out.Activities {
			if aws.ToInt32(act.Progress) < 100 {
				allDone = false
				t.Logf("waitForScalingActivityComplete: activity %s progress=%d%%",
					aws.ToString(act.ActivityId)[:8], aws.ToInt32(act.Progress))
				break
			}
		}
		if allDone {
			t.Log("waitForScalingActivityComplete: all scaling activities complete")
			return
		}
	}
	t.Fatalf("waitForScalingActivityComplete: timed out after %s", timeout)
}

// waitForCleanScaleInMetric polls CloudWatch GetMetricStatistics until
// ActiveTransfers has been 0 (max) for the last 5 minutes. This mirrors the
// active transfer guard in the scale-in Lambda: if this passes, the Lambda
// will not abort the scale-in.
//
// Why this is needed: the Globus endpoint_manager/task_list API has a ~1-2
// minute lag before removing SUCCEEDED tasks from ACTIVE. demand_monitor may
// therefore publish non-zero ActiveTransfers for up to ~2 minutes after the
// Globus CLI confirms tasks are terminal. The Lambda's 5-minute lookback then
// blocks scale-in for up to ~7 minutes after transfer completion.
//
// We wait for this window to clear before calling triggerScaleIn so the Lambda
// proceeds rather than silently skipping.
func (e *testEnv) waitForCleanScaleInMetric(ctx context.Context, t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(30 * time.Second)
		stats, err := e.cwClient.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
			Namespace:  aws.String("ElasticGCS"),
			MetricName: aws.String("ActiveTransfers"),
			StartTime:  aws.Time(time.Now().Add(-5 * time.Minute)),
			EndTime:    aws.Time(time.Now()),
			Period:     aws.Int32(300),
			Statistics: []cwtypes.Statistic{cwtypes.StatisticMaximum},
			Dimensions: []cwtypes.Dimension{
				{Name: aws.String("Region"), Value: aws.String(e.region)},
			},
		})
		if err != nil {
			t.Logf("waitForCleanScaleInMetric: GetMetricStatistics warning: %v", err)
			continue
		}
		// No datapoints = missing data, treated as 0 by the Lambda guard.
		// Max = 0 = clean window.
		if len(stats.Datapoints) == 0 || aws.ToFloat64(stats.Datapoints[0].Maximum) == 0 {
			t.Logf("waitForCleanScaleInMetric: ActiveTransfers clear — Lambda guard will pass")
			return
		}
		t.Logf("waitForCleanScaleInMetric: max=%.0f in last 5min, waiting...",
			aws.ToFloat64(stats.Datapoints[0].Maximum))
	}
	t.Fatalf("waitForCleanScaleInMetric: timed out after %s", timeout)
}

// triggerScaleIn immediately forces the scale-in alarm to ALARM state,
// driving the scale-in Lambda without waiting 15 minutes for demand_monitor
// to organically produce three consecutive zero-demand periods.
//
// Why not wait for the organic path:
//   - demand_monitor uses endpoint_manager/task_list which has a ~1-2 minute
//     lag before marking SUCCEEDED tasks as terminal. This resets CloudWatch's
//     3×5-minute evaluation window every test run.
//   - Test transfers (1168 bytes, ~13s) complete far too quickly for organic
//     scale-in to be reliable in any reasonable test timeout.
//   - The production scale-in path works correctly for real transfers (minutes
//     to hours), but is not suitable for automated testing with tiny files.
//
// What we do test:
//   - The scale-in Lambda fires on OK→ALARM transition
//   - The Lambda reduces ASG desired capacity
//   - The lifecycle hook (disable→stop) completes and returns the instance to Warmed:Stopped
//
// The alarm is first reset to OK so the OK→ALARM transition always fires
// regardless of state left by demand_monitor or a prior run.
func (e *testEnv) triggerScaleIn(ctx context.Context, t *testing.T) {
	t.Helper()

	if _, err := e.cwClient.SetAlarmState(ctx, &cloudwatch.SetAlarmStateInput{
		AlarmName:   aws.String(e.scaleInAlarm),
		StateValue:  cwtypes.StateValueOk,
		StateReason: aws.String("reset by integration test before triggerScaleIn"),
	}); err != nil {
		t.Logf("triggerScaleIn: SetAlarmState(OK) warning: %v", err)
	} else {
		t.Logf("triggerScaleIn: scale-in alarm reset to OK")
	}

	if _, err := e.cwClient.SetAlarmState(ctx, &cloudwatch.SetAlarmStateInput{
		AlarmName:   aws.String(e.scaleInAlarm),
		StateValue:  cwtypes.StateValueAlarm,
		StateReason: aws.String("integration test: active transfers dropped to zero"),
	}); err != nil {
		t.Logf("triggerScaleIn: SetAlarmState(ALARM) warning: %v", err)
	} else {
		t.Logf("triggerScaleIn: scale-in alarm set to ALARM (SNS→scalein Lambda→ASG triggered)")
	}
}

// waitForAllTasksTerminal polls until every task in taskIDs has reached a
// terminal state (SUCCEEDED, FAILED, or CANCELLED). Used to detect when
// active transfer demand drops to zero so the scale-in clock can start.
func (e *testEnv) waitForAllTasksTerminal(ctx context.Context, t *testing.T,
	taskIDs []string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	pending := make(map[string]bool, len(taskIDs))
	for _, id := range taskIDs {
		pending[id] = true
	}
	for time.Now().Before(deadline) && len(pending) > 0 {
		time.Sleep(10 * time.Second)
		for id := range pending {
			info := e.getTaskInfo(ctx, t, id)
			switch info.Status {
			case "SUCCEEDED", "FAILED", "CANCELLED":
				t.Logf("task %s: %s", id, info.Status)
				delete(pending, id)
			}
		}
	}
	if len(pending) > 0 {
		var ids []string
		for id := range pending {
			ids = append(ids, id)
		}
		t.Fatalf("tasks still pending after %s: %v", timeout, ids)
	}
}

// waitForTransferSucceeded polls until the task reaches SUCCEEDED (or fails/cancels).
// Uses the `globus` CLI since the task was submitted under the endpoint owner's identity.
func (e *testEnv) waitForTransferSucceeded(ctx context.Context, t *testing.T, taskID string, timeout time.Duration) *globusTaskInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		info := e.getTaskInfo(ctx, t, taskID)
		switch info.Status {
		case "SUCCEEDED":
			return info
		case "FAILED", "CANCELLED":
			t.Fatalf("transfer %s: terminal status %s", taskID, info.Status)
		}
	}
	t.Fatalf("transfer %s: timed out after %s (last polled via globus task show)", taskID, timeout)
	return nil
}
