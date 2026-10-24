// Package main implements the GCS version drift detection Lambda.
//
// Triggered every 6 hours by EventBridge. For each running node (control node
// + InService DTNs), it runs `globus-connect-server --version` via SSM and
// collects the version string. It then publishes the metric:
//
//	ElasticGCS/GCSVersionDrift{Region=<region>} = distinct_version_count - 1
//
// A value of 0 means all nodes agree on the same GCS version (healthy).
// A value > 0 means version drift — nodes are running different GCS versions,
// which Globus does not support in a multi-node endpoint.
package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/logger"
)

// versionRe extracts the semver string from globus-connect-server --version output.
// Example output: "GlobusConnectServer 5.4.92"
var versionRe = regexp.MustCompile(`(\d+\.\d+\.\d+)`)

// handler runs the version drift check and publishes the GCSVersionDrift metric.
func handler(ctx context.Context, _ interface{}) error {
	region := os.Getenv("REGION")
	asgName := os.Getenv("ASG_NAME")
	controlNodeIDParam := os.Getenv("CONTROL_NODE_ID_PARAM")

	logger.Info("version check starting", "region", region, "asg", asgName)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	ssmClient := ssm.NewFromConfig(cfg)
	asgClient := autoscaling.NewFromConfig(cfg)
	cwClient := cloudwatch.NewFromConfig(cfg)

	// Resolve control node instance ID.
	paramOut, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(controlNodeIDParam),
	})
	if err != nil {
		return fmt.Errorf("GetParameter %s: %w", controlNodeIDParam, err)
	}
	controlNodeID := aws.ToString(paramOut.Parameter.Value)
	logger.Info("control node resolved", "instance", controlNodeID)

	// Collect InService DTN instances from the ASG.
	// Stopped warm pool instances cannot run SSM commands, so we skip them.
	var instanceIDs []string
	instanceIDs = append(instanceIDs, controlNodeID)

	groups, err := asgClient.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName},
	})
	if err != nil {
		logger.Warn("DescribeAutoScalingGroups failed, checking only control node", "error", err.Error())
	} else if len(groups.AutoScalingGroups) > 0 {
		for _, inst := range groups.AutoScalingGroups[0].Instances {
			if string(inst.LifecycleState) == "InService" {
				instanceIDs = append(instanceIDs, aws.ToString(inst.InstanceId))
			}
		}
	}

	logger.Info("checking GCS versions", "instances", instanceIDs, "count", len(instanceIDs))

	// Query each running instance for its GCS version via SSM.
	versions := make(map[string]string, len(instanceIDs))
	for _, id := range instanceIDs {
		ver, err := getGCSVersion(ctx, ssmClient, id)
		if err != nil {
			logger.Warn("failed to get GCS version",
				"instance", id,
				"error", err.Error(),
			)
			versions[id] = "unknown"
		} else {
			versions[id] = ver
			logger.Info("GCS version retrieved", "instance", id, "version", ver)
		}
	}

	// Count distinct known versions (exclude "unknown" to avoid false drift alerts
	// when an instance is temporarily unreachable).
	distinct := make(map[string]struct{})
	for _, v := range versions {
		if v != "unknown" {
			distinct[v] = struct{}{}
		}
	}

	driftCount := len(distinct) - 1
	if driftCount < 0 {
		driftCount = 0
	}

	logger.Info("version check complete",
		"versions", versions,
		"distinct_versions", len(distinct),
		"drift_count", driftCount,
	)

	if driftCount > 0 {
		logger.Warn("GCS version drift detected — all nodes must run the same GCS version",
			"drift_count", driftCount,
			"versions", versions,
		)
	}

	// Publish GCSVersionDrift metric.
	// 0 = all nodes agree (healthy), >0 = drift (alert).
	_, err = cwClient.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String("ElasticGCS"),
		MetricData: []cwtypes.MetricDatum{
			{
				MetricName: aws.String("GCSVersionDrift"),
				Value:      aws.Float64(float64(driftCount)),
				Unit:       cwtypes.StandardUnitCount,
				Dimensions: []cwtypes.Dimension{
					{Name: aws.String("Region"), Value: aws.String(region)},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("PutMetricData: %w", err)
	}

	logger.Info("GCSVersionDrift metric published", "value", driftCount)
	return nil
}

// getGCSVersion runs `globus-connect-server --version` on the given instance
// via SSM and returns the parsed version string (e.g. "5.4.92").
func getGCSVersion(ctx context.Context, ssmClient *ssm.Client, instanceID string) (string, error) {
	cmdOut, err := ssmClient.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:  []string{instanceID},
		DocumentName: aws.String("AWS-RunShellScript"),
		Parameters: map[string][]string{
			"commands":         {"globus-connect-server --version 2>&1"},
			"executionTimeout": {"30"},
		},
		Comment: aws.String("elastic-gcs: GCS version check"),
	})
	if err != nil {
		return "", fmt.Errorf("SendCommand: %w", err)
	}

	commandID := aws.ToString(cmdOut.Command.CommandId)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)

		inv, err := ssmClient.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(commandID),
			InstanceId: aws.String(instanceID),
		})
		if err != nil {
			continue
		}

		switch inv.Status {
		case ssmtypes.CommandInvocationStatusSuccess,
			ssmtypes.CommandInvocationStatusFailed:
			output := aws.ToString(inv.StandardOutputContent)
			if m := versionRe.FindString(output); m != "" {
				return m, nil
			}
			return "unknown", nil

		case ssmtypes.CommandInvocationStatusTimedOut,
			ssmtypes.CommandInvocationStatusCancelled,
			ssmtypes.CommandInvocationStatusCancelling:
			return "", fmt.Errorf("SSM command in terminal state: %s", inv.Status)
		}
		// Pending / InProgress — keep polling
	}
	return "", fmt.Errorf("timed out waiting for version command on %s", instanceID)
}

func main() {
	region := os.Getenv("AWS_REGION")
	logger.Init("elastic-gcs-version-check", region)
	lambda.Start(handler)
}
