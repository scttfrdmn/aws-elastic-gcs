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

	"github.com/scttfrdmn/aws-elastic-gcs/internal/gcscli"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/ssmexec"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// Well-known IDs used across all integration tests.
const (
	collection1000Genomes = "28daf55e-07a4-4a65-9f54-292b0b4b8739"
	destEndpoint          = "31ce9ba0-176d-45a5-add3-f37d233ba47d" // Globus Tutorial Endpoint 2
	testSourceFile        = "/1000genomes/release/20130502/20140625_related_individuals.txt"
	testDestFile          = "/~/elastic-gcs-inttest.txt"
)

// testEnv holds shared AWS clients for integration tests.
type testEnv struct {
	region           string
	dep              names.Deployment
	secretsPrefix    string
	controlNodeParam string
	endpointIDParam  string

	// GCS CLI credentials (same as Globus client creds) used when running
	// globus-connect-server commands on the control node via SSM. These are
	// passed as GCS_CLI_CLIENT_ID/SECRET/ENDPOINT_ID env vars.
	gcsCLIClientID string
	gcsEndpointID  string

	cfg       aws.Config
	ec2Client *ec2.Client
	ssmClient *ssm.Client
}

// newTestEnv initialises shared clients from environment variables.
// Required env vars: GLOBUS_CLIENT_ID, GLOBUS_CLIENT_SECRET.
// Optional: ELASTIC_GCS_REGION (default us-east-1), ELASTIC_GCS_DEPLOYMENT
// (default "default").
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	region := os.Getenv("ELASTIC_GCS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	clientID := requireEnv(t, "GLOBUS_CLIENT_ID")
	requireEnv(t, "GLOBUS_CLIENT_SECRET")

	dep, err := names.New(region, os.Getenv("ELASTIC_GCS_DEPLOYMENT"))
	if err != nil {
		t.Fatalf("ELASTIC_GCS_REGION/ELASTIC_GCS_DEPLOYMENT: %v", err)
	}
	secretsPrefix := getEnvOrDefault("ELASTIC_GCS_SECRETS_PREFIX", dep.SecretsPrefix())
	controlNodeParam := dep.Param(names.ParamControlNodeID)
	endpointIDParam := dep.Param(names.ParamEndpointID)

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

	return &testEnv{
		region:           region,
		dep:              dep,
		secretsPrefix:    secretsPrefix,
		controlNodeParam: controlNodeParam,
		endpointIDParam:  endpointIDParam,
		gcsCLIClientID:   clientID,
		gcsEndpointID:    gcsEndpointID,
		cfg:              cfg,
		ec2Client:        ec2.NewFromConfig(cfg),
		ssmClient:        ssmClient,
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

// ── Slot controller (#65) ─────────────────────────────────────────────────────

// setDesired sets desired-dtns for the slot controller. It also refreshes
// last-active, so demand_monitor keeps the value for scale_in_idle instead of
// resetting it while no transfers are active.
func (e *testEnv) setDesired(ctx context.Context, t *testing.T, desired int) {
	t.Helper()
	for _, kv := range [][2]string{
		{names.ParamLastActive, time.Now().UTC().Format(time.RFC3339)},
		{names.ParamDesiredDTNs, fmt.Sprint(desired)},
	} {
		if _, err := e.ssmClient.PutParameter(ctx, &ssm.PutParameterInput{
			Name: aws.String(e.dep.Param(kv[0])), Value: aws.String(kv[1]),
			Type: ssmtypes.ParameterTypeString, Overwrite: aws.Bool(true),
		}); err != nil {
			t.Fatalf("put %s=%s: %v", kv[0], kv[1], err)
		}
	}
}

// slotInstances returns this deployment's slot instances in the given EC2
// states, mapped to their elastic-gcs:phase tag ("" once healthy).
func (e *testEnv) slotInstances(ctx context.Context, states ...string) (map[string]string, error) {
	out, err := e.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:" + names.TagDeployment), Values: []string{e.dep.ID()}},
		{Name: aws.String("tag:elastic-gcs:pool"), Values: []string{"slots"}},
		{Name: aws.String("instance-state-name"), Values: states},
	}})
	if err != nil {
		return nil, err
	}
	ids := map[string]string{}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			phase := ""
			for _, tag := range i.Tags {
				if aws.ToString(tag.Key) == "elastic-gcs:phase" {
					phase = aws.ToString(tag.Value)
				}
			}
			ids[aws.ToString(i.InstanceId)] = phase
		}
	}
	return ids, nil
}

// servingSlotIDs returns the slot instances that are running and healthy (the
// controller clears their starting/reviving phase).
func (e *testEnv) servingSlotIDs(ctx context.Context) []string {
	running, err := e.slotInstances(ctx, "running")
	if err != nil {
		return nil
	}
	var ids []string
	for id, phase := range running {
		if phase == "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// waitForServingSlots polls until >= minCount slots are serving.
func (e *testEnv) waitForServingSlots(ctx context.Context, t *testing.T, minCount int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		if ids := e.servingSlotIDs(ctx); len(ids) >= minCount {
			return ids
		}
	}
	t.Fatalf("timed out waiting for %d serving slots after %s", minCount, timeout)
	return nil
}

// waitForNoRunningSlots polls until no slot instance is pending, running or
// stopping (every slot is WARM or COLD).
func (e *testEnv) waitForNoRunningSlots(ctx context.Context, t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		running, err := e.slotInstances(ctx, "pending", "running", "stopping")
		if err == nil && len(running) == 0 {
			return
		}
	}
	t.Fatalf("slots still running after %s", timeout)
}

// ── SSM and GCS ───────────────────────────────────────────────────────────────

// runSSMCommand executes a shell command on an EC2 instance via SSM RunCommand
// and returns stdout. Fails the test on error or non-zero exit.
func (e *testEnv) runSSMCommand(ctx context.Context, t *testing.T, instanceID, cmd string) string {
	t.Helper()
	res, err := ssmexec.RunOK(ctx, e.ssmClient, instanceID, cmd, ssmexec.Options{Wait: 2 * time.Minute})
	if err != nil {
		t.Fatalf("SSM command %q on %s: %v", cmd, instanceID, err)
	}
	return strings.TrimSpace(res.Stdout)
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
	// Credentials are read on the node (gcscli.Preamble), never placed in the
	// SSM command text, which SSM keeps in its history (#48).
	cmd := gcscli.Script(e.dep, "globus-connect-server node list -F json 2>&1")
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

// activeNodeCount returns the number of GCS nodes with status "active".
func activeNodeCount(nodes []gcsNodeStatus) int {
	n := 0
	for _, node := range nodes {
		if strings.EqualFold(node.Status, "active") {
			n++
		}
	}
	return n
}

// ── Transfers ─────────────────────────────────────────────────────────────────

// submitTestTransfer submits a file transfer via the `globus` CLI (authenticated
// as the endpoint owner) and returns the Globus task ID.
//
// We use the CLI instead of the SDK client credentials flow because the automation
// client doesn't have user-level transfer permission on the mapped collection.
// The `globus` CLI is authenticated as scofri@globusid.org which owns the endpoint.
func (e *testEnv) submitTestTransfer(ctx context.Context, t *testing.T) string {
	t.Helper()
	// Retry for up to 3 minutes to cover GCS DNS propagation after scale-out
	// (Globus docs: "up to 2 minutes" after node enable).
	destPath := fmt.Sprintf("/~/elastic-gcs-inttest-%d.txt", time.Now().Unix())
	for attempt := 1; attempt <= 12; attempt++ {
		taskID, err := e.trySubmitTransferWithDest(ctx, destPath)
		if err == nil {
			t.Logf("submitted transfer task %s", taskID)
			return taskID
		}
		t.Logf("submitTestTransfer: attempt %d failed: %v", attempt, err)
		if attempt < 12 {
			time.Sleep(15 * time.Second)
		}
	}
	t.Fatal("submitTestTransfer: all attempts failed")
	return ""
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

// trySubmitTransferWithDest attempts a transfer submission and returns
// (taskID, nil) on success or ("", err) on failure without calling t.Fatal,
// to tolerate transient endpoint failures (e.g. during DNS propagation after
// a node joins or leaves).
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
		"--label", fmt.Sprintf("elastic-gcs-inttest-%s", time.Now().Format("20060102-150405")),
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
