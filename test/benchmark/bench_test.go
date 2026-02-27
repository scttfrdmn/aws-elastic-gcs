//go:build benchmark

package benchmark

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
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// Well-known IDs used across benchmark tests.
const (
	collection1000Genomes = "28daf55e-07a4-4a65-9f54-292b0b4b8739"
	destEndpoint          = "31ce9ba0-176d-45a5-add3-f37d233ba47d" // Globus Tutorial Endpoint 2
	testSourceFile        = "/1000genomes/release/20130502/20140625_related_individuals.txt"
	testDestFile          = "/~/elastic-gcs-bench.txt"
)

// benchEnv holds AWS clients and configuration for benchmark tests.
type benchEnv struct {
	region           string
	asgName          string
	controlNodeParam string
	endpointIDParam  string

	gcsCLIClientID     string
	gcsCLIClientSecret string
	gcsEndpointID      string

	cfg       aws.Config
	asgClient *autoscaling.Client
	ec2Client *ec2.Client
	ssmClient *ssm.Client
}

// newBenchEnv initialises clients from environment variables.
// Required: GLOBUS_CLIENT_ID, GLOBUS_CLIENT_SECRET.
// Optional: ELASTIC_GCS_REGION (default us-east-1), ELASTIC_GCS_ASG_NAME.
func newBenchEnv(t *testing.T) *benchEnv {
	t.Helper()

	region := getEnvOrDefault("ELASTIC_GCS_REGION", "us-east-1")
	clientID := requireEnv(t, "GLOBUS_CLIENT_ID")
	clientSecret := requireEnv(t, "GLOBUS_CLIENT_SECRET")

	asgName := getEnvOrDefault("ELASTIC_GCS_ASG_NAME", fmt.Sprintf("elastic-gcs-dtn-%s", region))
	controlNodeParam := fmt.Sprintf("/elastic-gcs/%s/control-node-id", region)
	endpointIDParam := fmt.Sprintf("/elastic-gcs/%s/endpoint-id", region)

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region))
	if err != nil {
		t.Fatalf("AWS config: %v", err)
	}

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

	return &benchEnv{
		region:             region,
		asgName:            asgName,
		controlNodeParam:   controlNodeParam,
		endpointIDParam:    endpointIDParam,
		gcsCLIClientID:     clientID,
		gcsCLIClientSecret: clientSecret,
		gcsEndpointID:      gcsEndpointID,
		cfg:                cfg,
		asgClient:          autoscaling.NewFromConfig(cfg),
		ec2Client:          ec2.NewFromConfig(cfg),
		ssmClient:          ssmClient,
	}
}

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
func (e *benchEnv) controlNodeID(ctx context.Context, t *testing.T) string {
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
func (e *benchEnv) scaleASG(ctx context.Context, t *testing.T, desired int32) {
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

// waitForWarmPool polls until >= minCount Warmed:Stopped instances exist.
func (e *benchEnv) waitForWarmPool(ctx context.Context, t *testing.T, minCount int, timeout time.Duration) []string {
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

// waitForASGInService polls until >= minCount InService instances exist.
func (e *benchEnv) waitForASGInService(ctx context.Context, t *testing.T, minCount int, timeout time.Duration) []string {
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
	t.Fatalf("timed out waiting for %d InService instances after %s", minCount, timeout)
	return nil
}

// firstASGInstanceID waits for any non-warm-pool instance to appear in the ASG.
func (e *benchEnv) firstASGInstanceID(ctx context.Context, t *testing.T, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		out, err := e.asgClient.DescribeAutoScalingGroups(ctx,
			&autoscaling.DescribeAutoScalingGroupsInput{
				AutoScalingGroupNames: []string{e.asgName},
			})
		if err != nil || len(out.AutoScalingGroups) == 0 {
			continue
		}
		for _, inst := range out.AutoScalingGroups[0].Instances {
			if id := aws.ToString(inst.InstanceId); id != "" {
				return id
			}
		}
	}
	t.Fatalf("no instance appeared in ASG within %s", timeout)
	return ""
}

// waitForEC2Running polls until the instance reaches the EC2 "running" state.
// Returns the time it transitioned.
func (e *benchEnv) waitForEC2Running(ctx context.Context, t *testing.T, instanceID string, timeout time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		out, err := e.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err != nil || len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
			continue
		}
		if out.Reservations[0].Instances[0].State.Name == ec2types.InstanceStateNameRunning {
			return time.Now()
		}
	}
	t.Fatalf("instance %s did not reach running state within %s", instanceID, timeout)
	return time.Now()
}

// waitForSSMOnline polls until the SSM agent on the instance reports Online.
// Returns the time it came online.
func (e *benchEnv) waitForSSMOnline(ctx context.Context, t *testing.T, instanceID string, timeout time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		out, err := e.ssmClient.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
			Filters: []ssmtypes.InstanceInformationStringFilter{
				{Key: aws.String("InstanceIds"), Values: []string{instanceID}},
			},
		})
		if err != nil || len(out.InstanceInformationList) == 0 {
			continue
		}
		if out.InstanceInformationList[0].PingStatus == ssmtypes.PingStatusOnline {
			return time.Now()
		}
	}
	t.Fatalf("SSM agent on %s did not come online within %s", instanceID, timeout)
	return time.Now()
}

// gcsNodeStatus holds fields from `node list -F json`.
type gcsNodeStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// gcsNodeListResult is the result#1.x.x wrapper returned by the GCS CLI.
type gcsNodeListResult struct {
	Code string          `json:"code"`
	Data []gcsNodeStatus `json:"data"`
}

// countActiveGCSNodes returns the number of nodes with status "active".
// Returns 0 silently on error (e.g. SSM not yet ready).
func (e *benchEnv) countActiveGCSNodes(ctx context.Context, t *testing.T) int {
	t.Helper()
	controlID := e.controlNodeID(ctx, t)
	cmd := fmt.Sprintf(
		"export GCS_CLI_CLIENT_ID=%s; export GCS_CLI_CLIENT_SECRET=%s; export GCS_CLI_ENDPOINT_ID=%s; "+
			"sudo -E globus-connect-server node list -F json 2>&1",
		e.gcsCLIClientID, e.gcsCLIClientSecret, e.gcsEndpointID,
	)
	raw := e.runSSMCommandSoft(ctx, controlID, cmd)
	if raw == "" {
		return 0
	}
	var results []gcsNodeListResult
	if err := json.Unmarshal([]byte(raw), &results); err != nil || len(results) == 0 {
		return 0
	}
	n := 0
	for _, nd := range results[0].Data {
		if strings.EqualFold(nd.Status, "active") {
			n++
		}
	}
	return n
}

// waitForGCSNodeActive polls until the active GCS node count exceeds the baseline.
// Returns the time the new node became active.
func (e *benchEnv) waitForGCSNodeActive(ctx context.Context, t *testing.T, baselineActive int, timeout time.Duration) time.Time {
	t.Helper()
	controlID := e.controlNodeID(ctx, t)
	cmd := fmt.Sprintf(
		"export GCS_CLI_CLIENT_ID=%s; export GCS_CLI_CLIENT_SECRET=%s; export GCS_CLI_ENDPOINT_ID=%s; "+
			"sudo -E globus-connect-server node list -F json 2>&1",
		e.gcsCLIClientID, e.gcsCLIClientSecret, e.gcsEndpointID,
	)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		raw := e.runSSMCommandSoft(ctx, controlID, cmd)
		if raw == "" {
			continue
		}
		var results []gcsNodeListResult
		if err := json.Unmarshal([]byte(raw), &results); err != nil || len(results) == 0 {
			continue
		}
		n := 0
		for _, nd := range results[0].Data {
			if strings.EqualFold(nd.Status, "active") {
				n++
			}
		}
		if n > baselineActive {
			return time.Now()
		}
	}
	t.Fatalf("DTN GCS node did not become active within %s", timeout)
	return time.Now()
}

// runSSMCommand executes a shell command on an EC2 instance via SSM RunCommand.
// Fails the test on error.
func (e *benchEnv) runSSMCommand(ctx context.Context, t *testing.T, instanceID, cmd string) string {
	t.Helper()
	result, err := e.sendSSMCommand(ctx, instanceID, cmd)
	if err != nil {
		t.Fatalf("SSM command on %s: %v", instanceID, err)
	}
	return result
}

// runSSMCommandSoft executes a shell command via SSM, returning "" on any error.
func (e *benchEnv) runSSMCommandSoft(ctx context.Context, instanceID, cmd string) string {
	result, _ := e.sendSSMCommand(ctx, instanceID, cmd)
	return result
}

// sendSSMCommand sends an SSM RunCommand and waits for completion.
func (e *benchEnv) sendSSMCommand(ctx context.Context, instanceID, cmd string) (string, error) {
	out, err := e.ssmClient.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:  []string{instanceID},
		DocumentName: aws.String("AWS-RunShellScript"),
		Parameters: map[string][]string{
			"commands":         {cmd},
			"executionTimeout": {"60"},
		},
	})
	if err != nil {
		return "", fmt.Errorf("SendCommand: %w", err)
	}
	cmdID := aws.ToString(out.Command.CommandId)

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
			return strings.TrimSpace(aws.ToString(inv.StandardOutputContent)), nil
		case ssmtypes.CommandInvocationStatusFailed,
			ssmtypes.CommandInvocationStatusTimedOut,
			ssmtypes.CommandInvocationStatusCancelled:
			return "", fmt.Errorf("status=%s stderr=%s",
				inv.Status, aws.ToString(inv.StandardErrorContent))
		}
	}
	return "", fmt.Errorf("timed out waiting for SSM command")
}

// submitTestTransfer submits a transfer via the `globus` CLI and returns the task ID.
func (e *benchEnv) submitTestTransfer(ctx context.Context, t *testing.T) string {
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
		"--label", fmt.Sprintf("elastic-gcs-bench-%s", time.Now().Format("20060102-150405")),
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
	return result.TaskID
}

// globusTaskInfo holds fields from `globus task show -F json`.
type globusTaskInfo struct {
	TaskID           string `json:"task_id"`
	Status           string `json:"status"`
	BytesTransferred int64  `json:"bytes_transferred"`
}

// getTaskInfo fetches task status via the `globus` CLI.
func (e *benchEnv) getTaskInfo(ctx context.Context, t *testing.T, taskID string) *globusTaskInfo {
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

// waitForTransferSucceeded polls until the task reaches SUCCEEDED or a terminal failure.
func (e *benchEnv) waitForTransferSucceeded(ctx context.Context, t *testing.T, taskID string, timeout time.Duration) *globusTaskInfo {
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
	t.Fatalf("transfer %s: timed out after %s", taskID, timeout)
	return nil
}

// waitForFirstBytes polls until BytesTransferred > 0 or task succeeds.
// Returns the time the first bytes were observed.
func (e *benchEnv) waitForFirstBytes(ctx context.Context, t *testing.T, taskID string, timeout time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		info := e.getTaskInfo(ctx, t, taskID)
		if info.BytesTransferred > 0 || info.Status == "SUCCEEDED" {
			return time.Now()
		}
		if info.Status == "FAILED" || info.Status == "CANCELLED" {
			t.Fatalf("transfer %s: %s", taskID, info.Status)
		}
	}
	t.Fatalf("no bytes transferred for task %s within %s", taskID, timeout)
	return time.Now()
}
