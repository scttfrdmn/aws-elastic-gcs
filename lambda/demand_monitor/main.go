package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/scttfrdmn/globus-go-sdk/v4/pkg/authorizers"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/demand"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/logger"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/scaling"
)

// handler polls the Globus Transfer endpoint_manager API for the endpoint's
// tasks, publishes demand metrics, and sets desired-dtns for the slot
// controller (#26, #129).
//
// Triggered by: EventBridge schedule (every minute)
//
// Metrics (dimensions Region, Deployment):
//   - ActiveTransfers    — ACTIVE tasks
//   - TransferThroughput — sum of effective_bytes_per_second over ACTIVE tasks
//   - QueuedTransfers    — INACTIVE (suspended) tasks; observability only
//
// INACTIVE tasks are suspended and won't progress without intervention (expired
// credentials, admin pause), so they don't wake the endpoint (#29). Note that
// Transfer rejects a submission outright when the endpoint has no serving node
// at all (#130), so demand can't wake an endpoint from zero nodes.
//
// Scale-up: desired = DesiredDTNs(active, throughput, per-DTN threshold,
// MAX_DTNS); it only rises while transfers are active, so a dip in throughput
// can't stop DTNs mid-transfer. It drops to 0 after SCALE_IN_IDLE.
func handler(ctx context.Context, _ interface{}) error {
	metricNamespace := envOr("METRIC_NAMESPACE", names.MetricNamespace)
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	endpointID, authz, err := credentials(ctx, cfg)
	if err != nil {
		return err
	}
	lister := demand.Lister{Authorizer: authz}
	activeTasks, err := lister.List(ctx, endpointID, "ACTIVE")
	if err != nil {
		return fmt.Errorf("list ACTIVE tasks: %w", err)
	}
	active := demand.Aggregate(activeTasks)
	suspended := demand.Stats{}
	if t, err := lister.List(ctx, endpointID, "INACTIVE"); err != nil {
		logger.Warn("list INACTIVE tasks failed", "error", err.Error())
	} else {
		suspended = demand.Aggregate(t)
	}
	logger.Info("transfer task stats", "endpoint", endpointID,
		"active_transfers", active.Count, "throughput_bytes_per_sec", active.TotalBytesPerSec,
		"suspended_transfers", suspended.Count)

	dim := []cwtypes.Dimension{
		{Name: aws.String("Region"), Value: aws.String(dep.Region)},
		{Name: aws.String("Deployment"), Value: aws.String(dep.Name)},
	}
	if _, err := cloudwatch.NewFromConfig(cfg).PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(metricNamespace),
		MetricData: []cwtypes.MetricDatum{
			{MetricName: aws.String("ActiveTransfers"), Value: aws.Float64(float64(active.Count)), Unit: cwtypes.StandardUnitCount, Dimensions: dim},
			{MetricName: aws.String("QueuedTransfers"), Value: aws.Float64(float64(suspended.Count)), Unit: cwtypes.StandardUnitCount, Dimensions: dim},
			{MetricName: aws.String("TransferThroughput"), Value: aws.Float64(float64(active.TotalBytesPerSec)), Unit: cwtypes.StandardUnitBytesSecond, Dimensions: dim},
		},
	}); err != nil {
		return fmt.Errorf("PutMetricData failed: %w", err)
	}

	return setDesired(ctx, cfg, active)
}

// setDesired drives the slot controller (#129): while transfers are active,
// desired-dtns only rises (to DesiredDTNs) and last-active is refreshed; after
// SCALE_IN_IDLE with nothing active it drops to 0. The controller scales in
// one slot at a time, draining each.
func setDesired(ctx context.Context, cfg aws.Config, active demand.Stats) error {
	client := ssm.NewFromConfig(cfg)
	get := func(leaf string) string {
		out, err := client.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(dep.Param(leaf))})
		if err != nil || out.Parameter == nil {
			return ""
		}
		return aws.ToString(out.Parameter.Value)
	}
	put := func(leaf, v string) error {
		_, err := client.PutParameter(ctx, &ssm.PutParameterInput{Name: aws.String(dep.Param(leaf)), Value: aws.String(v),
			Type: ssmtypes.ParameterTypeString, Overwrite: aws.Bool(true)})
		return err
	}
	current, _ := strconv.Atoi(get(names.ParamDesiredDTNs))
	now := time.Now().UTC()
	maxDTNs, _ := strconv.Atoi(os.Getenv("MAX_DTNS"))
	perDTN, _ := strconv.ParseInt(os.Getenv("THROUGHPUT_THRESHOLD_BPS"), 10, 64)
	idle, err := time.ParseDuration(os.Getenv("SCALE_IN_IDLE"))
	if err != nil {
		idle = 15 * time.Minute
	}
	next := current
	if active.Count > 0 {
		if err := put(names.ParamLastActive, now.Format(time.RFC3339)); err != nil {
			return fmt.Errorf("put last-active: %w", err)
		}
		next, _ = scaling.ScaleUp(current, scaling.DesiredDTNs(active.Count, active.TotalBytesPerSec, perDTN, maxDTNs))
	} else if last, err := time.Parse(time.RFC3339, get(names.ParamLastActive)); err != nil || now.Sub(last) >= idle {
		next = 0
	}
	if next == current && get(names.ParamDesiredDTNs) != "" {
		return nil
	}
	logger.Info("desired-dtns", "from", current, "to", next, "active", active.Count, "throughput_bytes_per_sec", active.TotalBytesPerSec)
	return put(names.ParamDesiredDTNs, strconv.Itoa(next))
}

// Warm-invocation cache (#28): the endpoint ID and an auto-refreshing
// client-credentials authorizer (globus-go-sdk v4) live for the container.
var (
	credMu   sync.Mutex
	cachedEP string
	authz    *authorizers.ClientCredentialsAuthorizer
)

func credentials(ctx context.Context, cfg aws.Config) (string, *authorizers.ClientCredentialsAuthorizer, error) {
	credMu.Lock()
	defer credMu.Unlock()
	if cachedEP == "" {
		p, err := ssm.NewFromConfig(cfg).GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(os.Getenv("ENDPOINT_ID_PARAM"))})
		if err != nil {
			return "", nil, fmt.Errorf("get endpoint ID: %w", err)
		}
		cachedEP = aws.ToString(p.Parameter.Value)
	}
	if authz == nil {
		sm := secretsmanager.NewFromConfig(cfg)
		prefix := os.Getenv("SECRETS_PREFIX")
		var vals [2]string
		for i, name := range []string{"globus-client-id", "globus-client-secret"} {
			out, err := sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(prefix + "/" + name)})
			if err != nil {
				return "", nil, fmt.Errorf("get %s: %w", name, err)
			}
			vals[i] = aws.ToString(out.SecretString)
		}
		authz = authorizers.NewClientCredentialsAuthorizer(vals[0], vals[1],
			[]string{"urn:globus:auth:scope:transfer.api.globus.org:all"})
	}
	return cachedEP, authz, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// dep identifies this deployment; set in main from AWS_REGION and DEPLOYMENT.
var dep names.Deployment

func main() {
	region := os.Getenv("AWS_REGION")
	logger.Init("elastic-gcs-demand-monitor", region)
	var err error
	if dep, err = names.FromEnv(); err != nil {
		logger.Error("invalid deployment configuration", "error", err.Error())
		os.Exit(1)
	}
	lambda.Start(handler)
}
