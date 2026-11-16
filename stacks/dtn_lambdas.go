package stacks

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// goLambda describes one Go Lambda built from lambda/<dir>.
type goLambda struct {
	id      string // construct ID; must never change (it fixes the logical ID)
	kind    string // function name is dep.Resource(kind)
	dir     string // source directory under lambda/
	timeout awscdk.Duration
	env     map[string]*string
}

// newGoLambda creates a Lambda from a `make build-lambdas` binary: arm64,
// provided.al2023, 128 MB. Every Lambda derives its names, SSM paths and metric
// dimensions from AWS_REGION (set by the runtime) and DEPLOYMENT via
// names.FromEnv, and sends SSM Run Command output to ssmLogs (SSM_LOG_GROUP).
//
// Logs go to /aws/lambda/<function name> with three months' retention, set
// through LogRetention. LogRetention is deprecated in favour of an explicit
// awslogs.LogGroup, but deployed stacks already have those log groups
// (created by Lambda, retention set by the LogRetention custom resource), and
// CloudFormation fails to create a LogGroup whose name already exists. The
// switch needs a deploy with --import-existing-resources (#53).
func (p *dtnPool) newGoLambda(spec goLambda, ssmLogs awslogs.LogGroup) awslambda.Function {
	env := map[string]*string{
		"DEPLOYMENT":    jsii.String(p.dep.Name),
		"SSM_LOG_GROUP": ssmLogs.LogGroupName(),
	}
	for k, v := range spec.env {
		env[k] = v
	}
	return awslambda.NewFunction(p.stack, jsii.String(spec.id), &awslambda.FunctionProps{
		FunctionName: jsii.String(p.dep.Resource(spec.kind)),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/"+spec.dir), nil),
		Timeout:      spec.timeout,
		MemorySize:   jsii.Number(128),
		Environment:  &env,
		LogRetention: awslogs.RetentionDays_THREE_MONTHS,
	})
}

func minutes(n float64) awscdk.Duration { return awscdk.Duration_Minutes(jsii.Number(n)) }

func itoa[T int | int64](n T) *string { return jsii.String(fmt.Sprintf("%d", n)) }

// demandMonitorFn polls the Globus Transfer API every minute, publishes
// ElasticGCS/ActiveTransfers, and writes desired-dtns (raised while transfers
// are active, 0 after scale_in_idle) and last-active for the slot controller.
func (p *dtnPool) demandMonitorFn(ssmLogs awslogs.LogGroup) awslambda.Function {
	dep := p.dep
	fn := p.newGoLambda(goLambda{
		id: "DemandMonitorFn", kind: "demand-monitor", dir: "demand_monitor",
		timeout: minutes(1),
		env: map[string]*string{
			"SECRETS_PREFIX":    jsii.String(p.secretsPrefix),
			"ENDPOINT_ID_PARAM": jsii.String(dep.Param(names.ParamEndpointID)),
			"METRIC_NAMESPACE":  jsii.String(names.MetricNamespace),
			// Demand-driven scale-up (#26): desired = ceil(throughput / threshold).
			"MAX_DTNS":                 itoa(p.cfg.MaxDTNs),
			"THROUGHPUT_THRESHOLD_BPS": itoa(p.cfg.ThroughputThresholdBps),
			"SCALE_IN_IDLE":            jsii.String(p.cfg.ScaleInIdle.String()),
		},
	}, ssmLogs)
	// Writes desired-dtns and last-active (#129).
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid: jsii.String("DemandMonitorDesired"), Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings("ssm:PutParameter"),
		Resources: jsii.Strings(
			fmt.Sprintf("arn:aws:ssm:%s:%s:parameter%s", dep.Region, *p.stack.Account(), dep.Param(names.ParamDesiredDTNs)),
			fmt.Sprintf("arn:aws:ssm:%s:%s:parameter%s", dep.Region, *p.stack.Account(), dep.Param(names.ParamLastActive)),
		),
	}))
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String("DemandMonitorLambdaPerms"),
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("cloudwatch:PutMetricData"),
		Resources: jsii.Strings("*"),
	}))
	fn.AddToRolePolicy(p.scoped("DemandMonitor").readParams(dep.ParamPrefix()))
	// Globus client credentials: this deployment's secrets only, not every
	// secret in the account (part of #44).
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:     jsii.String("DemandMonitorGlobusSecrets"),
		Effect:  awsiam.Effect_ALLOW,
		Actions: jsii.Strings("secretsmanager:GetSecretValue"),
		Resources: jsii.Strings(
			fmt.Sprintf("arn:aws:secretsmanager:%s:%s:secret:%s/*", dep.Region, *p.stack.Account(), p.secretsPrefix),
		),
	}))
	return fn
}
