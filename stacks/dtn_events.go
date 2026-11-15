package stacks

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsevents"
	"github.com/aws/aws-cdk-go/awscdk/v2/awseventstargets"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	"github.com/aws/jsii-runtime-go"
)

// eventRules wires ASG lifecycle events and schedules to the Lambdas.
func (p *dtnPool) eventRules(fns dtnLambdas) {
	region, asgName := p.dep.Region, p.asgName
	target := func(rule awsevents.Rule, fn awslambda.Function) {
		rule.AddTarget(awseventstargets.NewLambdaFunction(fn, nil))
	}

	// ASG Lifecycle Hook Events → Lifecycle Lambda.
	// ASG publishes lifecycle events to EventBridge automatically.
	// We filter to only our ASG by name (hardcoded to avoid CDK token issues).
	lifecycleDetail := map[string]interface{}{
		"AutoScalingGroupName": []interface{}{asgName},
	}
	target(awsevents.NewRule(p.stack, jsii.String("LifecycleRule"), &awsevents.RuleProps{
		Description: jsii.String(fmt.Sprintf("ASG lifecycle hook events for %s", asgName)),
		EventPattern: &awsevents.EventPattern{
			Source: jsii.Strings("aws.autoscaling"),
			DetailType: jsii.Strings(
				"EC2 Instance-launch Lifecycle Action",
				"EC2 Instance-terminate Lifecycle Action",
			),
			Detail: &lifecycleDetail,
		},
	}), fns.lifecycle)

	// Daily cache eviction: terminates warm pool instances stopped for longer
	// than cacheEvictionHours.
	target(awsevents.NewRule(p.stack, jsii.String("CacheEvictionRule"), &awsevents.RuleProps{
		Description: jsii.String(fmt.Sprintf(
			"Evict warm pool instances stopped > %dh in %s", p.cfg.CacheEvictionHours, region)),
		Schedule: awsevents.Schedule_Rate(awscdk.Duration_Hours(jsii.Number(24))),
	}), fns.cacheEviction)

	// Demand monitor every 1 minute.
	// Note: 1 minute is the AWS minimum for EventBridge rate() expressions.
	// Sub-minute polling would require Step Functions or SQS-based self-chaining.
	target(awsevents.NewRule(p.stack, jsii.String("DemandMonitorRule"), &awsevents.RuleProps{
		Description: jsii.String(fmt.Sprintf(
			"Poll Globus Transfer API for active transfers every 1 min in %s", region)),
		Schedule: awsevents.Schedule_Rate(minutes(1)),
	}), fns.demandMonitor)

	// Version check every 6 hours.
	target(awsevents.NewRule(p.stack, jsii.String("VersionCheckRule"), &awsevents.RuleProps{
		Description: jsii.String(fmt.Sprintf("GCS version drift check every 6h in %s", region)),
		Schedule:    awsevents.Schedule_Rate(awscdk.Duration_Hours(jsii.Number(6))),
	}), fns.versionCheck)

	// Idle control node check every 15 minutes (optional).
	if fns.idleShutdown != nil {
		target(awsevents.NewRule(p.stack, jsii.String("IdleShutdownRule"), &awsevents.RuleProps{
			Description: jsii.String(fmt.Sprintf(
				"Check for idle control node every 15 min in %s (threshold: %dmin)",
				region, p.cfg.IdleShutdownMinutes)),
			Schedule: awsevents.Schedule_Rate(minutes(15)),
		}), fns.idleShutdown)
	}
}
