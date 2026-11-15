package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/logger"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
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

	logger.Info("scale-in evaluation triggered", "region", region, "asg", asgName)

	// Unwrap SNS envelope if present
	var env snsEnvelope
	if err := json.Unmarshal(rawEvent, &env); err == nil &&
		len(env.Records) > 0 && env.Records[0].SNS.Message != "" {
		logger.Info("unwrapped SNS envelope from CloudWatch alarm")
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
			{Name: aws.String("Region"), Value: aws.String(dep.Region)},
			{Name: aws.String("Deployment"), Value: aws.String(dep.Name)},
		},
	})
	if err != nil {
		logger.Warn("GetMetricStatistics failed, proceeding with scale-in", "error", err.Error())
	} else if len(stats.Datapoints) > 0 && aws.ToFloat64(stats.Datapoints[0].Maximum) > 0 {
		logger.Info("active transfers detected, skipping scale-in",
			"transfers", aws.ToFloat64(stats.Datapoints[0].Maximum),
		)
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
		logger.Info("already at desired capacity 0, no scale-in needed")
		return nil
	}

	newDesired := currentDesired - 1

	// Never leave a running endpoint with zero active nodes (#71). Before the
	// last DTN goes, make sure a running control node is serving: if it was
	// taken out of the data path, re-enable it. A stopped control node means
	// the endpoint is meant to be offline, so scale-in proceeds.
	if newDesired == 0 {
		if err := ensureControlNodeServing(ctx, cfg); err != nil {
			logger.Warn("control node not serving; keeping the last DTN", "error", err.Error())
			return nil
		}
	}

	logger.Info("scaling in", "from", currentDesired, "to", newDesired)

	// SetDesiredCapacity with retry on ScalingActivityInProgress.
	//
	// The ASG rejects capacity changes while a scaling activity is in progress
	// (e.g. the scale-out lifecycle hook completing, or warm pool replenishment
	// finishing its health-check grace period — typically 5 min after InService).
	// Rather than returning an error and relying on SNS retry timing, we retry
	// internally so the scale-in completes as soon as the activity clears.
	//
	// HonorCooldown=false: the CloudWatch alarm (15-min zero-demand window, or
	// forced in tests) is the appropriate gate for scale-in. Adding the ASG
	// cooldown period on top is redundant and causes spurious failures.
	const (
		maxRetries    = 16
		retryInterval = 30 * time.Second
	)
	for attempt := 1; attempt <= maxRetries; attempt++ {
		_, err = asgClient.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
			AutoScalingGroupName: aws.String(asgName),
			DesiredCapacity:      aws.Int32(int32(newDesired)),
			HonorCooldown:        aws.Bool(false),
		})
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "ScalingActivityInProgress") && attempt < maxRetries {
			logger.Info("scaling activity in progress, retrying",
				"attempt", attempt, "max", maxRetries, "backoff_sec", int(retryInterval.Seconds()))
			time.Sleep(retryInterval)
			continue
		}
		return fmt.Errorf("SetDesiredCapacity failed: %w", err)
	}

	logger.Info("scale-in complete", "asg", asgName, "from", currentDesired, "to", newDesired)
	return nil
}

// ensureControlNodeServing enables the control node's GCS node (and waits for
// it to appear in endpoint DNS) if the control node is running. It returns nil
// if the control node is stopped: then there is nothing to keep serving.
func ensureControlNodeServing(ctx context.Context, cfg aws.Config) error {
	ssmClient := ssm.NewFromConfig(cfg)
	param, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(dep.Param(names.ParamControlNodeID)),
	})
	if err != nil {
		return fmt.Errorf("get control node ID: %w", err)
	}
	instanceID := aws.ToString(param.Parameter.Value)

	desc, err := ec2.NewFromConfig(cfg).DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil || len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		return fmt.Errorf("describe control node %s: %v", instanceID, err)
	}
	if state := string(desc.Reservations[0].Instances[0].State.Name); state != "running" {
		logger.Info("control node not running; endpoint goes offline with the pool", "state", state)
		return nil
	}

	// control_node_boot.sh (0.12+) enables and waits for DNS; older control
	// nodes fall back to a plain local enable.
	const script = `if [ -x /opt/elastic-gcs/scripts/control_node_boot.sh ]; then /opt/elastic-gcs/scripts/control_node_boot.sh start; ` +
		`else globus-connect-server node enable; chown gcsweb:gcsweb /var/lib/globus-connect-server/gcs-manager/locks/subscription_pause 2>/dev/null; true; fi`
	out, err := ssmClient.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:    []string{instanceID},
		DocumentName:   aws.String("AWS-RunShellScript"),
		Parameters:     map[string][]string{"commands": {script}, "executionTimeout": {"300"}},
		TimeoutSeconds: aws.Int32(60),
	})
	if err != nil {
		return fmt.Errorf("SendCommand: %w", err)
	}
	cmdID := aws.ToString(out.Command.CommandId)
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		inv, err := ssmClient.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId: aws.String(cmdID), InstanceId: aws.String(instanceID),
		})
		if err != nil {
			continue // invocation not registered yet
		}
		switch inv.Status {
		case ssmtypes.CommandInvocationStatusSuccess:
			logger.Info("control node serving", "instance", instanceID, "output", aws.ToString(inv.StandardOutputContent))
			return nil
		case ssmtypes.CommandInvocationStatusFailed, ssmtypes.CommandInvocationStatusTimedOut,
			ssmtypes.CommandInvocationStatusCancelled:
			return fmt.Errorf("enable on %s: %s %s", instanceID, inv.Status, aws.ToString(inv.StandardErrorContent))
		}
	}
	return fmt.Errorf("enable on %s did not finish in time", instanceID)
}

// dep identifies this deployment; set in main from AWS_REGION and DEPLOYMENT.
var dep names.Deployment

func main() {
	region := os.Getenv("AWS_REGION")
	logger.Init("elastic-gcs-scalein", region)
	var err error
	if dep, err = names.FromEnv(); err != nil {
		logger.Error("invalid deployment configuration", "error", err.Error())
		os.Exit(1)
	}
	lambda.Start(handler)
}
