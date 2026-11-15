package stacks

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsautoscaling"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// dtnLambdas are the orchestration Lambdas of one DTN pool.
type dtnLambdas struct {
	lifecycle, scaleOut, scaleIn, cacheEviction, demandMonitor, versionCheck, waker awslambda.Function
	idleShutdown                                                                    awslambda.Function // nil unless IdleShutdownMinutes > 0
	wakerURL                                                                        awslambda.FunctionUrl
}

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

// lambdas creates the orchestration Lambdas and their IAM grants.
// All built for arm64/provided.al2023. Run `make build-lambdas` before deploy.
func (p *dtnPool) lambdas(asg awsautoscaling.AutoScalingGroup) dtnLambdas {
	// SSM Run Command output from the Lambdas goes to this log group, so every
	// command run on an instance is auditable (#48). Instances write it with
	// CloudWatchAgentServerPolicy.
	ssmLogs := awslogs.NewLogGroup(p.stack, jsii.String("SSMCommandLogs"), &awslogs.LogGroupProps{
		LogGroupName:  jsii.String("/elastic-gcs/" + p.dep.ID() + "/ssm-commands"),
		Retention:     awslogs.RetentionDays_THREE_MONTHS,
		RemovalPolicy: awscdk.RemovalPolicy_RETAIN,
	})

	fns := dtnLambdas{
		lifecycle:     p.lifecycleFn(asg, ssmLogs),
		scaleOut:      p.scaleOutFn(asg, ssmLogs),
		scaleIn:       p.scaleInFn(asg, ssmLogs),
		cacheEviction: p.cacheEvictionFn(asg, ssmLogs),
		demandMonitor: p.demandMonitorFn(ssmLogs),
		versionCheck:  p.versionCheckFn(asg, ssmLogs),
		waker:         p.wakerFn(asg, ssmLogs),
	}

	// Lambda Function URL with AWS_IAM auth — enforced by the Lambda service
	// before code runs. Only callers with SigV4-signed requests and
	// lambda:InvokeFunctionUrl permission on this function are admitted.
	fns.wakerURL = fns.waker.AddFunctionUrl(&awslambda.FunctionUrlOptions{
		AuthType: awslambda.FunctionUrlAuthType_AWS_IAM,
	})

	if p.cfg.IdleShutdownMinutes > 0 {
		fns.idleShutdown = p.idleShutdownFn(asg, ssmLogs)
	}
	return fns
}

// lifecycleFn handles ASG lifecycle hook events: node disable before a DTN
// stops, slot lock release, warm pool entry tagging.
func (p *dtnPool) lifecycleFn(asg awsautoscaling.AutoScalingGroup, ssmLogs awslogs.LogGroup) awslambda.Function {
	dep := p.dep
	fn := p.newGoLambda(goLambda{
		id: "LifecycleFn", kind: "lifecycle", dir: "lifecycle",
		timeout: minutes(6), // > terminate hook heartbeat
		env: map[string]*string{
			"ASG_NAME":       asg.AutoScalingGroupName(),
			"SECRETS_PREFIX": jsii.String(p.secretsPrefix),
		},
	}, ssmLogs)
	// Lifecycle Lambda needs SSM (node disable + slot lock release), ASG (complete hook), EC2 (describe + CreateTags)
	lc := p.scoped("Lifecycle")
	grant(fn, lc.readParams(dep.ParamPrefix()))
	grant(fn, lc.runShellOnRole(names.RoleDTN)...)
	grant(fn,
		lc.onASG("autoscaling:CompleteLifecycleAction"),
		lc.onRoleInstances("TagInstances", names.RoleDTN, "ec2:CreateTags"),
		lc.unscoped(
			"autoscaling:DescribeAutoScalingInstances",
			"ec2:DescribeInstances",
		),
		// Slot-lock release: this deployment's parameters only.
		awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid:       jsii.String("LifecycleReleaseSlotLock"),
			Effect:    awsiam.Effect_ALLOW,
			Actions:   jsii.Strings("ssm:DeleteParameter"),
			Resources: jsii.Strings(fmt.Sprintf("arn:aws:ssm:%s:%s:parameter%s/*", dep.Region, *p.stack.Account(), dep.ParamPrefix())),
		}),
	)
	return fn
}

// scaleOutFn raises desired capacity when a scale-out alarm fires.
func (p *dtnPool) scaleOutFn(asg awsautoscaling.AutoScalingGroup, ssmLogs awslogs.LogGroup) awslambda.Function {
	dep := p.dep
	fn := p.newGoLambda(goLambda{
		id: "ScaleOutFn", kind: "scaleout", dir: "scaleout",
		timeout: minutes(5),
		env: map[string]*string{
			"ASG_NAME":       asg.AutoScalingGroupName(),
			"MAX_DTNS":       itoa(p.cfg.MaxDTNs),
			"SECRETS_PREFIX": jsii.String(p.secretsPrefix),
			"WARM_POOL_SIZE": itoa(p.cfg.WarmPoolSize),
			// Only these alarms may scale out; anything else on the topic is ignored (#27).
			"SCALE_OUT_ALARMS": jsii.String(dep.Resource("scaleout") + "," + dep.Resource("scaleout-throughput")),
		},
	}, ssmLogs)
	so := p.scoped("ScaleOut")
	grant(fn,
		so.onASG("autoscaling:SetDesiredCapacity", "autoscaling:PutWarmPool"),
		so.unscoped("autoscaling:DescribeAutoScalingGroups", "autoscaling:DescribeWarmPool", "cloudwatch:PutMetricData"),
	)
	return fn
}

// scaleInFn lowers desired capacity once no transfers are active.
func (p *dtnPool) scaleInFn(asg awsautoscaling.AutoScalingGroup, ssmLogs awslogs.LogGroup) awslambda.Function {
	fn := p.newGoLambda(goLambda{
		id: "ScaleInFn", kind: "scalein", dir: "scalein",
		timeout: minutes(10), // up to 8 min of ScalingActivityInProgress retries
		env: map[string]*string{
			"ASG_NAME":         asg.AutoScalingGroupName(),
			"SECRETS_PREFIX":   jsii.String(p.secretsPrefix),
			"METRIC_NAMESPACE": jsii.String(names.MetricNamespace),
		},
	}, ssmLogs)
	si := p.scoped("ScaleIn")
	grant(fn, si.readParams(p.dep.ParamPrefix()))
	grant(fn, si.runShellOnRole(names.RoleControl)...) // re-enable the control node before the last DTN goes (#71)
	grant(fn,
		si.onASG("autoscaling:SetDesiredCapacity"),
		si.unscoped("autoscaling:DescribeAutoScalingGroups", "cloudwatch:GetMetricStatistics", "cloudwatch:PutMetricData", "ec2:DescribeInstances"),
	)
	return fn
}

// cacheEvictionFn moves warm pool instances idle past the TTL to the cold tier.
func (p *dtnPool) cacheEvictionFn(asg awsautoscaling.AutoScalingGroup, ssmLogs awslogs.LogGroup) awslambda.Function {
	dep := p.dep
	fn := p.newGoLambda(goLambda{
		id: "CacheEvictionFn", kind: "cache-eviction", dir: "cache_eviction",
		timeout: minutes(10),
		env: map[string]*string{
			"ASG_NAME":              asg.AutoScalingGroupName(),
			"SECRETS_PREFIX":        jsii.String(p.secretsPrefix),
			"CACHE_EVICTION_HOURS":  itoa(p.cfg.CacheEvictionHours),
			"CONTROL_NODE_ID_PARAM": jsii.String(dep.Param(names.ParamControlNodeID)),
		},
	}, ssmLogs)
	ce := p.scoped("CacheEviction")
	grant(fn, ce.readParams(dep.ParamPrefix()))
	grant(fn, ce.runShellOnRole(names.RoleControl)...)
	grant(fn,
		ce.onASG("autoscaling:DeleteWarmPool"), // eviction to the cold tier (#33)
		ce.unscoped(
			"autoscaling:DescribeAutoScalingGroups",
			"autoscaling:DescribeWarmPool",
			"autoscaling:DescribeAutoScalingInstances",
			"ec2:DescribeInstances",
			"cloudwatch:PutMetricData",
		),
	)
	// Cache eviction also needs to release slot locks for instances it terminates,
	// since those instances may not run their shutdown script before being killed.
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String("CacheEvictionSlotLockCleanup"),
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("ssm:DeleteParameter"),
		Resources: jsii.Strings(fmt.Sprintf("arn:aws:ssm:%s:*:parameter%s/%s/*", dep.Region, dep.ParamPrefix(), names.ParamSlotLock)),
	}))
	return fn
}

// demandMonitorFn polls the Globus Transfer API every minute and publishes
// ElasticGCS/ActiveTransfers. This metric drives both the scale-out alarm
// (> 0) and the scale-in alarm (== 0 for 15 min).
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
			"ASG_NAME":                 jsii.String(p.asgName),
			"MAX_DTNS":                 itoa(p.cfg.MaxDTNs),
			"THROUGHPUT_THRESHOLD_BPS": itoa(p.cfg.ThroughputThresholdBps),
			"WARM_POOL_SIZE":           itoa(p.cfg.WarmPoolSize),
			"POOL":                     jsii.String(p.cfg.Pool),
			"SCALE_IN_IDLE":            jsii.String(p.cfg.ScaleInIdle.String()),
		},
	}, ssmLogs)
	if p.cfg.Pool == "slots" { // writes desired-dtns and last-active (#129)
		fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid: jsii.String("DemandMonitorDesired"), Effect: awsiam.Effect_ALLOW,
			Actions: jsii.Strings("ssm:PutParameter"),
			Resources: jsii.Strings(
				fmt.Sprintf("arn:aws:ssm:%s:%s:parameter%s", dep.Region, *p.stack.Account(), dep.Param(names.ParamDesiredDTNs)),
				fmt.Sprintf("arn:aws:ssm:%s:%s:parameter%s", dep.Region, *p.stack.Account(), dep.Param(names.ParamLastActive)),
			),
		}))
	}
	fn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("DemandMonitorLambdaPerms"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"cloudwatch:PutMetricData",
			"autoscaling:DescribeAutoScalingGroups",
		),
		Resources: jsii.Strings("*"),
	}))
	fn.AddToRolePolicy(p.scoped("DemandMonitor").onASG("autoscaling:SetDesiredCapacity", "autoscaling:PutWarmPool"))
	fn.AddToRolePolicy(p.scoped("DemandMonitorWarm").unscoped("autoscaling:DescribeWarmPool"))
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

// versionCheckFn runs every 6h. It queries each running node for its GCS
// version, then publishes ElasticGCS/GCSVersionDrift (0=healthy, >0=drift).
// Only InService DTNs are checked — stopped warm pool instances can't run SSM.
func (p *dtnPool) versionCheckFn(asg awsautoscaling.AutoScalingGroup, ssmLogs awslogs.LogGroup) awslambda.Function {
	fn := p.newGoLambda(goLambda{
		id: "VersionCheckFn", kind: "version-check", dir: "version_check",
		timeout: minutes(2),
		env: map[string]*string{
			"ASG_NAME":              asg.AutoScalingGroupName(),
			"CONTROL_NODE_ID_PARAM": jsii.String(p.dep.Param(names.ParamControlNodeID)),
		},
	}, ssmLogs)
	vc := p.scoped("VersionCheck")
	grant(fn, vc.readParams(p.dep.ParamPrefix()))
	grant(fn, vc.runShell()...)
	grant(fn, vc.unscoped(
		"autoscaling:DescribeAutoScalingGroups",
		"ec2:DescribeInstances",
		"cloudwatch:PutMetricData",
	))
	return fn
}

// wakerFn is always created. It starts the control node on demand and
// optionally triggers DTN scale-out.
//
// Auth: AWS_IAM — the Lambda service enforces SigV4 authentication before
// the function code runs. Only IAM principals with lambda:InvokeFunctionUrl
// permission on this function can call the URL.
//
// aws-elastic-gcs-ctl wake uses direct Lambda Invoke (also IAM-signed).
// Globus Flows HTTP actions cannot call AWS_IAM URLs directly; see
// docs/ephemeral-control-node.md for the proxy pattern.
func (p *dtnPool) wakerFn(asg awsautoscaling.AutoScalingGroup, ssmLogs awslogs.LogGroup) awslambda.Function {
	fn := p.newGoLambda(goLambda{
		id: "WakerFn", kind: "waker", dir: "waker",
		timeout: minutes(6),
		env: map[string]*string{
			"ASG_NAME":              asg.AutoScalingGroupName(),
			"CONTROL_NODE_ID_PARAM": jsii.String(p.dep.Param(names.ParamControlNodeID)),
		},
	}, ssmLogs)
	wk := p.scoped("Waker")
	grant(fn, wk.readParams(p.dep.ParamPrefix()))
	grant(fn, wk.runShellOnRole(names.RoleControl)...)
	grant(fn,
		wk.onRoleInstances("StartInstances", names.RoleControl, "ec2:StartInstances"),
		wk.onASG("autoscaling:SetDesiredCapacity"),
		wk.unscoped(
			"ec2:DescribeInstances",
			"ssm:DescribeInstanceInformation",
			"autoscaling:DescribeAutoScalingGroups",
			"autoscaling:DescribeWarmPool",
		),
	)
	return fn
}

// idleShutdownFn (only when IdleShutdownMinutes > 0) runs every 15 minutes
// and stops the control node when all conditions are met: desired=0, all warm
// pool instances Warmed:Stopped, no active transfers for IdleShutdownMinutes,
// min uptime elapsed.
//
// IMPORTANT: When enabled, the endpoint becomes unreachable while stopped.
// Read docs/ephemeral-control-node.md before enabling.
func (p *dtnPool) idleShutdownFn(asg awsautoscaling.AutoScalingGroup, ssmLogs awslogs.LogGroup) awslambda.Function {
	fn := p.newGoLambda(goLambda{
		id: "IdleShutdownFn", kind: "idle-shutdown", dir: "idle_shutdown",
		timeout: minutes(6), // up to ~4 min waiting for node disable + DNS
		env: map[string]*string{
			"ASG_NAME":              asg.AutoScalingGroupName(),
			"CONTROL_NODE_ID_PARAM": jsii.String(p.dep.Param(names.ParamControlNodeID)),
			"IDLE_MINUTES":          itoa(p.cfg.IdleShutdownMinutes),
			"MIN_UPTIME_MINUTES":    itoa(p.cfg.MinUptimeMinutes),
			"METRIC_NAMESPACE":      jsii.String(names.MetricNamespace),
		},
	}, ssmLogs)
	is := p.scoped("IdleShutdown")
	grant(fn, is.readParams(p.dep.ParamPrefix()))
	grant(fn, is.runShellOnRole(names.RoleControl)...) // node disable before stop (#30)
	grant(fn,
		is.onRoleInstances("StopInstances", names.RoleControl, "ec2:StopInstances"),
		is.unscoped(
			"ec2:DescribeInstances",
			"autoscaling:DescribeAutoScalingGroups",
			"autoscaling:DescribeWarmPool",
			"cloudwatch:GetMetricStatistics",
		),
	)
	return fn
}
