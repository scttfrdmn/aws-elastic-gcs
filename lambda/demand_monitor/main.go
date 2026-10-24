package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	sdkauth "github.com/scttfrdmn/globus-go-sdk/v3/pkg/services/auth"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/logger"
)

// wakeRequest matches the WakeRequest struct in the waker Lambda.
type wakeRequest struct {
	ScaleOut bool `json:"scale_out"`
}

// handler polls the Globus Transfer API for active and inactive (queued) tasks
// on the configured endpoint, publishes CloudWatch metrics, and invokes the
// waker Lambda when queued transfers are detected (meaning the endpoint is
// offline and needs to be started).
//
// Triggered by: EventBridge schedule (every 1 minute)
//
// Metrics published:
//   - ElasticGCS/ActiveTransfers{Region=<region>}   — count of ACTIVE tasks; drives scale-in alarm
//   - ElasticGCS/QueuedTransfers{Region=<region>}   — count of INACTIVE tasks; drives waker
//   - ElasticGCS/TransferThroughput{Region=<region>} — sum of effective_bytes_per_second across
//     all ACTIVE tasks (bytes/sec); drives multi-DTN scale-out alarm
//
// Auto-wake logic:
//
//	INACTIVE tasks exist → endpoint offline → invoke waker with scale_out=true
func handler(ctx context.Context, _ interface{}) error {
	region := os.Getenv("REGION")
	secretsPrefix := os.Getenv("SECRETS_PREFIX")
	endpointIDParam := os.Getenv("ENDPOINT_ID_PARAM")
	metricNamespace := os.Getenv("METRIC_NAMESPACE")
	wakerFunctionName := os.Getenv("WAKER_FUNCTION_NAME")
	if metricNamespace == "" {
		metricNamespace = "ElasticGCS"
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Read the Globus endpoint ID from SSM Parameter Store
	ssmClient := ssm.NewFromConfig(cfg)
	paramOut, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(endpointIDParam),
	})
	if err != nil {
		return fmt.Errorf("GetParameter %s failed: %w", endpointIDParam, err)
	}
	endpointID := aws.ToString(paramOut.Parameter.Value)

	// Read Globus client credentials from Secrets Manager
	smClient := secretsmanager.NewFromConfig(cfg)
	clientIDOut, err := smClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(fmt.Sprintf("%s/globus-client-id", secretsPrefix)),
	})
	if err != nil {
		return fmt.Errorf("GetSecretValue globus-client-id failed: %w", err)
	}
	clientSecretOut, err := smClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(fmt.Sprintf("%s/globus-client-secret", secretsPrefix)),
	})
	if err != nil {
		return fmt.Errorf("GetSecretValue globus-client-secret failed: %w", err)
	}
	clientID := aws.ToString(clientIDOut.SecretString)
	clientSecret := aws.ToString(clientSecretOut.SecretString)

	// Authenticate with Globus using client credentials flow
	authClient, err := sdkauth.NewClient(
		sdkauth.WithClientID(clientID),
		sdkauth.WithClientSecret(clientSecret),
	)
	if err != nil {
		return fmt.Errorf("failed to create Globus auth client: %w", err)
	}
	token, err := authClient.GetClientCredentialsToken(ctx,
		"urn:globus:auth:scope:transfer.api.globus.org:all")
	if err != nil {
		return fmt.Errorf("GetClientCredentialsToken failed: %w", err)
	}

	// Fetch ACTIVE task stats: count + sum of effective_bytes_per_second.
	// The endpoint manager API (GET /endpoint_manager/task_list) returns tasks
	// submitted by ALL users, not just the caller. This requires Activity Monitor
	// or higher role on the endpoint (the automation client has Admin).
	// The regular ListTasks API only returns the caller's own tasks and would
	// always return 0 for tasks submitted by researchers via the globus CLI.
	activeStats, err := fetchEndpointManagerStats(ctx, token.AccessToken, endpointID, "ACTIVE")
	if err != nil {
		return fmt.Errorf("endpoint_manager/task_list ACTIVE failed: %w", err)
	}

	// Count INACTIVE tasks for our endpoint.
	// Globus marks tasks INACTIVE when the endpoint is offline or unreachable.
	// A non-zero count means transfers are queued and the system needs to wake up.
	queuedStats, err := fetchEndpointManagerStats(ctx, token.AccessToken, endpointID, "INACTIVE")
	if err != nil {
		// Non-fatal: log and continue so we still publish the active metrics.
		logger.Warn("endpoint_manager/task_list INACTIVE failed", "error", err.Error())
		queuedStats = taskStats{}
	}

	logger.Info("transfer task stats",
		"endpoint", endpointID,
		"active_transfers", activeStats.Count,
		"throughput_bytes_per_sec", activeStats.TotalBytesPerSec,
		"queued_transfers", queuedStats.Count,
	)

	// Publish all three metrics in a single PutMetricData call.
	cwClient := cloudwatch.NewFromConfig(cfg)
	dim := []cwtypes.Dimension{{Name: aws.String("Region"), Value: aws.String(region)}}
	_, err = cwClient.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(metricNamespace),
		MetricData: []cwtypes.MetricDatum{
			{
				MetricName: aws.String("ActiveTransfers"),
				Value:      aws.Float64(float64(activeStats.Count)),
				Unit:       cwtypes.StandardUnitCount,
				Dimensions: dim,
			},
			{
				MetricName: aws.String("QueuedTransfers"),
				Value:      aws.Float64(float64(queuedStats.Count)),
				Unit:       cwtypes.StandardUnitCount,
				Dimensions: dim,
			},
			{
				MetricName: aws.String("TransferThroughput"),
				Value:      aws.Float64(float64(activeStats.TotalBytesPerSec)),
				Unit:       cwtypes.StandardUnitBytesSecond,
				Dimensions: dim,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("PutMetricData failed: %w", err)
	}

	// If queued transfers are detected and we have a waker configured, invoke it
	// to start the control node and scale out DTNs. This is fire-and-forget:
	// the waker runs asynchronously and handles idempotency (won't double-start
	// an already-running node). We log but don't fail on invoke errors.
	if queuedStats.Count > 0 && wakerFunctionName != "" {
		logger.Info("queued transfers detected, invoking waker", "queued", queuedStats.Count, "waker", wakerFunctionName)
		payload, _ := json.Marshal(wakeRequest{ScaleOut: true})
		lambdaClient := awslambda.NewFromConfig(cfg)
		_, invokeErr := lambdaClient.Invoke(ctx, &awslambda.InvokeInput{
			FunctionName:   aws.String(wakerFunctionName),
			InvocationType: "Event", // async — don't wait for response
			Payload:        payload,
		})
		if invokeErr != nil {
			logger.Warn("waker invocation failed", "error", invokeErr.Error())
		} else {
			logger.Info("waker invoked successfully")
		}
	}

	return nil
}

// taskStats holds aggregate statistics for tasks in a given status.
type taskStats struct {
	Count            int
	TotalBytesPerSec int64 // sum of effective_bytes_per_second across all tasks
}

// endpointManagerTaskList is the response shape for
// GET /v0.10/endpoint_manager/task_list
type endpointManagerTaskList struct {
	DATA []struct {
		TaskID             string `json:"task_id"`
		EffectiveBytesPerS int64  `json:"effective_bytes_per_second"`
	} `json:"DATA"`
}

// fetchEndpointManagerStats queries the Globus Transfer endpoint manager task
// list API for tasks in the given status on the given endpoint. Returns the
// task count and the sum of effective_bytes_per_second across all tasks.
//
// Unlike the regular /task_list endpoint, this returns tasks submitted by ALL
// users on the endpoint, which is required for monitoring researcher transfers.
// Requires Activity Monitor (or higher) role on the endpoint.
func fetchEndpointManagerStats(ctx context.Context, bearerToken, endpointID, status string) (taskStats, error) {
	q := url.Values{}
	q.Set("filter_status", status)
	q.Set("filter_endpoint", endpointID)
	reqURL := "https://transfer.api.globus.org/v0.10/endpoint_manager/task_list?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return taskStats{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return taskStats{}, fmt.Errorf("HTTP GET endpoint_manager/task_list: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return taskStats{}, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return taskStats{}, fmt.Errorf("endpoint_manager/task_list returned HTTP %d: %s", resp.StatusCode, body)
	}

	var result endpointManagerTaskList
	if err := json.Unmarshal(body, &result); err != nil {
		return taskStats{}, fmt.Errorf("parse endpoint_manager/task_list response: %w", err)
	}

	var stats taskStats
	stats.Count = len(result.DATA)
	for _, task := range result.DATA {
		stats.TotalBytesPerSec += task.EffectiveBytesPerS
	}
	return stats, nil
}

func main() {
	region := os.Getenv("AWS_REGION")
	logger.Init("elastic-gcs-demand-monitor", region)
	lambda.Start(handler)
}
