package main

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
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

// handler polls the Globus Transfer API for active tasks on the configured
// endpoint and publishes the count as a CloudWatch metric.
//
// Triggered by: EventBridge schedule (every 1 minute)
//
// Metric published: ElasticGCS/ActiveTransfers{Region=<region>}
// This metric drives:
//   - Scale-out alarm: ActiveTransfers > 0 → trigger scale-out Lambda
//   - Scale-in alarm: ActiveTransfers == 0 for 15 min → trigger scale-in Lambda
func handler(ctx context.Context, _ interface{}) error {
	region := os.Getenv("REGION")
	secretsPrefix := os.Getenv("SECRETS_PREFIX")
	endpointIDParam := os.Getenv("ENDPOINT_ID_PARAM")
	metricNamespace := os.Getenv("METRIC_NAMESPACE")
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

	// List active transfer tasks; filter to those involving our endpoint
	taskList, err := transferClient.ListTasks(ctx, &transfer.ListTasksOptions{
		FilterStatus: "ACTIVE",
	})
	if err != nil {
		return fmt.Errorf("ListTasks failed: %w", err)
	}

	var activeCount int
	for _, task := range taskList.Data {
		if task.SourceEndpointID == endpointID || task.DestinationEndpointID == endpointID {
			activeCount++
		}
	}
	logger.Info("active transfers counted",
		"endpoint", endpointID,
		"active_transfers", activeCount,
		"total_active_tasks", len(taskList.Data),
	)

	// Publish metric
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
		},
	})
	if err != nil {
		return fmt.Errorf("PutMetricData failed: %w", err)
	}

	return nil
}

func main() {
	region := os.Getenv("AWS_REGION")
	logger.Init("elastic-gcs-demand-monitor", region)
	lambda.Start(handler)
}
