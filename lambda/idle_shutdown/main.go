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

	"github.com/scttfrdmn/aws-elastic-gcs/internal/idle"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/logger"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/scaling"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/ssmexec"
)

func handler(ctx context.Context) error {
	asgNameStr := os.Getenv("ASG_NAME")
	region := os.Getenv("AWS_REGION")
	controlNodeIDParam := os.Getenv("CONTROL_NODE_ID_PARAM")
	metricNamespace := os.Getenv("METRIC_NAMESPACE")
	if metricNamespace == "" {
		metricNamespace = names.MetricNamespace
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

	// Gather the facts, then decide with idle.ShouldStop (#52).
	in := idle.Inputs{
		Running:   inst.State != nil && string(inst.State.Name) == "running",
		MinUptime: time.Duration(minUptimeMinutes) * time.Minute,
	}
	if inst.LaunchTime != nil {
		in.Uptime = time.Since(aws.ToTime(inst.LaunchTime))
	}
	if in.Running {
		groups, err := asgClient.DescribeAutoScalingGroups(ctx,
			&autoscaling.DescribeAutoScalingGroupsInput{AutoScalingGroupNames: []string{asgNameStr}})
		if err != nil || len(groups.AutoScalingGroups) == 0 {
			return fmt.Errorf("DescribeAutoScalingGroups: %w", err)
		}
		asg := groups.AutoScalingGroups[0]
		in.Desired = int(aws.ToInt32(asg.DesiredCapacity))
		for _, i := range asg.Instances {
			if string(i.LifecycleState) == "InService" {
				in.InService++
			}
		}
		wp, err := asgClient.DescribeWarmPool(ctx,
			&autoscaling.DescribeWarmPoolInput{AutoScalingGroupName: aws.String(asgNameStr)})
		if err != nil {
			logger.Warn("DescribeWarmPool failed, skipping", "error", err.Error())
			return nil
		}
		for _, wi := range wp.Instances {
			in.WarmStates = append(in.WarmStates, string(wi.LifecycleState))
		}
		stats, err := cwClient.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
			Namespace:  aws.String(metricNamespace),
			MetricName: aws.String("ActiveTransfers"),
			StartTime:  aws.Time(time.Now().Add(-time.Duration(idleMinutes) * time.Minute)),
			EndTime:    aws.Time(time.Now()),
			Period:     aws.Int32(60),
			Statistics: []cwtypes.Statistic{cwtypes.StatisticMaximum},
			Dimensions: []cwtypes.Dimension{
				{Name: aws.String("Region"), Value: aws.String(dep.Region)},
				{Name: aws.String("Deployment"), Value: aws.String(dep.Name)},
			},
		})
		if err != nil {
			logger.Warn("GetMetricStatistics failed, skipping", "error", err.Error())
			return nil
		}
		in.ActiveTransfers = scaling.Maxima(stats.Datapoints)
	}
	stop, why := idle.ShouldStop(in)
	if !stop {
		logger.Info("not stopping control node", "instance", instanceID, "reason", why)
		return nil
	}

	logger.Info("idle conditions met, disabling control node before stop",
		"instance", instanceID, "idle_minutes", idleMinutes)

	// Disable the GCS node first (#30), so a stopped control node doesn't leave
	// a dead IP in endpoint DNS. The node's elastic-gcs-control.service does the
	// same on shutdown; this makes it explicit and observable. If the disable
	// fails, don't stop: the next run retries.
	if err := disableNode(ctx, ssmClient, instanceID); err != nil {
		logger.Warn("node disable failed, not stopping", "instance", instanceID, "error", err.Error())
		return nil
	}

	if _, err := ec2Client.StopInstances(ctx,
		&ec2.StopInstancesInput{InstanceIds: []string{instanceID}}); err != nil {
		return fmt.Errorf("StopInstances: %w", err)
	}
	logger.Info("control node stop initiated", "instance", instanceID)
	return nil
}

// disableNode runs `node disable` on the control node via SSM, waits for its IP
// to leave endpoint DNS (bounded), and waits for the command to finish.
func disableNode(ctx context.Context, client *ssm.Client, instanceID string) error {
	res, err := ssmexec.RunOK(ctx, client, instanceID, "/opt/elastic-gcs/scripts/control_node_boot.sh stop",
		ssmexec.Options{Wait: 4 * time.Minute, Poll: 5 * time.Second, Comment: "elastic-gcs: disable control node before idle stop"})
	if err != nil {
		return err
	}
	logger.Info("control node disabled", "output", res.Stdout)
	return nil
}

// dep identifies this deployment; set in main from AWS_REGION and DEPLOYMENT.
var dep names.Deployment

func main() {
	region := os.Getenv("AWS_REGION")
	logger.Init("elastic-gcs-idle-shutdown", region)
	var err error
	if dep, err = names.FromEnv(); err != nil {
		logger.Error("invalid deployment configuration", "error", err.Error())
		os.Exit(1)
	}
	lambda.Start(handler)
}
