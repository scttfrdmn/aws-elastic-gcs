package stacks

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsautoscaling"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/config"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// DTNPoolStackProps extends the basic props with Open Data bucket access
// and cache eviction configuration.
type DTNPoolStackProps struct {
	awscdk.StackProps
	Deployment names.Deployment
	// Config is the resolved, validated deployment config (config.Resolve).
	Config          config.Resolved
	ControlStack    *ControlNodeStack
	OpenDataBuckets []config.OpenDataBucket

	// OperatorEmail, if set, is subscribed to the operator alert topic.
	OperatorEmail string
}

// dtnPool carries what every part of the DTN pool stack needs. Each concern
// lives in its own file:
//
//	dtn_iam.go       DTN instance role
//	dtn_userdata.go  UserData and systemd units
//	dtn_asg.go       launch template, ASG, warm pool custom resources, lifecycle hooks
//	dtn_lambdas.go   orchestration Lambdas and their IAM grants
//	dtn_events.go    EventBridge rules
//	dtn_alarms.go    SNS topics and CloudWatch alarms
//	dtn_dashboard.go CloudWatch dashboard
type dtnPool struct {
	stack         awscdk.Stack
	id            string // stack ID, used in export names
	props         *DTNPoolStackProps
	dep           names.Deployment
	cfg           config.Resolved
	secretsPrefix string
	// asgName is hardcoded so EventBridge lifecycle rules can filter on it
	// without relying on CDK tokens.
	asgName string
}

// scoped builds IAM statements confined to this deployment's instances and ASG (#35).
func (p *dtnPool) scoped(sid string) iamScope {
	return iamScope{sid: sid, region: p.dep.Region, account: *p.stack.Account(), asgName: p.asgName, depID: p.dep.ID()}
}

// NewDTNPoolStack creates the Phase 3 DTN pool infrastructure:
//   - IAM role for DTN instances
//   - EC2 Launch Template with dtn_boot.sh UserData
//   - Auto Scaling Group (min=0, max=MaxDTNs) with stopped warm pool
//   - Warm pool via CDK AwsCustomResource (bypasses CFN WarmPool resource bug)
//   - Lifecycle hooks for graceful GCS node enable/disable
//   - Lambda functions: lifecycle, scaleout, scalein, cache_eviction, demand_monitor
//   - EventBridge rules wiring it all together
//   - CloudWatch alarms for scale-out/in triggers (based on ActiveTransfers metric)
func NewDTNPoolStack(scope constructs.Construct, id string, props *DTNPoolStackProps) awscdk.Stack {
	var sprops awscdk.StackProps
	if props != nil {
		sprops = props.StackProps
	}
	stack := awscdk.NewStack(scope, &id, &sprops)

	dep := props.Deployment
	// Instances launched by the ASG inherit these tags (PropagateAtLaunch).
	awscdk.Tags_Of(stack).Add(jsii.String(names.TagDeployment), jsii.String(dep.ID()), nil)
	awscdk.Tags_Of(stack).Add(jsii.String(names.TagRegion), jsii.String(dep.Region), nil)

	// All sizing comes from config.Resolve, shared with the control stack (#49).
	p := &dtnPool{
		stack:         stack,
		id:            id,
		props:         props,
		dep:           dep,
		cfg:           props.Config,
		secretsPrefix: props.ControlStack.SecretsPrefix,
		asgName:       dep.Resource("dtn"),
	}
	if p.cfg.ThroughputDerivedFallback {
		awscdk.Annotations_Of(stack).AddWarningV2(jsii.String("elastic-gcs:throughputThreshold"),
			jsii.String(fmt.Sprintf("DTN type %s has no known sustained throughput; using a "+
				"500 MB/s scale-out threshold. Set throughput_scaleout_mbps for this region "+
				"(see docs/instance-sizing.md).", p.cfg.DTNInstance)), nil)
	}

	if p.cfg.Pool == "slots" {
		p.buildSlots() // slot controller instead of ASG + warm pool (#65)
		return stack
	}

	role := p.dtnRole()
	ud := p.dtnUserData(role)
	asg := p.dtnASG(p.dtnLaunchTemplate(role, ud))
	fns := p.lambdas(asg)
	p.eventRules(fns)
	alarms := p.alarms(fns)
	p.dashboard(fns, alarms)
	p.outputs(asg, fns, alarms)

	return stack
}

// outputs adds the stack outputs.
func (p *dtnPool) outputs(asg awsautoscaling.AutoScalingGroup, fns dtnLambdas, alarms dtnAlarms) {
	stack, id := p.stack, p.id
	awscdk.NewCfnOutput(stack, jsii.String("ASGName"), &awscdk.CfnOutputProps{
		Value:       asg.AutoScalingGroupName(),
		Description: jsii.String("DTN Auto Scaling Group name"),
		ExportName:  jsii.String(fmt.Sprintf("%s-ASGName", id)),
	})

	awscdk.NewCfnOutput(stack, jsii.String("WarmPoolSize"), &awscdk.CfnOutputProps{
		Value:       jsii.String(fmt.Sprintf("%d", p.cfg.WarmPoolSize)),
		Description: jsii.String("Number of DTN slots in the warm pool"),
	})

	awscdk.NewCfnOutput(stack, jsii.String("MaxDTNs"), &awscdk.CfnOutputProps{
		Value:       jsii.String(fmt.Sprintf("%d", p.cfg.MaxDTNs)),
		Description: jsii.String("Maximum number of simultaneously active DTNs"),
	})

	awscdk.NewCfnOutput(stack, jsii.String("ScaleOutAlarmName"), &awscdk.CfnOutputProps{
		Value:       jsii.String(alarms.scaleOutName),
		Description: jsii.String("CW alarm to trigger manual scale-out: aws cloudwatch set-alarm-state ..."),
	})

	awscdk.NewCfnOutput(stack, jsii.String("ScaleOutFunctionArn"), &awscdk.CfnOutputProps{
		Value:       fns.scaleOut.FunctionArn(),
		Description: jsii.String("Scale-out Lambda ARN — invoke directly to trigger scale-out"),
		ExportName:  jsii.String(fmt.Sprintf("%s-ScaleOutFnArn", id)),
	})

	awscdk.NewCfnOutput(stack, jsii.String("DTNLogGroup"), &awscdk.CfnOutputProps{
		Value:       jsii.String("/aws/lambda/" + p.dep.Resource("lifecycle")),
		Description: jsii.String("CloudWatch log group for DTN lifecycle events"),
	})

	awscdk.NewCfnOutput(stack, jsii.String("WakerFunctionUrl"), &awscdk.CfnOutputProps{
		Value:       fns.wakerURL.Url(),
		Description: jsii.String("Waker Lambda Function URL — POST here (or use aws-elastic-gcs-ctl wake) to start the control node"),
	})
}
