package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/logger"
)

func handler(ctx context.Context) error {
	asgNameStr := os.Getenv("ASG_NAME")
	region := os.Getenv("AWS_REGION")
	controlNodeIDParam := os.Getenv("CONTROL_NODE_ID_PARAM")
	metricNamespace := os.Getenv("METRIC_NAMESPACE")
	if metricNamespace == "" {
		metricNamespace = "ElasticGCS"
	}

	idleMinutes := 30
	if v := os.Getenv("IDLE_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			idleMinutes = n
		}
	}
	minUptimeMinutes := 30
	if v := os.Getenv("MIN_UPTIME_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			minUptimeMinutes = n
		}
	}

	logger.Info("idle shutdown check", "region", region,
		"idle_minutes", idleMinutes, "min_uptime_minutes", minUptimeMinutes)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("AWS config: %w", err)
	}

	ssmClient := ssm.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
	asgClient := autoscaling.NewFromConfig(cfg)
	cwClient := cloudwatch.NewFromConfig(cfg)

	// 1. Resolve control node instance ID.
	param, err := ssmClient.GetParameter(ctx,
		&ssm.GetParameterInput{Name: aws.String(controlNodeIDParam)})
	if err != nil {
		return fmt.Errorf("get control node ID: %w", err)
	}
	instanceID := aws.ToString(param.Parameter.Value)

	// 2. Check control node EC2 state — only proceed if running.
	desc, err := ec2Client.DescribeInstances(ctx,
		&ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil || len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		logger.Info("control node not found", "instance", instanceID)
		return nil
	}
	inst := desc.Reservations[0].Instances[0]
	if string(inst.State.Name) != "running" {
		logger.Info("control node not running, nothing to do",
			"instance", instanceID, "state", string(inst.State.Name))
		return nil
	}

	// 3. Minimum uptime guard: prevents thrash if the waker and idle_shutdown
	//    fire close together (e.g. control node started, idle check fires
	//    before any transfer begins).
	if inst.LaunchTime != nil {
		uptime := time.Since(aws.ToTime(inst.LaunchTime))
		if uptime < time.Duration(minUptimeMinutes)*time.Minute {
			logger.Info("control node within minimum uptime window, skipping",
				"uptime_minutes", int(uptime.Minutes()),
				"min_uptime_minutes", minUptimeMinutes)
			return nil
		}
	}

	// 4. ASG desired must be 0 and no instance InService.
	groups, err := asgClient.DescribeAutoScalingGroups(ctx,
		&autoscaling.DescribeAutoScalingGroupsInput{
			AutoScalingGroupNames: []string{asgNameStr},
		})
	if err != nil || len(groups.AutoScalingGroups) == 0 {
		return fmt.Errorf("DescribeAutoScalingGroups: %w", err)
	}
	asg := groups.AutoScalingGroups[0]
	if aws.ToInt32(asg.DesiredCapacity) != 0 {
		logger.Info("ASG desired > 0, skipping", "desired", aws.ToInt32(asg.DesiredCapacity))
		return nil
	}
	for _, i := range asg.Instances {
		if string(i.LifecycleState) == "InService" {
			logger.Info("DTN still InService, skipping", "instance", aws.ToString(i.InstanceId))
			return nil
		}
	}

	// 5. Warm pool — all instances must be in a stable stopped state.
	//    A Warmed:Pending or Warmed:Running instance means a scale-out is in
	//    progress and we must not stop the control node.
	wp, err := asgClient.DescribeWarmPool(ctx,
		&autoscaling.DescribeWarmPoolInput{AutoScalingGroupName: aws.String(asgNameStr)})
	if err != nil {
		logger.Warn("DescribeWarmPool failed, skipping", "error", err.Error())
		return nil
	}
	for _, wi := range wp.Instances {
		ls := string(wi.LifecycleState)
		if ls != "Warmed:Stopped" && ls != "Warmed:Terminated" {
			logger.Info("warm pool instance not stable, skipping",
				"instance", aws.ToString(wi.InstanceId), "state", ls)
			return nil
		}
	}

	// 6. ActiveTransfers must have been 0 for the full idle window.
	stats, err := cwClient.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String(metricNamespace),
		MetricName: aws.String("ActiveTransfers"),
		StartTime:  aws.Time(time.Now().Add(-time.Duration(idleMinutes) * time.Minute)),
		EndTime:    aws.Time(time.Now()),
		Period:     aws.Int32(int32(idleMinutes * 60)),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticMaximum},
		Dimensions: []cwtypes.Dimension{
			{Name: aws.String("Region"), Value: aws.String(region)},
		},
	})
	if err != nil {
		logger.Warn("GetMetricStatistics failed, skipping", "error", err.Error())
		return nil
	}
	if len(stats.Datapoints) > 0 && aws.ToFloat64(stats.Datapoints[0].Maximum) > 0 {
		logger.Info("active transfers in idle window, skipping",
			"transfers", aws.ToFloat64(stats.Datapoints[0].Maximum))
		return nil
	}

	// All conditions met — stop the control node.
	logger.Info("idle conditions met, stopping control node",
		"instance", instanceID, "idle_minutes", idleMinutes)
	if _, err := ec2Client.StopInstances(ctx,
		&ec2.StopInstancesInput{InstanceIds: []string{instanceID}}); err != nil {
		return fmt.Errorf("StopInstances: %w", err)
	}
	logger.Info("control node stop initiated", "instance", instanceID)
	return nil
}

func main() {
	region := os.Getenv("AWS_REGION")
	logger.Init("elastic-gcs-idle-shutdown", region)
	lambda.Start(handler)
}
