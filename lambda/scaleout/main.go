package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/logger"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/warmpool"
)

// ScaleOutEvent is the payload for manual or alarm-triggered scale-out.
type ScaleOutEvent struct {
	Source    string `json:"source"`     // "cloudwatch", "globus_webhook", "manual"
	Reason    string `json:"reason"`     // Human-readable reason
	TargetDTN int    `json:"target_dtn"` // Specific DTN count to target (0 = auto +1)

	// Ignore is set for CloudWatch alarm notifications that must not scale out:
	// alarms other than the deployment's scale-out alarms, or OK transitions (#27).
	Ignore bool `json:"-"`
}

// alarmMessage is the subset of a CloudWatch alarm SNS notification we check.
type alarmMessage struct {
	AlarmName     string `json:"AlarmName"`
	NewStateValue string `json:"NewStateValue"`
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
// string in Records[0].Sns.Message. allowedAlarms lists the alarm names that
// may scale out; any other alarm, or a non-ALARM state, is ignored.
func parseEvent(rawEvent json.RawMessage, allowedAlarms map[string]bool) ScaleOutEvent {
	var event ScaleOutEvent

	// Try SNS envelope first
	var env snsEnvelope
	if err := json.Unmarshal(rawEvent, &env); err == nil &&
		len(env.Records) > 0 && env.Records[0].SNS.Message != "" {
		msg := []byte(env.Records[0].SNS.Message)
		var alarm alarmMessage
		if json.Unmarshal(msg, &alarm) == nil && alarm.AlarmName != "" {
			event.Source = "cloudwatch"
			event.Reason = "alarm " + alarm.AlarmName + " -> " + alarm.NewStateValue
			event.Ignore = !allowedAlarms[alarm.AlarmName] || alarm.NewStateValue != "ALARM"
			return event
		}
		// Inner message is another JSON string — parse it
		if err := json.Unmarshal(msg, &event); err != nil {
			event.Source = "sns"
			event.Reason = "unparseable message"
			event.Ignore = true
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

	event := parseEvent(rawEvent, alarmSet(os.Getenv("SCALE_OUT_ALARMS")))
	if event.Ignore {
		logger.Info("ignoring notification", "source", event.Source, "reason", event.Reason)
		return nil
	}
	logger.Info("scale-out triggered",
		"source", event.Source,
		"reason", event.Reason,
		"region", region,
		"asg", asgName,
	)

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
		logger.Info("already at desired capacity, no action",
			"desired", currentDesired,
			"max", maxDTNs,
		)
		return nil
	}

	logger.Info("setting ASG desired capacity",
		"from", currentDesired,
		"to", newDesired,
		"max", maxDTNs,
	)
	_, err = asgClient.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: aws.String(asgName),
		DesiredCapacity:      aws.Int32(int32(newDesired)),
		HonorCooldown:        aws.Bool(false), // Scale out immediately, don't wait for cooldown
	})
	if err != nil {
		return fmt.Errorf("SetDesiredCapacity failed: %w", err)
	}

	logger.Info("scale-out complete", "asg", asgName, "desired", newDesired)
	restoreWarmPool(ctx, asgClient, asgName)
	return nil
}

func main() {
	region := os.Getenv("AWS_REGION")
	logger.Init("elastic-gcs-scaleout", region)
	lambda.Start(handler)
}

// restoreWarmPool puts the warm pool back if cache eviction removed it (#33).
// This scale-out is a cold launch; later ones resume from the restored pool.
func restoreWarmPool(ctx context.Context, api warmpool.API, asgName string) {
	size, _ := strconv.Atoi(os.Getenv("WARM_POOL_SIZE"))
	if made, err := warmpool.Ensure(ctx, api, asgName, size); err != nil {
		logger.Warn("could not restore warm pool", "error", err.Error())
	} else if made {
		logger.Info("warm pool restored after eviction", "size", size)
	}
}

// alarmSet parses a comma-separated list of alarm names.
func alarmSet(csv string) map[string]bool {
	set := map[string]bool{}
	for _, n := range strings.Split(csv, ",") {
		if n = strings.TrimSpace(n); n != "" {
			set[n] = true
		}
	}
	return set
}
