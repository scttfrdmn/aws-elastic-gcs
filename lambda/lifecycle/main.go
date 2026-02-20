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
}

// Handler processes ASG lifecycle hook events for DTN nodes.
//
// Scale-out flow (LAUNCHING):
//   The boot script (dtn_boot.sh) handles its own lifecycle completion via
//   CompleteLifecycleAction when the GCS node becomes active. This Lambda is
//   a no-op for LAUNCHING events since the boot script signals first.
//
// Scale-in flow (TERMINATING):
//   1. Receives the lifecycle hook event before the instance stops
//   2. Sends SSM Run Command to run: globus-connect-server node disable
//   3. Waits for command completion (up to 90 seconds)
//   4. Signals CONTINUE — ASG then stops/terminates the instance
//   5. systemd ExecStop also runs dtn_shutdown.sh (idempotent second disable)
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

	switch lifecycle.LifecycleTransition {
	case "autoscaling:EC2_INSTANCE_TERMINATING":
		return handleScaleIn(ctx, ssmClient, asgClient, lifecycle)
	case "autoscaling:EC2_INSTANCE_LAUNCHING":
		// Boot script handles its own completion. Lambda is a no-op here.
		log.Printf("Scale-out LAUNCHING: boot script handles lifecycle, no Lambda action needed")
		return nil
	default:
		log.Printf("Unhandled lifecycle transition: %s — completing with CONTINUE", lifecycle.LifecycleTransition)
		return completeLifecycle(ctx, asgClient, lifecycle, "CONTINUE")
	}
}

// handleScaleIn disables the GCS node via SSM before allowing the instance to stop.
// Preventing a node from stopping while still registered in GCS DNS avoids
// the "endpoint unreliable" state documented in the GCS docs.
func handleScaleIn(ctx context.Context, ssmClient *ssm.Client, asgClient *autoscaling.Client, event LifecycleEvent) error {
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
			return completeLifecycle(ctx, asgClient, event, "CONTINUE")

		case ssmtypes.CommandInvocationStatusTimedOut,
			ssmtypes.CommandInvocationStatusCancelled,
			ssmtypes.CommandInvocationStatusCancelling:
			log.Printf("SSM command in terminal error state: %s — proceeding with CONTINUE", inv.Status)
			return completeLifecycle(ctx, asgClient, event, "CONTINUE")
		}
		// InProgress / Pending — keep polling
	}

	log.Printf("Timed out waiting for SSM node disable — proceeding with CONTINUE")
	return completeLifecycle(ctx, asgClient, event, "CONTINUE")
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
