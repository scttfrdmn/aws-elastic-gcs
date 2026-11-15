package stacks

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsautoscaling"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/customresources"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// dtnLaunchTemplate creates the DTN launch template.
// Instance type from config (default c8in.2xlarge; see docs/instance-sizing.md).
// Uses the same security group as the control node (GCS requires same ports).
func (p *dtnPool) dtnLaunchTemplate(role awsiam.Role, ud awsec2.UserData) awsec2.LaunchTemplate {
	// Use the DTN AMI that scripts/build_ami.sh built for this exact GCS version.
	// It is resolved from a version-keyed SSM parameter at deploy time, not
	// looked up at synth time, so the app synthesizes before any AMI exists (the
	// control stack must be deployed first, because the AMI builder borrows its
	// networking). Deploying this stack fails until `make ami` has written the
	// parameter, so a DTN can never run a different GCS version than the pin.
	dtnAMI := awsec2.MachineImage_FromSsmParameter(jsii.String(p.dep.AMIParam(p.cfg.GCSVersion)), nil)

	ltProps := &awsec2.LaunchTemplateProps{
		LaunchTemplateName: jsii.String(p.dep.Resource("dtn")),
		InstanceType:       awsec2.NewInstanceType(jsii.String(p.cfg.DTNInstance)),
		MachineImage:       dtnAMI,
		SecurityGroup:      p.props.ControlStack.SecurityGroup,
		Role:               role,
		UserData:           ud,
		RequireImdsv2:      jsii.Bool(true),
		// One hop: containers or anything else forwarding through the DTN can't
		// reach the instance role credentials (#48).
		HttpPutResponseHopLimit: jsii.Number(1),
		// 20GB gp3 root volume — DTNs need less storage than control node
		BlockDevices: &[]*awsec2.BlockDevice{
			{
				DeviceName: jsii.String("/dev/sda1"),
				Volume: awsec2.BlockDeviceVolume_Ebs(jsii.Number(20), &awsec2.EbsDeviceOptions{
					VolumeType:          awsec2.EbsDeviceVolumeType_GP3,
					Encrypted:           jsii.Bool(true),
					DeleteOnTermination: jsii.Bool(true),
				}),
			},
		},
	}
	if p.cfg.KeyPairName != "" {
		ltProps.KeyPair = awsec2.KeyPair_FromKeyPairName(
			p.stack, jsii.String("DTNKeyPair"),
			jsii.String(p.cfg.KeyPairName),
		)
	}
	// Note: Spot options are NOT set on the launch template. ASG warm pools do not
	// allow SpotOptions in the launch template; Spot must be configured via the
	// ASG's MixedInstancesPolicy (see dtnASG).
	return awsec2.NewLaunchTemplate(p.stack, jsii.String("DTNLaunchTemplate"), ltProps)
}

// dtnASG creates the DTN Auto Scaling Group, its warm pool (or the spot-mode
// warm pool cleanup) and its lifecycle hooks.
//
// Min=0 so there are no running DTNs when idle (only stopped in warm pool).
// MaxCapacity=maxDTNs caps the number of simultaneously active DTNs.
// DefaultInstanceWarmup=0 disables the built-in warmup delay; our lifecycle
// hook handles readiness signaling instead.
//
// Spot vs warm pool: AWS does not allow Spot instances with a warm pool
// (API error 400). The two modes are mutually exclusive:
//
//	use_spot=false  → warm pool (fast 30-60s scale-out, on-demand pricing)
//	use_spot=true   → no warm pool (2-3 min cold-start, ~70% cost savings)
//
// When UseSpot=true, warmPoolSize is forced to 0 (warm pool skipped).
func (p *dtnPool) dtnASG(lt awsec2.LaunchTemplate) awsautoscaling.AutoScalingGroup {
	cfg := p.cfg
	asgProps := &awsautoscaling.AutoScalingGroupProps{
		AutoScalingGroupName: jsii.String(p.asgName),
		Vpc:                  p.props.ControlStack.Vpc,
		VpcSubnets: &awsec2.SubnetSelection{
			SubnetType: awsec2.SubnetType_PUBLIC,
		},
		MinCapacity: jsii.Number(0),
		MaxCapacity: jsii.Number(float64(cfg.MaxDTNs)),
		// No DesiredCapacity (#71): CloudFormation would reset the live group to it
		// on every deploy, stopping DTNs mid-transfer. Omitted, a new group starts
		// at MinCapacity and later deploys leave the scaling Lambdas' value alone.
		DefaultInstanceWarmup: awscdk.Duration_Seconds(jsii.Number(0)),
	}
	if cfg.UseSpot {
		instancesDist := &awsautoscaling.InstancesDistribution{
			OnDemandBaseCapacity:                jsii.Number(0),
			OnDemandPercentageAboveBaseCapacity: jsii.Number(0),
			SpotAllocationStrategy:              awsautoscaling.SpotAllocationStrategy_PRICE_CAPACITY_OPTIMIZED,
		}
		if cfg.SpotMaxPrice != "" {
			instancesDist.SpotMaxPrice = jsii.String(cfg.SpotMaxPrice)
		}
		asgProps.MixedInstancesPolicy = &awsautoscaling.MixedInstancesPolicy{
			LaunchTemplate:        lt,
			InstancesDistribution: instancesDist,
		}
	} else {
		asgProps.LaunchTemplate = lt
	}
	asg := awsautoscaling.NewAutoScalingGroup(p.stack, jsii.String("DTNASG"), asgProps)
	// DTNs carry elastic-gcs:role=dtn (propagated at launch); Lambdas that act on
	// DTNs require it, and those that act on the control node require "control" (#44).
	awscdk.Tags_Of(asg).Add(jsii.String(names.TagRole), jsii.String(names.RoleDTN), nil)

	p.warmPool(asg)
	p.lifecycleHooks(asg)
	return asg
}

// warmPoolCall is one AutoScaling SDK call made by the warm pool custom
// resources. ignoreErrors makes it a no-op when, e.g., there is no warm pool
// to delete.
func (p *dtnPool) warmPoolCall(action string, params *map[string]interface{}, physicalID string, ignoreErrors bool) *customresources.AwsSdkCall {
	call := &customresources.AwsSdkCall{
		Service:            jsii.String("AutoScaling"),
		Action:             jsii.String(action),
		Parameters:         params,
		PhysicalResourceId: customresources.PhysicalResourceId_Of(jsii.String(physicalID)),
	}
	if ignoreErrors {
		call.IgnoreErrorCodesMatching = jsii.String(".*")
	}
	return call
}

// warmPool manages the warm pool through a CDK AwsCustomResource (putWarmPool
// API call).
//
// We use a custom resource instead of the native AWS::AutoScaling::WarmPool
// CloudFormation resource type because the CFN resource has a known bug
// where the "eventual consistency check" loops indefinitely, leaving the
// stack stuck in CREATE_IN_PROGRESS even though the warm pool is created.
//
// The AwsCustomResource directly calls the AutoScaling SDK, bypassing CFN.
//
// Switching modes requires careful ordering to avoid a CFN race condition:
//
// use_spot=false → use_spot=true:
//
//	The warm pool must be deleted BEFORE the ASG switches to MixedInstancesPolicy
//	(Spot + warm pool is an AWS API hard error). SpotCleanupCR handles this:
//	asg.Node().AddDependency(spotCleanupCR) makes CFN wait for deleteWarmPool
//	before updating the ASG. IgnoreErrorCodesMatching=".*" makes it a no-op
//	when no warm pool exists (first deploy with use_spot=true).
//
// use_spot=true → use_spot=false:
//
//	No warm pool exists; ASG switches back to LaunchTemplate (always succeeds),
//	then WarmPoolCR creates the warm pool. SpotCleanupCR is deleted (no OnDelete
//	configured → no-op). WarmPoolCR OnDelete uses IgnoreErrorCodesMatching=".*"
//	so stack deletion is idempotent whether or not a warm pool exists.
func (p *dtnPool) warmPool(asg awsautoscaling.AutoScalingGroup) {
	asgName, warmPoolSize := p.asgName, p.cfg.WarmPoolSize
	warmPoolParams := &map[string]interface{}{
		"AutoScalingGroupName":     jsii.String(asgName),
		"MinSize":                  jsii.Number(float64(warmPoolSize)),
		"MaxGroupPreparedCapacity": jsii.Number(float64(warmPoolSize)),
		"PoolState":                jsii.String("Stopped"),
		"InstanceReusePolicy": map[string]interface{}{
			"ReuseOnScaleIn": jsii.Bool(true),
		},
	}
	deleteWarmPoolParams := &map[string]interface{}{
		"AutoScalingGroupName": jsii.String(asgName),
		"ForceDelete":          jsii.Bool(true),
	}
	policy := customresources.AwsCustomResourcePolicy_FromSdkCalls(
		&customresources.SdkCallsPolicyOptions{
			Resources: jsii.Strings(p.scoped("").asgARN()),
		})

	if !p.cfg.UseSpot {
		id := fmt.Sprintf("WarmPool-%s", asgName)
		warmPoolCR := customresources.NewAwsCustomResource(p.stack, jsii.String("WarmPoolCR"),
			&customresources.AwsCustomResourceProps{
				OnCreate:            p.warmPoolCall("putWarmPool", warmPoolParams, id, false),
				OnUpdate:            p.warmPoolCall("putWarmPool", warmPoolParams, id, false),
				OnDelete:            p.warmPoolCall("deleteWarmPool", deleteWarmPoolParams, id, true),
				Policy:              policy,
				InstallLatestAwsSdk: jsii.Bool(false),
			})
		// Warm pool must be created after the ASG exists.
		warmPoolCR.Node().AddDependency(asg)
		return
	}

	// Spot mode: delete any existing warm pool before the ASG switches to
	// MixedInstancesPolicy. Parameters use the literal asgName string (not an
	// ASG ref) so SpotCleanupCR has no formal CFN dependency on the ASG.
	// asg.Node().AddDependency ensures the ASG update waits for this to finish.
	id := fmt.Sprintf("SpotCleanup-%s", asgName)
	spotCleanupCR := customresources.NewAwsCustomResource(p.stack, jsii.String("SpotCleanupCR"),
		&customresources.AwsCustomResourceProps{
			OnCreate: p.warmPoolCall("deleteWarmPool", deleteWarmPoolParams, id, true),
			OnUpdate: p.warmPoolCall("deleteWarmPool", deleteWarmPoolParams, id, true),
			// No OnDelete: switching back to warm pool mode leaves no warm pool
			// to clean up (SpotCleanupCR already removed it).
			Policy:              policy,
			InstallLatestAwsSdk: jsii.Bool(false),
		})
	// ASG Spot update must wait for warm pool deletion to complete.
	asg.Node().AddDependency(spotCleanupCR)
}

// lifecycleHooks adds the DTN lifecycle hooks.
//
// LAUNCH hook (warm pool → active):
//
//	dtn_boot.sh signals completion via CompleteLifecycleAction when the GCS
//	node is active. DefaultResult=ABANDON terminates unhealthy instances
//	rather than leaving broken nodes in the endpoint.
//
// TERMINATE hook (active → warm pool or termination):
//
//	The lifecycle Lambda runs `globus-connect-server node disable` via SSM
//	before signaling CONTINUE. Without this, ungraceful shutdown makes the
//	endpoint unreliable (per GCS docs).
func (p *dtnPool) lifecycleHooks(asg awsautoscaling.AutoScalingGroup) {
	asg.AddLifecycleHook(jsii.String("DTNLaunchHook"), &awsautoscaling.BasicLifecycleHookProps{
		LifecycleTransition: awsautoscaling.LifecycleTransition_INSTANCE_LAUNCHING,
		LifecycleHookName:   jsii.String(names.HookReady),
		HeartbeatTimeout:    awscdk.Duration_Minutes(jsii.Number(15)),
		DefaultResult:       awsautoscaling.DefaultResult_ABANDON,
	})
	asg.AddLifecycleHook(jsii.String("DTNTerminateHook"), &awsautoscaling.BasicLifecycleHookProps{
		LifecycleTransition: awsautoscaling.LifecycleTransition_INSTANCE_TERMINATING,
		LifecycleHookName:   jsii.String(names.HookDisable),
		HeartbeatTimeout:    awscdk.Duration_Minutes(jsii.Number(5)),
		DefaultResult:       awsautoscaling.DefaultResult_CONTINUE,
	})
}
