package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/scttfrdmn/globus-go-sdk/v3/pkg/services/transfer"

	"github.com/scttfrdmn/elastic-gcs/internal/logger"
)

// staticBearerToken implements the auth.Authorizer interface expected by the
// Globus transfer client. The interface requires a variadic context signature:
//
//	GetAuthorizationHeader(ctx ...context.Context) (string, error)
//
// The SDK's authorizers.StaticTokenAuthorizer uses a non-variadic signature so
// it cannot be used directly here.
type staticBearerToken struct{ token string }

func (a *staticBearerToken) GetAuthorizationHeader(_ ...context.Context) (string, error) {
	return "Bearer " + a.token, nil
}

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
//   - ElasticGCS/ActiveTransfers{Region=<region>}  — drives scale-in alarm (== 0 for 15 min)
//   - ElasticGCS/QueuedTransfers{Region=<region>}  — drives waker invocation
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

	// Create transfer client with the bearer token
	transferClient, err := transfer.NewClient(transfer.WithAuthorizer(&staticBearerToken{token: token.AccessToken}))
	if err != nil {
		return fmt.Errorf("failed to create Globus transfer client: %w", err)
	}

	// Count ACTIVE tasks for our endpoint
	activeCount, err := countTasksByStatus(ctx, transferClient, endpointID, "ACTIVE")
	if err != nil {
		return fmt.Errorf("ListTasks ACTIVE failed: %w", err)
	}

	// Count INACTIVE tasks for our endpoint.
	// Globus marks tasks INACTIVE when the endpoint is offline or unreachable.
	// A non-zero count means transfers are queued and the system needs to wake up.
	queuedCount, err := countTasksByStatus(ctx, transferClient, endpointID, "INACTIVE")
	if err != nil {
		// Non-fatal: log and continue so we still publish the active metric.
		logger.Warn("ListTasks INACTIVE failed", "error", err.Error())
		queuedCount = 0
	}

	logger.Info("transfer task counts",
		"endpoint", endpointID,
		"active_transfers", activeCount,
		"queued_transfers", queuedCount,
	)

	// Publish both metrics
	cwClient := cloudwatch.NewFromConfig(cfg)
	_, err = cwClient.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(metricNamespace),
		MetricData: []cwtypes.MetricDatum{
			{
				MetricName: aws.String("ActiveTransfers"),
				Value:      aws.Float64(float64(activeCount)),
				Unit:       cwtypes.StandardUnitCount,
				Dimensions: []cwtypes.Dimension{
					{Name: aws.String("Region"), Value: aws.String(region)},
				},
			},
			{
				MetricName: aws.String("QueuedTransfers"),
				Value:      aws.Float64(float64(queuedCount)),
				Unit:       cwtypes.StandardUnitCount,
				Dimensions: []cwtypes.Dimension{
					{Name: aws.String("Region"), Value: aws.String(region)},
				},
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
	if queuedCount > 0 && wakerFunctionName != "" {
		logger.Info("queued transfers detected, invoking waker", "queued", queuedCount, "waker", wakerFunctionName)
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

// countTasksByStatus queries the Globus Transfer API for tasks in the given
// status that involve our endpoint (as source or destination).
func countTasksByStatus(ctx context.Context, client *transfer.Client, endpointID, status string) (int, error) {
	taskList, err := client.ListTasks(ctx, &transfer.ListTasksOptions{
		FilterStatus: status,
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, task := range taskList.Data {
		if task.SourceEndpointID == endpointID || task.DestinationEndpointID == endpointID {
			count++
		}
	}
	return count, nil
}

func main() {
	region := os.Getenv("AWS_REGION")
	logger.Init("elastic-gcs-demand-monitor", region)
	lambda.Start(handler)
}
