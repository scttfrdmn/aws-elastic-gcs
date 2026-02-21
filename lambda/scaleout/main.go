package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
)

// ScaleOutEvent is the payload for manual or alarm-triggered scale-out.
type ScaleOutEvent struct {
	Source    string `json:"source"`     // "cloudwatch", "globus_webhook", "manual"
	Reason    string `json:"reason"`     // Human-readable reason
	TargetDTN int    `json:"target_dtn"` // Specific DTN count to target (0 = auto +1)
}

// snsEnvelope wraps an event when CloudWatch alarm triggers through SNS.
type snsEnvelope struct {
	Records []struct {
		SNS struct {
			Message string `json:"Message"`
		} `json:"Sns"`
	} `json:"Records"`
}

// parseEvent unwraps SNS envelope if present, then parses ScaleOutEvent.
// CloudWatch alarms route through SNS, wrapping the inner message as a JSON
// string in Records[0].Sns.Message.
func parseEvent(rawEvent json.RawMessage) ScaleOutEvent {
	var event ScaleOutEvent

	// Try SNS envelope first
	var env snsEnvelope
	if err := json.Unmarshal(rawEvent, &env); err == nil &&
		len(env.Records) > 0 && env.Records[0].SNS.Message != "" {
		// Inner message is another JSON string — parse it
		if err := json.Unmarshal([]byte(env.Records[0].SNS.Message), &event); err != nil {
			// CloudWatch alarm SNS payloads don't have our fields — that's OK
			event.Source = "cloudwatch"
			event.Reason = "alarm triggered"
		}
		return event
	}

	// Fall back to direct parse (manual invocation or webhook)
	if err := json.Unmarshal(rawEvent, &event); err != nil {
		event.Source = "unknown"
		event.Reason = "parse error"
	}
	return event
}

// handler processes scale-out triggers by increasing ASG desired capacity.
//
// Logic:
//  1. Get current ASG desired capacity
//  2. If target specified, use that; otherwise increment by 1
//  3. Cap at MAX_DTNS
//  4. Call SetDesiredCapacity — ASG resumes a warm pool instance
func handler(ctx context.Context, rawEvent json.RawMessage) error {
	asgName := os.Getenv("ASG_NAME")
	region := os.Getenv("AWS_REGION")
	maxDTNs, _ := strconv.Atoi(os.Getenv("MAX_DTNS"))
	if maxDTNs == 0 {
		maxDTNs = 3
	}

	event := parseEvent(rawEvent)
	log.Printf("Scale-out triggered: source=%s reason=%s region=%s asg=%s",
		event.Source, event.Reason, region, asgName)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	asgClient := autoscaling.NewFromConfig(cfg)

	// Get current ASG state
	groups, err := asgClient.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName},
	})
	if err != nil {
		return fmt.Errorf("DescribeAutoScalingGroups failed: %w", err)
	}
	if len(groups.AutoScalingGroups) == 0 {
		return fmt.Errorf("ASG not found: %s", asgName)
	}

	asg := groups.AutoScalingGroups[0]
	currentDesired := int(aws.ToInt32(asg.DesiredCapacity))

	// Calculate new desired capacity
	newDesired := currentDesired + 1
	if event.TargetDTN > 0 {
		newDesired = event.TargetDTN
	}
	if newDesired > maxDTNs {
		newDesired = maxDTNs
	}

	if newDesired <= currentDesired {
		log.Printf("Already at desired capacity %d (max %d) — no action", currentDesired, maxDTNs)
		return nil
	}

	log.Printf("Setting ASG desired capacity: %d → %d (max %d)", currentDesired, newDesired, maxDTNs)
	_, err = asgClient.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: aws.String(asgName),
		DesiredCapacity:      aws.Int32(int32(newDesired)),
		HonorCooldown:        aws.Bool(false), // Scale out immediately, don't wait for cooldown
	})
	if err != nil {
		return fmt.Errorf("SetDesiredCapacity failed: %w", err)
	}

	log.Printf("Scale-out complete: ASG=%s desired=%d", asgName, newDesired)
	return nil
}

func main() {
	log.Printf("Elastic GCS Scale-Out Lambda starting")
	lambda.Start(handler)
}
