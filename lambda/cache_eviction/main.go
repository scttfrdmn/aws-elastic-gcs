package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

const (
	warmPoolEntryTagKey = "elastic-gcs:warm-pool-entry-time"
	gcsNodeIDTagKey     = "elastic-gcs:gcs-node-id"
)

// gcsNodeResult is a subset of the `node list -F json` output.
// Full shape: [{"code":"success","data":[{"id":"...","status":"active/inactive",...}],...}]
type gcsNodeResult struct {
	Data []gcsNode `json:"data"`
}

type gcsNode struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// handler terminates warm pool instances that have been stopped too long,
// deletes their GCS node registrations, and sweeps any other orphaned nodes.
//
// Triggered by: EventBridge scheduled rule (daily)
//
// Logic:
//  1. List instances in ASG warm pool with state "Warmed:Stopped"
//  2. For each, check the elastic-gcs:warm-pool-entry-time tag (set by the
//     lifecycle Lambda on scale-in). Falls back to LaunchTime if absent.
//  3. If stopped > CACHE_EVICTION_HOURS → terminate + delete GCS node
//  4. Orphan sweep: list all GCS nodes; delete inactive ones whose
//     EC2 instance no longer exists (catches ABANDON'd / hard-terminated cases)
func handler(ctx context.Context, _ interface{}) error {
	asgName := os.Getenv("ASG_NAME")
	region := os.Getenv("AWS_REGION")
	secretsPrefix := os.Getenv("SECRETS_PREFIX")
	controlNodeIDParam := os.Getenv("CONTROL_NODE_ID_PARAM")
	evictionHours, _ := strconv.Atoi(os.Getenv("CACHE_EVICTION_HOURS"))
	if evictionHours == 0 {
		evictionHours = 72
	}

	log.Printf("Cache eviction check: region=%s asg=%s ttl=%dh", region, asgName, evictionHours)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	asgClient := autoscaling.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
	ssmClient := ssm.NewFromConfig(cfg)

	// Resolve control node instance ID (needed for GCS CLI operations via SSM).
	controlNodeID := ""
	if controlNodeIDParam != "" {
		paramOut, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
			Name: aws.String(controlNodeIDParam),
		})
		if err != nil {
			log.Printf("WARNING: could not fetch control node ID from %s: %v — GCS node cleanup skipped", controlNodeIDParam, err)
		} else {
			controlNodeID = aws.ToString(paramOut.Parameter.Value)
			log.Printf("Control node instance: %s", controlNodeID)
		}
	}

	// List warm pool instances.
	warmPool, err := asgClient.DescribeWarmPool(ctx, &autoscaling.DescribeWarmPoolInput{
		AutoScalingGroupName: aws.String(asgName),
	})
	if err != nil {
		return fmt.Errorf("DescribeWarmPool failed: %w", err)
	}

	var stoppedIDs []string
	for _, inst := range warmPool.Instances {
		if string(inst.LifecycleState) == "Warmed:Stopped" {
			stoppedIDs = append(stoppedIDs, aws.ToString(inst.InstanceId))
		}
	}

	if len(stoppedIDs) == 0 {
		log.Printf("No Warmed:Stopped instances to check")
	} else {
		log.Printf("Checking %d stopped warm pool instances for eviction (TTL=%dh)", len(stoppedIDs), evictionHours)
		if err := evictStaleInstances(ctx, ec2Client, ssmClient, stoppedIDs,
			evictionHours, region, secretsPrefix, controlNodeID); err != nil {
			log.Printf("WARNING: eviction step failed: %v", err)
		}
	}

	// Orphan sweep: delete GCS nodes whose EC2 instance no longer exists.
	// This catches nodes left behind by ABANDON'd lifecycle actions or hard terminates.
	if controlNodeID != "" && secretsPrefix != "" {
		if err := sweepOrphanedNodes(ctx, ssmClient, ec2Client, asgClient, controlNodeID, secretsPrefix, region, asgName); err != nil {
			log.Printf("WARNING: orphan node sweep failed: %v", err)
		}
	} else {
		log.Printf("Skipping orphan sweep: CONTROL_NODE_ID_PARAM=%q SECRETS_PREFIX=%q", controlNodeIDParam, secretsPrefix)
	}

	return nil
}

// evictStaleInstances terminates warm pool instances past their TTL and
// deletes their GCS node registrations.
func evictStaleInstances(ctx context.Context, ec2Client *ec2.Client, ssmClient *ssm.Client,
	instanceIDs []string, evictionHours int, region, secretsPrefix, controlNodeID string) error {

	ec2Instances, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return fmt.Errorf("DescribeInstances failed: %w", err)
	}

	evictionCutoff := time.Now().Add(-time.Duration(evictionHours) * time.Hour)
	var evicted int

	for _, reservation := range ec2Instances.Reservations {
		for _, inst := range reservation.Instances {
			instanceID := aws.ToString(inst.InstanceId)
			entryTime := warmPoolEntryTime(inst.Tags, aws.ToTime(inst.LaunchTime))
			age := time.Since(entryTime)
			log.Printf("Instance %s: warm-pool-entry=%s age=%s",
				instanceID, entryTime.Format(time.RFC3339), age.Round(time.Minute))

			if !entryTime.Before(evictionCutoff) {
				continue
			}

			log.Printf("Evicting %s (age %s > TTL %dh)", instanceID, age.Round(time.Hour), evictionHours)
			_, err := ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
				InstanceIds: []string{instanceID},
			})
			if err != nil {
				log.Printf("WARNING: Failed to terminate %s: %v", instanceID, err)
				continue
			}
			log.Printf("Terminated warm pool instance %s", instanceID)
			evicted++

			// Release SSM slot lock (may be orphaned if shutdown script didn't run).
			if slotNum := slotNumber(inst.Tags); slotNum != "" {
				lockParam := fmt.Sprintf("/elastic-gcs/%s/slot-lock/%s", region, slotNum)
				if _, ssmErr := ssmClient.DeleteParameter(ctx, &ssm.DeleteParameterInput{
					Name: aws.String(lockParam),
				}); ssmErr != nil {
					log.Printf("WARNING: Could not release slot lock %s: %v", lockParam, ssmErr)
				} else {
					log.Printf("Released slot lock %s for terminated instance %s", lockParam, instanceID)
				}
			} else {
				log.Printf("WARNING: No elastic-gcs:slot tag on %s — slot lock not released", instanceID)
			}

			// Delete the GCS node registration for the terminated instance.
			if controlNodeID != "" {
				nodeID := gcsNodeIDTag(inst.Tags)
				if nodeID != "" {
					log.Printf("Deleting GCS node %s for evicted instance %s", nodeID, instanceID)
					if err := deleteGCSNode(ctx, ssmClient, controlNodeID, secretsPrefix, region, nodeID); err != nil {
						log.Printf("WARNING: GCS node delete failed for %s: %v — will be caught by orphan sweep", nodeID, err)
					}
				} else {
					log.Printf("No %s tag on %s — GCS node not deleted; will be caught by orphan sweep", gcsNodeIDTagKey, instanceID)
				}
			}
		}
	}

	log.Printf("Eviction complete: checked=%d evicted=%d", len(instanceIDs), evicted)
	return nil
}

// sweepOrphanedNodes deletes GCS node registrations that are inactive and
// have no corresponding live EC2 instance (running, stopped, pending, or stopping).
//
// Safety: if any live ASG instance lacks the elastic-gcs:gcs-node-id tag (i.e., it
// was booted before that tag was added), the sweep is skipped entirely. Without the
// tag we cannot determine which GCS nodes belong to live instances, so deleting
// inactive nodes would break their next resume. This guards the transition period
// until all instances in the pool run a boot script version that sets the tag.
func sweepOrphanedNodes(ctx context.Context, ssmClient *ssm.Client, ec2Client *ec2.Client,
	asgClient *autoscaling.Client, controlNodeID, secretsPrefix, region, asgName string) error {

	log.Printf("Starting orphan GCS node sweep")

	// Collect all live ASG instances: active (InService) + warm pool (stopped/pending).
	var allInstanceIDs []string

	asgOut, err := asgClient.DescribeAutoScalingInstances(ctx, &autoscaling.DescribeAutoScalingInstancesInput{})
	if err == nil {
		for _, inst := range asgOut.AutoScalingInstances {
			if aws.ToString(inst.AutoScalingGroupName) == asgName {
				allInstanceIDs = append(allInstanceIDs, aws.ToString(inst.InstanceId))
			}
		}
	}
	warmPoolOut, err := asgClient.DescribeWarmPool(ctx, &autoscaling.DescribeWarmPoolInput{
		AutoScalingGroupName: aws.String(asgName),
	})
	if err == nil {
		for _, inst := range warmPoolOut.Instances {
			allInstanceIDs = append(allInstanceIDs, aws.ToString(inst.InstanceId))
		}
	}

	// Check every live instance for the gcs-node-id tag.
	// If any instance is untagged we cannot safely sweep — skip and log.
	activeNodeIDs := make(map[string]struct{})
	if len(allInstanceIDs) > 0 {
		ec2Out, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: allInstanceIDs,
		})
		if err != nil {
			return fmt.Errorf("DescribeInstances for ASG instances failed: %w", err)
		}
		for _, r := range ec2Out.Reservations {
			for _, inst := range r.Instances {
				if inst.State.Name == ec2types.InstanceStateNameTerminated {
					continue
				}
				nodeID := gcsNodeIDTag(inst.Tags)
				if nodeID == "" {
					log.Printf("Instance %s (%s) has no %s tag — skipping orphan sweep to avoid deleting live nodes. Rebuild warm pool after boot script update to enable sweep.",
						aws.ToString(inst.InstanceId), inst.State.Name, gcsNodeIDTagKey)
					return nil
				}
				activeNodeIDs[nodeID] = struct{}{}
				log.Printf("Live instance %s ↔ GCS node %s (%s)",
					aws.ToString(inst.InstanceId), nodeID, inst.State.Name)
			}
		}
	}

	// Get all GCS nodes from the endpoint via SSM on the control node.
	nodeListJSON, err := runGCSCLI(ctx, ssmClient, controlNodeID, secretsPrefix, region,
		"globus-connect-server node list -F json 2>/dev/null")
	if err != nil {
		return fmt.Errorf("node list via SSM failed: %w", err)
	}

	var results []gcsNodeResult
	if err := json.Unmarshal([]byte(nodeListJSON), &results); err != nil || len(results) == 0 {
		return fmt.Errorf("failed to parse node list JSON (output: %.200s): %w", nodeListJSON, err)
	}
	allNodes := results[0].Data
	log.Printf("GCS endpoint has %d nodes total", len(allNodes))

	// Delete inactive GCS nodes that have no live EC2 instance.
	var deleted, skipped int
	for _, node := range allNodes {
		if node.Status == "active" {
			skipped++
			continue // active nodes are always kept (control node + serving DTNs)
		}
		if _, ok := activeNodeIDs[node.ID]; ok {
			log.Printf("Inactive node %s has a live EC2 instance — skipping", node.ID)
			skipped++
			continue
		}
		log.Printf("Deleting orphaned inactive GCS node %s", node.ID)
		if err := deleteGCSNode(ctx, ssmClient, controlNodeID, secretsPrefix, region, node.ID); err != nil {
			log.Printf("WARNING: failed to delete orphan node %s: %v", node.ID, err)
		} else {
			deleted++
		}
	}

	log.Printf("Orphan sweep complete: total=%d active_ec2=%d skipped=%d deleted=%d",
		len(allNodes), len(activeNodeIDs), skipped, deleted)
	return nil
}

// deleteGCSNode deletes a GCS node registration via `node delete` run on the control node.
func deleteGCSNode(ctx context.Context, ssmClient *ssm.Client,
	controlNodeID, secretsPrefix, region, nodeID string) error {

	out, err := runGCSCLI(ctx, ssmClient, controlNodeID, secretsPrefix, region,
		fmt.Sprintf("globus-connect-server node delete %s 2>&1 || true", nodeID))
	if err != nil {
		return err
	}
	log.Printf("node delete %s: %s", nodeID, strings.TrimSpace(out))
	return nil
}

// runGCSCLI sends an SSM Run Command to the control node that authenticates
// the GCS CLI via Secrets Manager credentials and runs the provided command.
// Returns the command's stdout.
func runGCSCLI(ctx context.Context, ssmClient *ssm.Client,
	controlNodeID, secretsPrefix, region, gcsCommand string) (string, error) {

	script := fmt.Sprintf(`
export GCS_CLI_CLIENT_ID=$(aws secretsmanager get-secret-value \
    --secret-id %s/globus-client-id \
    --query SecretString --output text --region %s 2>/dev/null)
export GCS_CLI_CLIENT_SECRET=$(aws secretsmanager get-secret-value \
    --secret-id %s/globus-client-secret \
    --query SecretString --output text --region %s 2>/dev/null)
export GCS_CLI_ENDPOINT_ID=$(aws ssm get-parameter \
    --name /elastic-gcs/%s/endpoint-id \
    --query Parameter.Value --output text --region %s 2>/dev/null)
%s`, secretsPrefix, region, secretsPrefix, region, region, region, gcsCommand)

	cmdOut, err := ssmClient.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:  []string{controlNodeID},
		DocumentName: aws.String("AWS-RunShellScript"),
		Parameters: map[string][]string{
			"commands":         {script},
			"executionTimeout": {"60"},
		},
		Comment: aws.String("elastic-gcs: GCS node cleanup"),
	})
	if err != nil {
		return "", fmt.Errorf("SSM SendCommand failed: %w", err)
	}

	commandID := aws.ToString(cmdOut.Command.CommandId)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		inv, err := ssmClient.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(commandID),
			InstanceId: aws.String(controlNodeID),
		})
		if err != nil {
			continue
		}
		switch inv.Status {
		case ssmtypes.CommandInvocationStatusSuccess:
			return aws.ToString(inv.StandardOutputContent), nil
		case ssmtypes.CommandInvocationStatusFailed:
			return aws.ToString(inv.StandardOutputContent),
				fmt.Errorf("SSM command failed: %s", aws.ToString(inv.StandardErrorContent))
		case ssmtypes.CommandInvocationStatusTimedOut,
			ssmtypes.CommandInvocationStatusCancelled,
			ssmtypes.CommandInvocationStatusCancelling:
			return "", fmt.Errorf("SSM command in terminal state: %s", inv.Status)
		}
		// Pending / InProgress — keep polling
	}
	return "", fmt.Errorf("timed out waiting for SSM command %s", commandID)
}

// gcsNodeIDTag returns the elastic-gcs:gcs-node-id EC2 tag value, or "".
func gcsNodeIDTag(tags []ec2types.Tag) string {
	for _, tag := range tags {
		if aws.ToString(tag.Key) == gcsNodeIDTagKey {
			return aws.ToString(tag.Value)
		}
	}
	return ""
}

// slotNumber returns the DTN slot number from the elastic-gcs:slot EC2 tag, or "".
func slotNumber(tags []ec2types.Tag) string {
	for _, tag := range tags {
		if aws.ToString(tag.Key) == "elastic-gcs:slot" {
			return aws.ToString(tag.Value)
		}
	}
	return ""
}

// warmPoolEntryTime returns the time the instance entered the warm pool.
// Uses the elastic-gcs:warm-pool-entry-time tag when present, falls back to launch time.
func warmPoolEntryTime(tags []ec2types.Tag, launchTime time.Time) time.Time {
	for _, tag := range tags {
		if aws.ToString(tag.Key) == warmPoolEntryTagKey {
			if t, err := time.Parse(time.RFC3339, aws.ToString(tag.Value)); err == nil {
				return t
			}
		}
	}
	return launchTime
}

func main() {
	log.Printf("Elastic GCS Cache Eviction Lambda starting")
	lambda.Start(handler)
}
