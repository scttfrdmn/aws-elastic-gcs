package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
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

	"github.com/scttfrdmn/aws-elastic-gcs/internal/logger"
)

// WakeRequest is the body for both direct Lambda invocations and HTTP Function URL calls.
type WakeRequest struct {
	ScaleOut bool `json:"scale_out"`
}

// WakeResponse is returned on success.
type WakeResponse struct {
	Status            string `json:"status"`
	ElapsedSeconds    int    `json:"elapsed_seconds"`
	WarmPoolPopulated bool   `json:"warm_pool_populated"`
	ScaleOutTriggered bool   `json:"scale_out_triggered,omitempty"`
	Message           string `json:"message,omitempty"`
}

var (
	asgNameEnv         = os.Getenv("ASG_NAME")
	region             = os.Getenv("AWS_REGION")
	controlNodeIDParam = os.Getenv("CONTROL_NODE_ID_PARAM")
)

// handler dispatches between Lambda Function URL (HTTP) and direct Lambda invocation.
// Authentication is enforced at the Lambda service level via AWS_IAM — callers must
// present SigV4-signed requests regardless of whether they use the Function URL or
// the Lambda Invoke API. No application-level token check is needed.
func handler(ctx context.Context, rawEvent json.RawMessage) (interface{}, error) {
	start := time.Now()

	// Detect HTTP invocation via Lambda Function URL.
	// LambdaFunctionURLRequest has requestContext.http.method set; direct
	// invocations do not, so the zero value leaves Method == "".
	var httpReq events.LambdaFunctionURLRequest
	isHTTP := json.Unmarshal(rawEvent, &httpReq) == nil &&
		httpReq.RequestContext.HTTP.Method != ""

	var req WakeRequest
	if isHTTP {
		json.Unmarshal([]byte(httpReq.Body), &req) //nolint:errcheck — empty body is fine
	} else {
		json.Unmarshal(rawEvent, &req) //nolint:errcheck
	}

	resp, err := wake(ctx, req, start)
	if err != nil {
		logger.Error("wake failed", "error", err.Error())
		if isHTTP {
			body, _ := json.Marshal(map[string]string{"error": err.Error()})
			return events.LambdaFunctionURLResponse{
				StatusCode: 500,
				Body:       string(body),
				Headers:    map[string]string{"Content-Type": "application/json"},
			}, nil
		}
		return nil, err
	}

	if isHTTP {
		body, _ := json.Marshal(resp)
		return events.LambdaFunctionURLResponse{
			StatusCode: 200,
			Body:       string(body),
			Headers:    map[string]string{"Content-Type": "application/json"},
		}, nil
	}
	return resp, nil
}

func wake(ctx context.Context, req WakeRequest, start time.Time) (*WakeResponse, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("AWS config: %w", err)
	}

	ssmClient := ssm.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
	asgClient := autoscaling.NewFromConfig(cfg)

	// Resolve control node instance ID from SSM.
	param, err := ssmClient.GetParameter(ctx,
		&ssm.GetParameterInput{Name: aws.String(controlNodeIDParam)})
	if err != nil {
		return nil, fmt.Errorf("get control node ID: %w", err)
	}
	instanceID := aws.ToString(param.Parameter.Value)
	logger.Info("wake requested", "instance", instanceID, "scale_out", req.ScaleOut)

	// Describe instance to determine current state.
	desc, err := ec2Client.DescribeInstances(ctx,
		&ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil || len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
		return nil, fmt.Errorf("describe control node: %w", err)
	}
	inst := desc.Reservations[0].Instances[0]
	state := inst.State.Name

	// Report whether the warm pool is populated (fast vs cold DTN path).
	warmPopulated := false
	wp, _ := asgClient.DescribeWarmPool(ctx,
		&autoscaling.DescribeWarmPoolInput{AutoScalingGroupName: aws.String(asgNameEnv)})
	if wp != nil && len(wp.Instances) > 0 {
		warmPopulated = true
	}

	if state == ec2types.InstanceStateNameRunning {
		logger.Info("control node already running, verifying GCS Manager health", "instance", instanceID)
		if err := waitForGCSManager(ctx, ssmClient, instanceID); err != nil {
			return nil, fmt.Errorf("GCS Manager health check: %w", err)
		}

		// Even when the control node is already running, check if DTNs need to
		// be scaled out. This handles the case where a user submits a transfer
		// while the control node is up but desired=0 (no DTNs active).
		scaleOutTriggered := false
		if req.ScaleOut {
			asgDesc, asgErr := asgClient.DescribeAutoScalingGroups(ctx,
				&autoscaling.DescribeAutoScalingGroupsInput{
					AutoScalingGroupNames: []string{asgNameEnv},
				})
			if asgErr == nil && len(asgDesc.AutoScalingGroups) > 0 &&
				aws.ToInt32(asgDesc.AutoScalingGroups[0].DesiredCapacity) == 0 {
				_, serr := asgClient.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
					AutoScalingGroupName: aws.String(asgNameEnv),
					DesiredCapacity:      aws.Int32(1),
					HonorCooldown:        aws.Bool(false),
				})
				if serr != nil {
					logger.Warn("scale-out trigger failed", "error", serr.Error())
				} else {
					scaleOutTriggered = true
					logger.Info("scale-out triggered (control node already running)", "desired", 1)
				}
			}
		}

		elapsed := int(time.Since(start).Seconds())
		return &WakeResponse{
			Status:            "ready",
			ElapsedSeconds:    elapsed,
			WarmPoolPopulated: warmPopulated,
			ScaleOutTriggered: scaleOutTriggered,
			Message:           "control node was already running",
		}, nil
	}

	if state != ec2types.InstanceStateNameStopped {
		return nil, fmt.Errorf("control node in unexpected state %q (expected stopped or running)", state)
	}

	// Start the instance.
	logger.Info("starting control node", "instance", instanceID)
	if _, err := ec2Client.StartInstances(ctx,
		&ec2.StartInstancesInput{InstanceIds: []string{instanceID}}); err != nil {
		return nil, fmt.Errorf("StartInstances: %w", err)
	}

	// If scale_out is requested, trigger it immediately in parallel.
	// The DTN can begin resuming from the warm pool while the control node boots.
	// The lifecycle Lambda's node-enable step waits for GCS Manager to be reachable,
	// so there is no race condition: the DTN completes scale-out only after the
	// control node is healthy.
	scaleOutTriggered := false
	if req.ScaleOut {
		_, serr := asgClient.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
			AutoScalingGroupName: aws.String(asgNameEnv),
			DesiredCapacity:      aws.Int32(1),
			HonorCooldown:        aws.Bool(false),
		})
		if serr != nil {
			logger.Warn("scale-out trigger failed", "error", serr.Error())
		} else {
			scaleOutTriggered = true
			logger.Info("scale-out triggered concurrently with control node boot", "desired", 1)
		}
	}

	// Wait for EC2 running + SSM agent online.
	if err := waitForSSMAgent(ctx, ec2Client, ssmClient, instanceID); err != nil {
		return nil, fmt.Errorf("waiting for SSM agent: %w", err)
	}

	// Wait for GCS Manager service to become active.
	if err := waitForGCSManager(ctx, ssmClient, instanceID); err != nil {
		return nil, fmt.Errorf("waiting for GCS Manager: %w", err)
	}

	elapsed := int(time.Since(start).Seconds())
	logger.Info("control node ready", "instance", instanceID, "elapsed_seconds", elapsed)
	return &WakeResponse{
		Status:            "ready",
		ElapsedSeconds:    elapsed,
		WarmPoolPopulated: warmPopulated,
		ScaleOutTriggered: scaleOutTriggered,
	}, nil
}

// waitForSSMAgent polls until the instance is running and its SSM agent reports Online.
// Timeout: 3 minutes.
func waitForSSMAgent(ctx context.Context, ec2Client *ec2.Client, ssmClient *ssm.Client, instanceID string) error {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)

		// Check EC2 state first.
		desc, err := ec2Client.DescribeInstances(ctx,
			&ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
		if err != nil || len(desc.Reservations) == 0 || len(desc.Reservations[0].Instances) == 0 {
			continue
		}
		if desc.Reservations[0].Instances[0].State.Name != ec2types.InstanceStateNameRunning {
			continue
		}

		// Check SSM agent ping status.
		info, err := ssmClient.DescribeInstanceInformation(ctx,
			&ssm.DescribeInstanceInformationInput{
				Filters: []ssmtypes.InstanceInformationStringFilter{
					{Key: aws.String("InstanceIds"), Values: []string{instanceID}},
				},
			})
		if err != nil || len(info.InstanceInformationList) == 0 {
			continue
		}
		if info.InstanceInformationList[0].PingStatus == ssmtypes.PingStatusOnline {
			logger.Info("SSM agent online", "instance", instanceID)
			return nil
		}
	}
	return fmt.Errorf("timed out waiting for SSM agent on %s", instanceID)
}

// waitForGCSManager polls via SSM RunCommand until globus-connect-server-manager is active.
// Timeout: 2 minutes.
func waitForGCSManager(ctx context.Context, ssmClient *ssm.Client, instanceID string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)

		out, err := ssmClient.SendCommand(ctx, &ssm.SendCommandInput{
			InstanceIds:  []string{instanceID},
			DocumentName: aws.String("AWS-RunShellScript"),
			Parameters: map[string][]string{
				"commands":         {"systemctl is-active globus-connect-server-manager"},
				"executionTimeout": {"10"},
			},
		})
		if err != nil {
			continue
		}

		cmdID := aws.ToString(out.Command.CommandId)
		// Poll for command result (up to ~12s).
		for i := 0; i < 6; i++ {
			time.Sleep(2 * time.Second)
			inv, err := ssmClient.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
				CommandId:  aws.String(cmdID),
				InstanceId: aws.String(instanceID),
			})
			if err != nil {
				break
			}
			switch inv.Status {
			case ssmtypes.CommandInvocationStatusSuccess:
				if strings.TrimSpace(aws.ToString(inv.StandardOutputContent)) == "active" {
					logger.Info("GCS Manager active", "instance", instanceID)
					return nil
				}
				goto nextAttempt
			case ssmtypes.CommandInvocationStatusFailed,
				ssmtypes.CommandInvocationStatusTimedOut,
				ssmtypes.CommandInvocationStatusCancelled:
				goto nextAttempt
			}
		}
	nextAttempt:
	}
	return fmt.Errorf("timed out waiting for GCS Manager on %s", instanceID)
}

func main() {
	logger.Init("elastic-gcs-waker", region)
	lambda.Start(handler)
}
