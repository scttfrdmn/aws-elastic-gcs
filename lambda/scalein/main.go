package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
)

// handler processes scale-in decisions by reducing ASG desired capacity.
//
// Triggered by: CloudWatch alarm (sustained low network throughput across all DTNs)
//
// Logic:
//   1. Get current ASG desired capacity
//   2. If desired > 0, decrement by 1
//   3. ASG moves the instance to warm pool (Warmed:Stopped) via ReuseOnScaleIn
//   4. The lifecycle Lambda handles node disable before the instance stops
func handler(ctx context.Context, rawEvent json.RawMessage) error {
	asgName := os.Getenv("ASG_NAME")
	region := os.Getenv("AWS_REGION")

	log.Printf("Scale-in evaluation triggered: region=%s asg=%s", region, asgName)

	// Parse event for logging — may be SNS or direct
	var event map[string]interface{}
	if err := json.Unmarshal(rawEvent, &event); err == nil {
		if src, ok := event["source"].(string); ok {
			log.Printf("Scale-in source: %s", src)
		}
	}

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

	if currentDesired <= 0 {
		log.Printf("Already at desired capacity 0 — no scale-in needed")
		return nil
	}

	newDesired := currentDesired - 1
	log.Printf("Scaling in: setting ASG desired capacity %d → %d", currentDesired, newDesired)

	_, err = asgClient.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: aws.String(asgName),
		DesiredCapacity:      aws.Int32(int32(newDesired)),
		HonorCooldown:        aws.Bool(true), // Respect cooldown on scale-in
	})
	if err != nil {
		return fmt.Errorf("SetDesiredCapacity failed: %w", err)
	}

	log.Printf("Scale-in complete: ASG=%s desired=%d → %d", asgName, currentDesired, newDesired)
	return nil
}

func main() {
	log.Printf("Elastic GCS Scale-In Lambda starting")
	lambda.Start(handler)
}
