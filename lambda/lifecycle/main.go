package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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

	log.Printf("Lifecycle event: transition=%s instance=%s hook=%s",
		lifecycle.LifecycleTransition,
		lifecycle.EC2InstanceID,
		lifecycle.LifecycleHookName,
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
		log.Printf("Scale-out LAUNCHING: boot script handles lifecycle, no Lambda action needed")
		return nil
	default:
		log.Printf("Unhandled lifecycle transition: %s — completing with CONTINUE", lifecycle.LifecycleTransition)
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
	log.Printf("Scale-in: disabling GCS node on %s", event.EC2InstanceID)

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
		log.Printf("WARNING: SSM SendCommand failed: %v — relying on systemd ExecStop for node disable", err)
		finalizeScaleIn(ctx, ec2Client, ssmClient, event.EC2InstanceID, region, event.Destination)
		return completeLifecycle(ctx, asgClient, event, "CONTINUE")
	}

	commandID := aws.ToString(cmdOut.Command.CommandId)
	log.Printf("SSM command sent: %s — polling for completion", commandID)

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
			log.Printf("GetCommandInvocation error: %v — retrying", err)
			continue
		}

		log.Printf("SSM command status: %s", string(inv.Status))

		switch inv.Status {
		case ssmtypes.CommandInvocationStatusSuccess,
			ssmtypes.CommandInvocationStatusFailed:
			// Success or failed (e.g. GCS not configured) — node is not serving.
			output := aws.ToString(inv.StandardOutputContent)
			if len(output) > 200 {
				output = output[:200] + "..."
			}
			log.Printf("Node disable output: %s", output)
			finalizeScaleIn(ctx, ec2Client, ssmClient, event.EC2InstanceID, region, event.Destination)
			return completeLifecycle(ctx, asgClient, event, "CONTINUE")

		case ssmtypes.CommandInvocationStatusTimedOut,
			ssmtypes.CommandInvocationStatusCancelled,
			ssmtypes.CommandInvocationStatusCancelling:
			log.Printf("SSM command in terminal error state: %s — proceeding with CONTINUE", inv.Status)
			finalizeScaleIn(ctx, ec2Client, ssmClient, event.EC2InstanceID, region, event.Destination)
			return completeLifecycle(ctx, asgClient, event, "CONTINUE")
		}
		// InProgress / Pending — keep polling
	}

	log.Printf("Timed out waiting for SSM node disable — proceeding with CONTINUE")
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
		log.Printf("Instance %s going to warm pool (destination=%q) — keeping slot lock", instanceID, destination)
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
		log.Printf("WARNING: failed to tag instance %s with warm-pool-entry-time: %v", instanceID, err)
	} else {
		log.Printf("Tagged instance %s warm-pool-entry-time=%s", instanceID, entryTime)
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
		log.Printf("WARNING: could not describe instance %s to find slot tag: %v", instanceID, err)
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
		log.Printf("Instance %s has no elastic-gcs:slot tag — slot lock not released", instanceID)
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
		log.Printf("Slot lock %s not found (already released or never set) — nothing to do", paramName)
		return
	}
	lockOwner := aws.ToString(lockOut.Parameter.Value)
	if lockOwner != instanceID {
		log.Printf("Slot lock %s is owned by %s, not %s — NOT releasing", paramName, lockOwner, instanceID)
		return
	}

	_, err = ssmClient.DeleteParameter(ctx, &ssm.DeleteParameterInput{
		Name: aws.String(paramName),
	})
	if err != nil {
		log.Printf("WARNING: failed to release slot lock %s: %v", paramName, err)
	} else {
		log.Printf("Released slot lock %s for terminated instance %s", paramName, instanceID)
	}
}

// completeLifecycle signals the ASG lifecycle hook with the given result (CONTINUE or ABANDON).
func completeLifecycle(ctx context.Context, asgClient *autoscaling.Client, event LifecycleEvent, result string) error {
	log.Printf("Completing lifecycle action: hook=%s instance=%s result=%s",
		event.LifecycleHookName, event.EC2InstanceID, result)

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
		log.Printf("CompleteLifecycleAction warning (may already be complete): %v", err)
	}
	return nil
}

func main() {
	region := os.Getenv("AWS_REGION")
	log.Printf("Elastic GCS Lifecycle Lambda starting in %s", region)
	lambda.Start(handler)
}
