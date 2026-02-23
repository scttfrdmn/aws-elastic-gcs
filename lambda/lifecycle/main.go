package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/scttfrdmn/elastic-gcs/internal/logger"
)

// LifecycleEvent is the detail payload from EventBridge ASG lifecycle hook events.
type LifecycleEvent struct {
	LifecycleHookName    string `json:"LifecycleHookName"`
	AutoScalingGroupName string `json:"AutoScalingGroupName"`
	LifecycleActionToken string `json:"LifecycleActionToken"`
	EC2InstanceID        string `json:"EC2InstanceId"`
	LifecycleTransition  string `json:"LifecycleTransition"`
	// Origin and Destination are present for warm pool lifecycle events.
	// Destination=="EC2" means permanent termination (vs "WarmPool" for scale-in to stopped).
	Origin      string `json:"Origin"`
	Destination string `json:"Destination"`
}

// Handler processes ASG lifecycle hook events for DTN nodes.
//
// Scale-out flow (LAUNCHING):
//
//	The boot script (dtn_boot.sh) handles its own lifecycle completion via
//	CompleteLifecycleAction when the GCS node becomes active. This Lambda is
//	a no-op for LAUNCHING events since the boot script signals first.
//
// Scale-in flow (TERMINATING):
//  1. Receives the lifecycle hook event before the instance stops
//  2. Sends SSM Run Command to run: globus-connect-server node disable
//  3. Waits for command completion (up to 90 seconds)
//  4. Tags the instance with elastic-gcs:warm-pool-entry-time (for TTL eviction)
//  5. Signals CONTINUE — ASG then stops/terminates the instance
//  6. systemd ExecStop also runs dtn_shutdown.sh (idempotent second disable)
func handler(ctx context.Context, event events.CloudWatchEvent) error {
	var lifecycle LifecycleEvent
	if err := json.Unmarshal(event.Detail, &lifecycle); err != nil {
		return fmt.Errorf("failed to parse lifecycle event: %w", err)
	}

	logger.Info("lifecycle event received",
		"transition", lifecycle.LifecycleTransition,
		"instance", lifecycle.EC2InstanceID,
		"hook", lifecycle.LifecycleHookName,
		"destination", lifecycle.Destination,
	)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	asgClient := autoscaling.NewFromConfig(cfg)
	ssmClient := ssm.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)

	region := os.Getenv("AWS_REGION")

	switch lifecycle.LifecycleTransition {
	case "autoscaling:EC2_INSTANCE_TERMINATING":
		return handleScaleIn(ctx, ssmClient, asgClient, ec2Client, lifecycle, region)
	case "autoscaling:EC2_INSTANCE_LAUNCHING":
		// Boot script handles its own completion. Lambda is a no-op here.
		logger.Info("scale-out LAUNCHING: boot script handles lifecycle, no Lambda action needed")
		return nil
	default:
		logger.Warn("unhandled lifecycle transition, completing with CONTINUE",
			"transition", lifecycle.LifecycleTransition,
		)
		return completeLifecycle(ctx, asgClient, lifecycle, "CONTINUE")
	}
}

// handleScaleIn disables the GCS node via SSM before allowing the instance to stop,
// then tags the instance with the time it entered the warm pool (for TTL eviction).
// If the instance is being permanently terminated (Destination=="EC2"), also releases
// the SSM slot lock so the slot can be claimed by a new instance.
// Preventing a node from stopping while still registered in GCS DNS avoids
// the "endpoint unreliable" state documented in the GCS docs.
func handleScaleIn(ctx context.Context, ssmClient *ssm.Client, asgClient *autoscaling.Client, ec2Client *ec2.Client, event LifecycleEvent, region string) error {
	logger.Info("scale-in: disabling GCS node", "instance", event.EC2InstanceID)

	// Send node disable command via SSM Run Command.
	// Use || true so exit code is always 0 — node may already be disabled
	// or GCS may not be configured on this instance.
	cmdOut, err := ssmClient.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:  []string{event.EC2InstanceID},
		DocumentName: aws.String("AWS-RunShellScript"),
		Parameters: map[string][]string{
			"commands":         {"globus-connect-server node disable 2>&1 || true"},
			"executionTimeout": {"60"},
		},
		Comment: aws.String("elastic-gcs: disable node before scale-in"),
	})
	if err != nil {
		// SSM may fail if agent isn't running (e.g. already stopping).
		// The systemd ExecStop will run dtn_shutdown.sh when the instance stops.
		logger.Warn("SSM SendCommand failed, relying on systemd ExecStop for node disable",
			"instance", event.EC2InstanceID,
			"error", err.Error(),
		)
		finalizeScaleIn(ctx, ec2Client, ssmClient, event.EC2InstanceID, region, event.Destination)
		return completeLifecycle(ctx, asgClient, event, "CONTINUE")
	}

	commandID := aws.ToString(cmdOut.Command.CommandId)
	logger.Info("SSM node-disable command sent, polling for completion",
		"command_id", commandID,
		"instance", event.EC2InstanceID,
	)

	// Poll for SSM command completion (up to 90 seconds).
	// Lambda timeout is 6 minutes; lifecycle hook heartbeat is 5 minutes.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)

		inv, err := ssmClient.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(commandID),
			InstanceId: aws.String(event.EC2InstanceID),
		})
		if err != nil {
			logger.Warn("GetCommandInvocation error, retrying", "error", err.Error())
			continue
		}

		logger.Info("SSM command status", "status", string(inv.Status), "command_id", commandID)

		switch inv.Status {
		case ssmtypes.CommandInvocationStatusSuccess,
			ssmtypes.CommandInvocationStatusFailed:
			// Success or failed (e.g. GCS not configured) — node is not serving.
			output := aws.ToString(inv.StandardOutputContent)
			if len(output) > 200 {
				output = output[:200] + "..."
			}
			logger.Info("node disable output", "output", output, "instance", event.EC2InstanceID)
			finalizeScaleIn(ctx, ec2Client, ssmClient, event.EC2InstanceID, region, event.Destination)
			return completeLifecycle(ctx, asgClient, event, "CONTINUE")

		case ssmtypes.CommandInvocationStatusTimedOut,
			ssmtypes.CommandInvocationStatusCancelled,
			ssmtypes.CommandInvocationStatusCancelling:
			logger.Warn("SSM command in terminal error state, proceeding with CONTINUE",
				"status", string(inv.Status),
			)
			finalizeScaleIn(ctx, ec2Client, ssmClient, event.EC2InstanceID, region, event.Destination)
			return completeLifecycle(ctx, asgClient, event, "CONTINUE")
		}
		// InProgress / Pending — keep polling
	}

	logger.Warn("timed out waiting for SSM node disable, proceeding with CONTINUE",
		"instance", event.EC2InstanceID,
	)
	finalizeScaleIn(ctx, ec2Client, ssmClient, event.EC2InstanceID, region, event.Destination)
	return completeLifecycle(ctx, asgClient, event, "CONTINUE")
}

// finalizeScaleIn tags the instance with the warm-pool entry time and, if the
// instance is being permanently terminated (destination=="EC2"), releases the
// SSM slot lock so new instances can claim that slot.
func finalizeScaleIn(ctx context.Context, ec2Client *ec2.Client, ssmClient *ssm.Client, instanceID, region, destination string) {
	tagWarmPoolEntry(ctx, ec2Client, instanceID)
	if destination == "EC2" {
		releaseSlotLock(ctx, ec2Client, ssmClient, instanceID, region)
	} else {
		logger.Info("instance going to warm pool, keeping slot lock",
			"instance", instanceID,
			"destination", destination,
		)
	}
}

// tagWarmPoolEntry records the time this instance entered the warm pool.
// The cache eviction Lambda uses this tag to calculate the correct TTL,
// since LaunchTime does not reset when an instance is stopped and resumed.
func tagWarmPoolEntry(ctx context.Context, ec2Client *ec2.Client, instanceID string) {
	entryTime := time.Now().UTC().Format(time.RFC3339)
	_, err := ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{instanceID},
		Tags: []ec2types.Tag{
			{
				Key:   aws.String("elastic-gcs:warm-pool-entry-time"),
				Value: aws.String(entryTime),
			},
		},
	})
	if err != nil {
		logger.Warn("failed to tag instance with warm-pool-entry-time",
			"instance", instanceID,
			"error", err.Error(),
		)
	} else {
		logger.Info("tagged warm-pool-entry-time",
			"instance", instanceID,
			"entry_time", entryTime,
		)
	}
}

// releaseSlotLock deletes the SSM slot-lock parameter for the given instance,
// allowing a new instance to claim that slot. Called only on permanent termination.
// It verifies the lock is actually held by this instance before deleting it —
// an instance may carry a stale slot tag without holding the lock (e.g. if it was
// stopped without ever acquiring the lock, or if the lock was already released).
func releaseSlotLock(ctx context.Context, ec2Client *ec2.Client, ssmClient *ssm.Client, instanceID, region string) {
	out, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil || len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		logger.Warn("could not describe instance to find slot tag",
			"instance", instanceID,
			"error", fmt.Sprintf("%v", err),
		)
		return
	}
	inst := out.Reservations[0].Instances[0]
	var slot string
	for _, tag := range inst.Tags {
		if aws.ToString(tag.Key) == "elastic-gcs:slot" {
			slot = aws.ToString(tag.Value)
			break
		}
	}
	if slot == "" {
		logger.Info("instance has no elastic-gcs:slot tag, slot lock not released",
			"instance", instanceID,
		)
		return
	}
	paramName := fmt.Sprintf("/elastic-gcs/%s/slot-lock/%s", region, slot)

	// Verify this instance actually holds the lock before releasing it.
	// The EC2 slot tag may be stale (e.g. if the instance never acquired the lock
	// or if another instance already holds it). Deleting a lock we don't own would
	// break the other instance's ability to resume.
	lockOut, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(paramName),
	})
	if err != nil {
		logger.Info("slot lock not found, nothing to release",
			"param", paramName,
		)
		return
	}
	lockOwner := aws.ToString(lockOut.Parameter.Value)
	if lockOwner != instanceID {
		logger.Warn("slot lock owned by another instance, NOT releasing",
			"param", paramName,
			"owner", lockOwner,
			"instance", instanceID,
		)
		return
	}

	_, err = ssmClient.DeleteParameter(ctx, &ssm.DeleteParameterInput{
		Name: aws.String(paramName),
	})
	if err != nil {
		logger.Warn("failed to release slot lock",
			"param", paramName,
			"error", err.Error(),
		)
	} else {
		logger.Info("released slot lock for terminated instance",
			"param", paramName,
			"instance", instanceID,
		)
	}
}

// completeLifecycle signals the ASG lifecycle hook with the given result (CONTINUE or ABANDON).
func completeLifecycle(ctx context.Context, asgClient *autoscaling.Client, event LifecycleEvent, result string) error {
	logger.Info("completing lifecycle action",
		"hook", event.LifecycleHookName,
		"instance", event.EC2InstanceID,
		"result", result,
	)

	_, err := asgClient.CompleteLifecycleAction(ctx, &autoscaling.CompleteLifecycleActionInput{
		LifecycleHookName:     aws.String(event.LifecycleHookName),
		AutoScalingGroupName:  aws.String(event.AutoScalingGroupName),
		LifecycleActionToken:  aws.String(event.LifecycleActionToken),
		InstanceId:            aws.String(event.EC2InstanceID),
		LifecycleActionResult: aws.String(result),
	})
	if err != nil {
		// If the action was already completed (by boot script or DefaultResult timeout),
		// this returns an error. Log it but don't fail the Lambda.
		logger.Warn("CompleteLifecycleAction warning (may already be complete)",
			"error", err.Error(),
		)
	}
	return nil
}

func main() {
	region := os.Getenv("AWS_REGION")
	logger.Init("elastic-gcs-lifecycle", region)
	lambda.Start(handler)
}
