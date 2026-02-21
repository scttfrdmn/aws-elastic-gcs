package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// snsEnvelope wraps an event when CloudWatch alarm triggers through SNS.
type snsEnvelope struct {
	Records []struct {
		SNS struct {
			Message string `json:"Message"`
		} `json:"Sns"`
	} `json:"Records"`
}

// handler processes scale-in decisions by reducing ASG desired capacity.
//
// Triggered by: CloudWatch alarm (ActiveTransfers == 0 for 15 min)
//
// Logic:
//  1. Unwrap SNS envelope if present (CloudWatch routes alarms through SNS)
//  2. Check CloudWatch for recent active transfers — skip if transfers detected
//  3. Get current ASG desired capacity
//  4. If desired > 0, decrement by 1
//  5. ASG moves the instance to warm pool (Warmed:Stopped) via ReuseOnScaleIn
//  6. The lifecycle Lambda handles node disable before the instance stops
func handler(ctx context.Context, rawEvent json.RawMessage) error {
	asgName := os.Getenv("ASG_NAME")
	region := os.Getenv("AWS_REGION")
	metricNamespace := os.Getenv("METRIC_NAMESPACE")
	if metricNamespace == "" {
		metricNamespace = "ElasticGCS"
	}

	log.Printf("Scale-in evaluation triggered: region=%s asg=%s", region, asgName)

	// Unwrap SNS envelope if present
	var env snsEnvelope
	if err := json.Unmarshal(rawEvent, &env); err == nil &&
		len(env.Records) > 0 && env.Records[0].SNS.Message != "" {
		log.Printf("Scale-in: unwrapped SNS envelope from CloudWatch alarm")
	}

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Active transfer guard: check CloudWatch for recent transfers before scaling in.
	// If transfers were active in the last 5 minutes, skip this scale-in event.
	cwClient := cloudwatch.NewFromConfig(cfg)
	stats, err := cwClient.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String(metricNamespace),
		MetricName: aws.String("ActiveTransfers"),
		StartTime:  aws.Time(time.Now().Add(-5 * time.Minute)),
		EndTime:    aws.Time(time.Now()),
		Period:     aws.Int32(300),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticMaximum},
		Dimensions: []cwtypes.Dimension{
			{Name: aws.String("Region"), Value: aws.String(region)},
		},
	})
	if err != nil {
		log.Printf("WARNING: GetMetricStatistics failed: %v — proceeding with scale-in", err)
	} else if len(stats.Datapoints) > 0 && aws.ToFloat64(stats.Datapoints[0].Maximum) > 0 {
		log.Printf("Active transfers detected (%.0f) — skipping scale-in",
			aws.ToFloat64(stats.Datapoints[0].Maximum))
		return nil
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
