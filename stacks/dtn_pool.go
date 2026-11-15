package stacks

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsautoscaling"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatch"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatchactions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsevents"
	"github.com/aws/aws-cdk-go/awscdk/v2/awseventstargets"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awss3assets"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssnssubscriptions"
	"github.com/aws/aws-cdk-go/awscdk/v2/customresources"
	"github.com/aws/aws-cdk-go/awscdk/v2/interfaces/interfacesawscloudwatch"
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
	region := dep.Region
	secretsPrefix := props.ControlStack.SecretsPrefix
	// Instances launched by the ASG inherit these tags (PropagateAtLaunch).
	awscdk.Tags_Of(stack).Add(jsii.String(names.TagDeployment), jsii.String(dep.ID()), nil)
	awscdk.Tags_Of(stack).Add(jsii.String(names.TagRegion), jsii.String(region), nil)

	// All sizing comes from config.Resolve, shared with the control stack (#49).
	cfg := props.Config
	warmPoolSize := cfg.WarmPoolSize
	maxDTNs := cfg.MaxDTNs
	totalSlots := cfg.TotalSlots
	cacheEvictionHours := cfg.CacheEvictionHours
	dtnInstanceStr := cfg.DTNInstance
	throughputThreshold := cfg.ThroughputThresholdBps
	if cfg.ThroughputDerivedFallback {
		awscdk.Annotations_Of(stack).AddWarningV2(jsii.String("elastic-gcs:throughputThreshold"),
			jsii.String(fmt.Sprintf("DTN type %s has no known sustained throughput; using a "+
				"500 MB/s scale-out threshold. Set throughput_scaleout_mbps for this region "+
				"(see docs/instance-sizing.md).", dtnInstanceStr)), nil)
	}

	// ASG name is hardcoded so EventBridge lifecycle rules can filter on it
	// without relying on CDK tokens.
	asgName := dep.Resource("dtn")
	// scoped builds IAM statements confined to this deployment's instances and ASG (#35).
	scoped := func(sid string) iamScope {
		return iamScope{sid: sid, region: region, account: *stack.Account(), asgName: asgName, depID: dep.ID()}
	}

	// -------------------------------------------------------------------------
	// DTN IAM Role
	// DTNs run the GCS S3 connector — they need the same S3 read access as
	// the control node, plus EC2 EIP association and lifecycle hook signaling.
	// -------------------------------------------------------------------------
	dtnRole := awsiam.NewRole(stack, jsii.String("DTNRole"), &awsiam.RoleProps{
		AssumedBy:   awsiam.NewServicePrincipal(jsii.String("ec2.amazonaws.com"), nil),
		Description: jsii.String("IAM role for Elastic GCS DTN instances"),
		RoleName:    jsii.String(dep.Resource("dtn")),
	})

	// SSM agent permissions without GetParameter on "*" (#45).
	addSSMCore(dtnRole)
	dtnRole.AddManagedPolicy(awsiam.ManagedPolicy_FromAwsManagedPolicyName(
		jsii.String("CloudWatchAgentServerPolicy")))

	// S3 read for Open Data buckets — DTNs do the actual data movement
	if len(props.OpenDataBuckets) > 0 {
		bucketArns := make([]*string, 0, len(props.OpenDataBuckets)*2)
		for _, b := range props.OpenDataBuckets {
			bucketArns = append(bucketArns,
				jsii.String(fmt.Sprintf("arn:aws:s3:::%s", b.Bucket)),
				jsii.String(fmt.Sprintf("arn:aws:s3:::%s/*", b.Bucket)),
			)
		}
		dtnRole.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid:    jsii.String("OpenDataS3Read"),
			Effect: awsiam.Effect_ALLOW,
			Actions: jsii.Strings(
				"s3:GetObject",
				"s3:ListBucket",
				"s3:GetBucketLocation",
				"s3:GetBucketRequestPayment",
			),
			Resources: &bucketArns,
		}))
	}

	// Secrets Manager: read-only for deployment key and node identities
	dtnRole.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("SecretsManagerRead"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"secretsmanager:GetSecretValue",
			"secretsmanager:DescribeSecret",
		),
		Resources: jsii.Strings(
			fmt.Sprintf("arn:aws:secretsmanager:%s:*:secret:%s/*", region, secretsPrefix),
		),
	}))

	// EC2: dtn_boot.sh tags its own instance with its slot number and GCS node
	// ID (read by the cache eviction Lambda). Scope this to instances in this
	// ASG and to those two tag keys, so a compromised DTN can't tag arbitrary
	// instances or forge elastic-gcs:warm-pool-entry-time (written by the
	// lifecycle Lambda) to force evictions.
	dtnRole.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:     jsii.String("EC2CreateTags"),
		Effect:  awsiam.Effect_ALLOW,
		Actions: jsii.Strings("ec2:CreateTags"),
		// The instance making the request only (#43): a DTN can't rewrite another
		// DTN's gcs-node-id tag and misdirect cache-eviction deletes.
		Resources: jsii.Strings("${ec2:SourceInstanceARN}"),
		Conditions: &map[string]interface{}{
			"StringEquals": map[string]interface{}{
				"ec2:ResourceTag/aws:autoscaling:groupName": asgName,
			},
			"ForAllValues:StringEquals": map[string]interface{}{
				"aws:TagKeys": []string{names.TagSlot, names.TagGCSNodeID},
			},
		},
	}))

	// SSM locks (#43). Slot and setup locks are claimed atomically by creating
	// the parameter, never by overwriting one, so a DTN can't steal a lock it
	// doesn't hold. DTNs never release slot locks (the lifecycle and
	// cache-eviction Lambdas do, owner-checked); they release only the setup lock.
	dtnRole.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:     jsii.String("ClaimLocks"),
		Effect:  awsiam.Effect_ALLOW,
		Actions: jsii.Strings("ssm:PutParameter"),
		Resources: jsii.Strings(
			fmt.Sprintf("arn:aws:ssm:%s:*:parameter%s/%s/*", region, dep.ParamPrefix(), names.ParamSlotLock),
			fmt.Sprintf("arn:aws:ssm:%s:*:parameter%s/%s", region, dep.ParamPrefix(), names.ParamSetupLock),
		),
		Conditions: &map[string]interface{}{
			"StringNotEquals": map[string]interface{}{"ssm:Overwrite": "true"},
		},
	}))
	dtnRole.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String("ReleaseSetupLock"),
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("ssm:DeleteParameter"),
		Resources: jsii.Strings(fmt.Sprintf("arn:aws:ssm:%s:*:parameter%s/%s", region, dep.ParamPrefix(), names.ParamSetupLock)),
	}))

	// AutoScaling: signal lifecycle hook completion from boot script, on this
	// deployment's ASG only.
	dtnRole.AddToPolicy(scoped("LifecycleHook").onASG("autoscaling:CompleteLifecycleAction"))
	dtnRole.AddToPolicy(scoped("LifecycleHook").unscoped("autoscaling:DescribeAutoScalingInstances"))

	// CloudWatch: DTNs publish their own boot latency only, in a namespace of
	// their own, so they can't forge the demand metrics that drive scaling (#43).
	dtnRole.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String("CloudWatchMetrics"),
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("cloudwatch:PutMetricData"),
		Resources: jsii.Strings("*"),
		Conditions: &map[string]interface{}{
			"StringEquals": map[string]interface{}{"cloudwatch:namespace": names.DTNMetricNamespace},
		},
	}))

	// SSM Parameter Store: read endpoint ID and AMI ID
	dtnRole.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("SSMParameterRead"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"ssm:GetParameter",
			"ssm:GetParameters",
		),
		Resources: jsii.Strings(
			fmt.Sprintf("arn:aws:ssm:%s:*:parameter%s/*", region, dep.ParamPrefix()),
		),
	}))

	// -------------------------------------------------------------------------
	// DTN UserData
	// On first EC2 boot (when entering the warm pool), UserData:
	//   1. Downloads dtn_boot.sh, dtn_shutdown.sh and spot_interrupt_monitor.sh
	//      from the CDK asset bucket (S3 gateway endpoint; DTN role has read)
	//   2. Installs the elastic-gcs-dtn and elastic-gcs-spot-monitor units
	//   3. Starts elastic-gcs-dtn
	//
	// The scripts are S3 assets rather than inline, so UserData stays far below
	// the 16 KB EC2 limit however large the scripts grow. Changing a script
	// changes its asset key, and therefore the launch template.
	//
	// The DTN AMI (scripts/build_ami.sh) is mandatory and bakes in GCS, the AWS
	// CLI and the SSM agent, so nothing is installed here.
	//
	// The systemd service runs dtn_boot.sh on every subsequent start (resume),
	// which handles first-boot vs warm-resume detection automatically.
	// -------------------------------------------------------------------------
	const scriptDir = "/opt/elastic-gcs/scripts"
	scriptFiles := []string{"dtn_boot.sh", "dtn_shutdown.sh", "spot_interrupt_monitor.sh"}

	// Systemd service with region/secrets/slot-count env vars baked in.
	// DTN_SLOT_NUMBER is NOT included — the boot script claims a slot at runtime
	// via SSM Parameter Store atomic locking (PutParameter without --overwrite).
	svcContent := fmt.Sprintf(`[Unit]
Description=Elastic GCS DTN Lifecycle Manager
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=%[1]s/dtn_boot.sh
ExecStop=%[1]s/dtn_shutdown.sh
TimeoutStartSec=600
TimeoutStopSec=300
Environment=AWS_REGION=%[2]s
Environment=DEPLOYMENT=%[3]s
Environment=SECRETS_PREFIX=%[4]s
Environment=PARAM_PREFIX=%[5]s
Environment=TOTAL_SLOTS=%[6]d
Environment=ASG_NAME=%[7]s
Environment=LIFECYCLE_HOOK=%[8]s
Environment=METRIC_NAMESPACE=%[9]s

[Install]
WantedBy=multi-user.target
`, scriptDir, region, dep.Name, secretsPrefix, dep.ParamPrefix(), totalSlots, asgName, names.HookReady, names.DTNMetricNamespace)
	svcB64 := base64.StdEncoding.EncodeToString([]byte(svcContent))

	// Spot interruption monitor — polls IMDS every 5 seconds for a spot
	// termination/stop notice and disables the GCS node before shutdown.
	// Harmless on on-demand instances (IMDS returns 404 for spot endpoint).
	// Tied to elastic-gcs-dtn.service lifecycle via BindsTo.
	spotMonitorSvcContent := fmt.Sprintf(`[Unit]
Description=Spot Instance Interruption Monitor for Elastic GCS DTN
After=elastic-gcs-dtn.service
BindsTo=elastic-gcs-dtn.service

[Service]
Type=simple
ExecStart=%s/spot_interrupt_monitor.sh
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=elastic-gcs-dtn.service
`, scriptDir)
	spotMonitorSvcB64 := base64.StdEncoding.EncodeToString([]byte(spotMonitorSvcContent))

	ud := awsec2.UserData_ForLinux(nil)
	ud.AddCommands(
		jsii.String("set -euo pipefail"),
		jsii.String(`command -v aws >/dev/null || { echo "ERROR: AWS CLI missing from DTN AMI; rebuild it with scripts/build_ami.sh"; exit 1; }`),
		jsii.String("systemctl enable --now amazon-ssm-agent || true"),
	)
	for _, f := range scriptFiles {
		asset := awss3assets.NewAsset(stack, jsii.String("DTNScript-"+strings.TrimSuffix(f, ".sh")),
			&awss3assets.AssetProps{Path: jsii.String("scripts/userdata/" + f)})
		asset.GrantRead(dtnRole)
		local := ud.AddS3DownloadCommand(&awsec2.S3DownloadOptions{
			Bucket:    asset.Bucket(),
			BucketKey: asset.S3ObjectKey(),
			LocalFile: jsii.String(scriptDir + "/" + f),
			Region:    jsii.String(region),
		})
		ud.AddCommands(jsii.String("chmod 0755 " + *local))
	}
	ud.AddCommands(
		jsii.String(fmt.Sprintf("echo '%s' | base64 -d > /etc/systemd/system/elastic-gcs-dtn.service", svcB64)),
		jsii.String(fmt.Sprintf("echo '%s' | base64 -d > /etc/systemd/system/elastic-gcs-spot-monitor.service", spotMonitorSvcB64)),
		jsii.String("systemctl daemon-reload"),
		jsii.String("systemctl enable elastic-gcs-dtn elastic-gcs-spot-monitor"),
		jsii.String("systemctl start elastic-gcs-dtn"),
	)

	// -------------------------------------------------------------------------
	// Launch Template
	// Instance type from config (default c8in.2xlarge; see docs/instance-sizing.md).
	// Uses the same security group as the control node (GCS requires same ports).
	// -------------------------------------------------------------------------

	// Use the DTN AMI that scripts/build_ami.sh built for this exact GCS version.
	// It is resolved from a version-keyed SSM parameter at deploy time, not
	// looked up at synth time, so the app synthesizes before any AMI exists (the
	// control stack must be deployed first, because the AMI builder borrows its
	// networking). Deploying this stack fails until `make ami` has written the
	// parameter, so a DTN can never run a different GCS version than the pin.
	dtnAMI := awsec2.MachineImage_FromSsmParameter(jsii.String(dep.AMIParam(cfg.GCSVersion)), nil)

	ltProps := &awsec2.LaunchTemplateProps{
		LaunchTemplateName: jsii.String(dep.Resource("dtn")),
		InstanceType:       awsec2.NewInstanceType(jsii.String(dtnInstanceStr)),
		MachineImage:       dtnAMI,
		SecurityGroup:      props.ControlStack.SecurityGroup,
		Role:               dtnRole,
		UserData:           ud,
		RequireImdsv2:      jsii.Bool(true),
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
	if cfg.KeyPairName != "" {
		ltProps.KeyPair = awsec2.KeyPair_FromKeyPairName(
			stack, jsii.String("DTNKeyPair"),
			jsii.String(cfg.KeyPairName),
		)
	}
	// Note: Spot options are NOT set on the launch template. ASG warm pools do not
	// allow SpotOptions in the launch template; Spot must be configured via the
	// ASG's MixedInstancesPolicy (see below).
	lt := awsec2.NewLaunchTemplate(stack, jsii.String("DTNLaunchTemplate"), ltProps)

	// -------------------------------------------------------------------------
	// Auto Scaling Group
	// Min=0 so there are no running DTNs when idle (only stopped in warm pool).
	// MaxCapacity=maxDTNs caps the number of simultaneously active DTNs.
	// DefaultInstanceWarmup=0 disables the built-in warmup delay; our lifecycle
	// hook handles readiness signaling instead.
	//
	// Spot vs warm pool: AWS does not allow Spot instances with a warm pool
	// (API error 400). The two modes are mutually exclusive:
	//   use_spot=false  → warm pool (fast 30-60s scale-out, on-demand pricing)
	//   use_spot=true   → no warm pool (2-3 min cold-start, ~70% cost savings)
	// When UseSpot=true, warmPoolSize is forced to 0 (warm pool skipped).
	// -------------------------------------------------------------------------
	asgProps := &awsautoscaling.AutoScalingGroupProps{
		AutoScalingGroupName: jsii.String(asgName),
		Vpc:                  props.ControlStack.Vpc,
		VpcSubnets: &awsec2.SubnetSelection{
			SubnetType: awsec2.SubnetType_PUBLIC,
		},
		MinCapacity: jsii.Number(0),
		MaxCapacity: jsii.Number(float64(maxDTNs)),
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
	asg := awsautoscaling.NewAutoScalingGroup(stack, jsii.String("DTNASG"), asgProps)
	// DTNs carry elastic-gcs:role=dtn (propagated at launch); Lambdas that act on
	// DTNs require it, and those that act on the control node require "control" (#44).
	awscdk.Tags_Of(asg).Add(jsii.String(names.TagRole), jsii.String(names.RoleDTN), nil)

	// -------------------------------------------------------------------------
	// Warm Pool — created via CDK AwsCustomResource (putWarmPool API call).
	//
	// We use a custom resource instead of the native AWS::AutoScaling::WarmPool
	// CloudFormation resource type because the CFN resource has a known bug
	// where the "eventual consistency check" loops indefinitely, leaving the
	// stack stuck in CREATE_IN_PROGRESS even though the warm pool is created.
	//
	// The AwsCustomResource directly calls the AutoScaling SDK, bypassing CFN.
	// -------------------------------------------------------------------------
	warmPoolParams := &map[string]interface{}{
		"AutoScalingGroupName":     jsii.String(asgName),
		"MinSize":                  jsii.Number(float64(warmPoolSize)),
		"MaxGroupPreparedCapacity": jsii.Number(float64(warmPoolSize)),
		"PoolState":                jsii.String("Stopped"),
		"InstanceReusePolicy": map[string]interface{}{
			"ReuseOnScaleIn": jsii.Bool(true),
		},
	}
	// Warm pool management via CDK AwsCustomResource.
	//
	// Switching modes requires careful ordering to avoid a CFN race condition:
	//
	// use_spot=false → use_spot=true:
	//   The warm pool must be deleted BEFORE the ASG switches to MixedInstancesPolicy
	//   (Spot + warm pool is an AWS API hard error). SpotCleanupCR handles this:
	//   asg.Node().AddDependency(spotCleanupCR) makes CFN wait for deleteWarmPool
	//   before updating the ASG. IgnoreErrorCodesMatching=".*" makes it a no-op
	//   when no warm pool exists (first deploy with use_spot=true).
	//
	// use_spot=true → use_spot=false:
	//   No warm pool exists; ASG switches back to LaunchTemplate (always succeeds),
	//   then WarmPoolCR creates the warm pool. SpotCleanupCR is deleted (no OnDelete
	//   configured → no-op). WarmPoolCR OnDelete uses IgnoreErrorCodesMatching=".*"
	//   so stack deletion is idempotent whether or not a warm pool exists.
	deleteWarmPoolParams := &map[string]interface{}{
		"AutoScalingGroupName": jsii.String(asgName),
		"ForceDelete":          jsii.Bool(true),
	}
	if !cfg.UseSpot {
		warmPoolCR := customresources.NewAwsCustomResource(stack, jsii.String("WarmPoolCR"),
			&customresources.AwsCustomResourceProps{
				OnCreate: &customresources.AwsSdkCall{
					Service:    jsii.String("AutoScaling"),
					Action:     jsii.String("putWarmPool"),
					Parameters: warmPoolParams,
					PhysicalResourceId: customresources.PhysicalResourceId_Of(
						jsii.String(fmt.Sprintf("WarmPool-%s", asgName))),
				},
				OnUpdate: &customresources.AwsSdkCall{
					Service:    jsii.String("AutoScaling"),
					Action:     jsii.String("putWarmPool"),
					Parameters: warmPoolParams,
					PhysicalResourceId: customresources.PhysicalResourceId_Of(
						jsii.String(fmt.Sprintf("WarmPool-%s", asgName))),
				},
				OnDelete: &customresources.AwsSdkCall{
					Service:                  jsii.String("AutoScaling"),
					Action:                   jsii.String("deleteWarmPool"),
					Parameters:               deleteWarmPoolParams,
					IgnoreErrorCodesMatching: jsii.String(".*"),
					PhysicalResourceId: customresources.PhysicalResourceId_Of(
						jsii.String(fmt.Sprintf("WarmPool-%s", asgName))),
				},
				Policy: customresources.AwsCustomResourcePolicy_FromSdkCalls(
					&customresources.SdkCallsPolicyOptions{
						Resources: jsii.Strings(scoped("").asgARN()),
					}),
				InstallLatestAwsSdk: jsii.Bool(false),
			})
		// Warm pool must be created after the ASG exists.
		warmPoolCR.Node().AddDependency(asg)
	} else {
		// Spot mode: delete any existing warm pool before the ASG switches to
		// MixedInstancesPolicy. Parameters use the literal asgName string (not an
		// ASG ref) so SpotCleanupCR has no formal CFN dependency on the ASG.
		// asg.Node().AddDependency ensures the ASG update waits for this to finish.
		spotCleanupCR := customresources.NewAwsCustomResource(stack, jsii.String("SpotCleanupCR"),
			&customresources.AwsCustomResourceProps{
				OnCreate: &customresources.AwsSdkCall{
					Service:                  jsii.String("AutoScaling"),
					Action:                   jsii.String("deleteWarmPool"),
					Parameters:               deleteWarmPoolParams,
					IgnoreErrorCodesMatching: jsii.String(".*"),
					PhysicalResourceId: customresources.PhysicalResourceId_Of(
						jsii.String(fmt.Sprintf("SpotCleanup-%s", asgName))),
				},
				OnUpdate: &customresources.AwsSdkCall{
					Service:                  jsii.String("AutoScaling"),
					Action:                   jsii.String("deleteWarmPool"),
					Parameters:               deleteWarmPoolParams,
					IgnoreErrorCodesMatching: jsii.String(".*"),
					PhysicalResourceId: customresources.PhysicalResourceId_Of(
						jsii.String(fmt.Sprintf("SpotCleanup-%s", asgName))),
				},
				// No OnDelete: switching back to warm pool mode leaves no warm pool
				// to clean up (SpotCleanupCR already removed it).
				Policy: customresources.AwsCustomResourcePolicy_FromSdkCalls(
					&customresources.SdkCallsPolicyOptions{
						Resources: jsii.Strings(scoped("").asgARN()),
					}),
				InstallLatestAwsSdk: jsii.Bool(false),
			})
		// ASG Spot update must wait for warm pool deletion to complete.
		asg.Node().AddDependency(spotCleanupCR)
	}

	// -------------------------------------------------------------------------
	// Lifecycle Hooks
	//
	// LAUNCH hook (warm pool → active):
	//   dtn_boot.sh signals completion via CompleteLifecycleAction when the GCS
	//   node is active. DefaultResult=ABANDON terminates unhealthy instances
	//   rather than leaving broken nodes in the endpoint.
	//
	// TERMINATE hook (active → warm pool or termination):
	//   The lifecycle Lambda runs `globus-connect-server node disable` via SSM
	//   before signaling CONTINUE. Without this, ungraceful shutdown makes the
	//   endpoint unreliable (per GCS docs).
	// -------------------------------------------------------------------------
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

	// -------------------------------------------------------------------------
	// Lambda Functions
	// All built for arm64/provided.al2023. Run `make build-lambdas` before deploy.
	// Each function gets a dedicated log group with 1-week retention.
	// -------------------------------------------------------------------------
	commonLambdaEnv := &map[string]*string{
		"ASG_NAME":       asg.AutoScalingGroupName(),
		"SECRETS_PREFIX": jsii.String(secretsPrefix),
	}

	lifecycleFn := awslambda.NewFunction(stack, jsii.String("LifecycleFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(dep.Resource("lifecycle")),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/lifecycle"), nil),
		Timeout:      awscdk.Duration_Minutes(jsii.Number(6)), // > terminate hook heartbeat
		MemorySize:   jsii.Number(128),
		Environment:  commonLambdaEnv,
		LogRetention: awslogs.RetentionDays_ONE_WEEK,
	})
	// Lifecycle Lambda needs SSM (node disable + slot lock release), ASG (complete hook), EC2 (describe + CreateTags)
	lc := scoped("Lifecycle")
	grant(lifecycleFn, lc.readParams(dep.ParamPrefix()))
	grant(lifecycleFn, lc.runShellOnRole(names.RoleDTN)...)
	grant(lifecycleFn,
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
			Resources: jsii.Strings(fmt.Sprintf("arn:aws:ssm:%s:%s:parameter%s/*", region, *stack.Account(), dep.ParamPrefix())),
		}),
	)

	scaleOutFn := awslambda.NewFunction(stack, jsii.String("ScaleOutFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(dep.Resource("scaleout")),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/scaleout"), nil),
		Timeout:      awscdk.Duration_Minutes(jsii.Number(5)),
		MemorySize:   jsii.Number(128),
		Environment: &map[string]*string{
			"ASG_NAME":       asg.AutoScalingGroupName(),
			"MAX_DTNS":       jsii.String(fmt.Sprintf("%d", maxDTNs)),
			"SECRETS_PREFIX": jsii.String(secretsPrefix),
			"WARM_POOL_SIZE": jsii.String(fmt.Sprintf("%d", warmPoolSize)),
			// Only these alarms may scale out; anything else on the topic is ignored (#27).
			"SCALE_OUT_ALARMS": jsii.String(dep.Resource("scaleout") + "," + dep.Resource("scaleout-throughput")),
		},
		LogRetention: awslogs.RetentionDays_ONE_WEEK,
	})
	so := scoped("ScaleOut")
	grant(scaleOutFn,
		so.onASG("autoscaling:SetDesiredCapacity", "autoscaling:PutWarmPool"),
		so.unscoped("autoscaling:DescribeAutoScalingGroups", "autoscaling:DescribeWarmPool", "cloudwatch:PutMetricData"),
	)

	scaleInFn := awslambda.NewFunction(stack, jsii.String("ScaleInFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(dep.Resource("scalein")),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/scalein"), nil),
		Timeout:      awscdk.Duration_Minutes(jsii.Number(10)), // up to 8 min of ScalingActivityInProgress retries
		MemorySize:   jsii.Number(128),
		Environment: &map[string]*string{
			"ASG_NAME":         asg.AutoScalingGroupName(),
			"SECRETS_PREFIX":   jsii.String(secretsPrefix),
			"METRIC_NAMESPACE": jsii.String(names.MetricNamespace),
		},
		LogRetention: awslogs.RetentionDays_ONE_WEEK,
	})
	si := scoped("ScaleIn")
	grant(scaleInFn, si.readParams(dep.ParamPrefix()))
	grant(scaleInFn, si.runShellOnRole(names.RoleControl)...) // re-enable the control node before the last DTN goes (#71)
	grant(scaleInFn,
		si.onASG("autoscaling:SetDesiredCapacity"),
		si.unscoped("autoscaling:DescribeAutoScalingGroups", "cloudwatch:GetMetricStatistics", "cloudwatch:PutMetricData", "ec2:DescribeInstances"),
	)

	cacheEvictionFn := awslambda.NewFunction(stack, jsii.String("CacheEvictionFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(dep.Resource("cache-eviction")),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/cache_eviction"), nil),
		Timeout:      awscdk.Duration_Minutes(jsii.Number(10)),
		MemorySize:   jsii.Number(128),
		Environment: &map[string]*string{
			"ASG_NAME":              asg.AutoScalingGroupName(),
			"SECRETS_PREFIX":        jsii.String(secretsPrefix),
			"CACHE_EVICTION_HOURS":  jsii.String(fmt.Sprintf("%d", cacheEvictionHours)),
			"CONTROL_NODE_ID_PARAM": jsii.String(dep.Param(names.ParamControlNodeID)),
		},
		LogRetention: awslogs.RetentionDays_ONE_WEEK,
	})
	ce := scoped("CacheEviction")
	grant(cacheEvictionFn, ce.readParams(dep.ParamPrefix()))
	grant(cacheEvictionFn, ce.runShellOnRole(names.RoleControl)...)
	grant(cacheEvictionFn,
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
	cacheEvictionFn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String("CacheEvictionSlotLockCleanup"),
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("ssm:DeleteParameter"),
		Resources: jsii.Strings(fmt.Sprintf("arn:aws:ssm:%s:*:parameter%s/%s/*", region, dep.ParamPrefix(), names.ParamSlotLock)),
	}))

	// Demand monitor Lambda: polls Globus Transfer API every 1 minute and
	// publishes ElasticGCS/ActiveTransfers metric. This metric drives both
	// the scale-out alarm (> 0) and the scale-in alarm (== 0 for 15 min).
	demandMonitorFn := awslambda.NewFunction(stack, jsii.String("DemandMonitorFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(dep.Resource("demand-monitor")),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/demand_monitor"), nil),
		Timeout:      awscdk.Duration_Minutes(jsii.Number(1)),
		MemorySize:   jsii.Number(128),
		Environment: &map[string]*string{
			"SECRETS_PREFIX":    jsii.String(secretsPrefix),
			"ENDPOINT_ID_PARAM": jsii.String(dep.Param(names.ParamEndpointID)),
			"METRIC_NAMESPACE":  jsii.String(names.MetricNamespace),
			// Demand-driven scale-up (#26): desired = ceil(throughput / threshold).
			"ASG_NAME":                 jsii.String(asgName),
			"MAX_DTNS":                 jsii.String(fmt.Sprintf("%d", maxDTNs)),
			"THROUGHPUT_THRESHOLD_BPS": jsii.String(fmt.Sprintf("%d", throughputThreshold)),
			"WARM_POOL_SIZE":           jsii.String(fmt.Sprintf("%d", warmPoolSize)),
		},
		LogRetention: awslogs.RetentionDays_ONE_WEEK,
	})
	demandMonitorFn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("DemandMonitorLambdaPerms"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"cloudwatch:PutMetricData",
			"autoscaling:DescribeAutoScalingGroups",
		),
		Resources: jsii.Strings("*"),
	}))
	demandMonitorFn.AddToRolePolicy(scoped("DemandMonitor").onASG("autoscaling:SetDesiredCapacity", "autoscaling:PutWarmPool"))
	demandMonitorFn.AddToRolePolicy(scoped("DemandMonitorWarm").unscoped("autoscaling:DescribeWarmPool"))
	demandMonitorFn.AddToRolePolicy(scoped("DemandMonitor").readParams(dep.ParamPrefix()))
	// Globus client credentials: this deployment's secrets only, not every
	// secret in the account (part of #44).
	demandMonitorFn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:     jsii.String("DemandMonitorGlobusSecrets"),
		Effect:  awsiam.Effect_ALLOW,
		Actions: jsii.Strings("secretsmanager:GetSecretValue"),
		Resources: jsii.Strings(
			fmt.Sprintf("arn:aws:secretsmanager:%s:%s:secret:%s/*", region, *stack.Account(), secretsPrefix),
		),
	}))

	// -------------------------------------------------------------------------
	// Version Check Lambda
	// Runs every 6h via EventBridge. Queries each running node for its GCS
	// version, then publishes ElasticGCS/GCSVersionDrift (0=healthy, >0=drift).
	// Only InService DTNs are checked — stopped warm pool instances can't run SSM.
	// -------------------------------------------------------------------------
	versionCheckFn := awslambda.NewFunction(stack, jsii.String("VersionCheckFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(dep.Resource("version-check")),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/version_check"), nil),
		Timeout:      awscdk.Duration_Minutes(jsii.Number(2)),
		MemorySize:   jsii.Number(128),
		Environment: &map[string]*string{
			"ASG_NAME":              asg.AutoScalingGroupName(),
			"CONTROL_NODE_ID_PARAM": jsii.String(dep.Param(names.ParamControlNodeID)),
		},
		LogRetention: awslogs.RetentionDays_ONE_WEEK,
	})
	vc := scoped("VersionCheck")
	grant(versionCheckFn, vc.readParams(dep.ParamPrefix()))
	grant(versionCheckFn, vc.runShell()...)
	grant(versionCheckFn, vc.unscoped(
		"autoscaling:DescribeAutoScalingGroups",
		"ec2:DescribeInstances",
		"cloudwatch:PutMetricData",
	))

	// -------------------------------------------------------------------------
	// EventBridge: ASG Lifecycle Hook Events → Lifecycle Lambda
	// ASG publishes lifecycle events to EventBridge automatically.
	// We filter to only our ASG by name (hardcoded to avoid CDK token issues).
	// -------------------------------------------------------------------------
	lifecycleDetail := map[string]interface{}{
		"AutoScalingGroupName": []interface{}{asgName},
	}
	lifecycleRule := awsevents.NewRule(stack, jsii.String("LifecycleRule"), &awsevents.RuleProps{
		Description: jsii.String(fmt.Sprintf("ASG lifecycle hook events for %s", asgName)),
		EventPattern: &awsevents.EventPattern{
			Source: jsii.Strings("aws.autoscaling"),
			DetailType: jsii.Strings(
				"EC2 Instance-launch Lifecycle Action",
				"EC2 Instance-terminate Lifecycle Action",
			),
			Detail: &lifecycleDetail,
		},
	})
	lifecycleRule.AddTarget(awseventstargets.NewLambdaFunction(lifecycleFn, nil))

	// -------------------------------------------------------------------------
	// EventBridge: Daily Cache Eviction Schedule
	// Terminates warm pool instances stopped for longer than cacheEvictionHours.
	// -------------------------------------------------------------------------
	cacheEvictionRule := awsevents.NewRule(stack, jsii.String("CacheEvictionRule"), &awsevents.RuleProps{
		Description: jsii.String(fmt.Sprintf(
			"Evict warm pool instances stopped > %dh in %s", cacheEvictionHours, region)),
		Schedule: awsevents.Schedule_Rate(awscdk.Duration_Hours(jsii.Number(24))),
	})
	cacheEvictionRule.AddTarget(awseventstargets.NewLambdaFunction(cacheEvictionFn, nil))

	// -------------------------------------------------------------------------
	// EventBridge: Demand Monitor Schedule (every 1 minute)
	// Note: 1 minute is the AWS minimum for EventBridge rate() expressions.
	// Sub-minute polling would require Step Functions or SQS-based self-chaining.
	// -------------------------------------------------------------------------
	demandMonitorRule := awsevents.NewRule(stack, jsii.String("DemandMonitorRule"), &awsevents.RuleProps{
		Description: jsii.String(fmt.Sprintf(
			"Poll Globus Transfer API for active transfers every 1 min in %s", region)),
		Schedule: awsevents.Schedule_Rate(awscdk.Duration_Minutes(jsii.Number(1))),
	})
	demandMonitorRule.AddTarget(awseventstargets.NewLambdaFunction(demandMonitorFn, nil))

	// -------------------------------------------------------------------------
	// EventBridge: Version Check Schedule (every 6 hours)
	// -------------------------------------------------------------------------
	versionCheckRule := awsevents.NewRule(stack, jsii.String("VersionCheckRule"), &awsevents.RuleProps{
		Description: jsii.String(fmt.Sprintf("GCS version drift check every 6h in %s", region)),
		Schedule:    awsevents.Schedule_Rate(awscdk.Duration_Hours(jsii.Number(6))),
	})
	versionCheckRule.AddTarget(awseventstargets.NewLambdaFunction(versionCheckFn, nil))

	// -------------------------------------------------------------------------
	// CloudWatch Alarms + SNS → Scale-Out / Scale-In Lambdas
	//
	// Scale-out uses two alarms in sequence:
	//   1. ActiveTransfers alarm  — fires when ANY transfer is active (0→1 DTN)
	//   2. TransferThroughput alarm — fires when endpoint is saturating (1→2 DTN, etc.)
	//
	// Both route to the same scaleout Lambda via the same SNS topic; the Lambda
	// increments DesiredCapacity by 1 per invocation and caps at MAX_DTNS.
	//
	// Scale-in uses ActiveTransfers == 0 for 15 minutes → scalein Lambda.
	//
	// TreatMissingData=NOT_BREACHING on all alarms: if demand_monitor is
	// unavailable (e.g. during a deploy), don't trigger false scale events.
	// -------------------------------------------------------------------------
	scaleOutTopic := awssns.NewTopic(stack, jsii.String("ScaleOutTopic"), &awssns.TopicProps{
		TopicName:   jsii.String(dep.Resource("scaleout")),
		DisplayName: jsii.String("Elastic GCS Scale-Out Trigger"),
	})
	scaleOutTopic.AddSubscription(awssnssubscriptions.NewLambdaSubscription(scaleOutFn, nil))

	scaleInTopic := awssns.NewTopic(stack, jsii.String("ScaleInTopic"), &awssns.TopicProps{
		TopicName:   jsii.String(dep.Resource("scalein")),
		DisplayName: jsii.String("Elastic GCS Scale-In Trigger"),
	})
	scaleInTopic.AddSubscription(awssnssubscriptions.NewLambdaSubscription(scaleInFn, nil))

	// Scale-out alarm: fires when ElasticGCS/ActiveTransfers > 0 for 1 minute.
	scaleOutAlarmName := dep.Resource("scaleout")
	scaleOutAlarm := awscloudwatch.NewAlarm(stack, jsii.String("ScaleOutAlarm"), &awscloudwatch.AlarmProps{
		AlarmName:        jsii.String(scaleOutAlarmName),
		AlarmDescription: jsii.String("Trigger DTN scale-out when active Globus transfers detected"),
		Metric: awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String(names.MetricNamespace),
			MetricName: jsii.String("ActiveTransfers"),
			DimensionsMap: &map[string]*string{
				"Region":     jsii.String(region),
				"Deployment": jsii.String(dep.Name),
			},
			Period:    awscdk.Duration_Minutes(jsii.Number(1)),
			Statistic: jsii.String("Maximum"),
		}),
		Threshold:          jsii.Number(0),
		ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
		EvaluationPeriods:  jsii.Number(1),
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
	})
	scaleOutAlarm.AddAlarmAction(awscloudwatchactions.NewSnsAction(scaleOutTopic))

	// Scale-in alarm: fires when ElasticGCS/ActiveTransfers == 0 for 15 min.
	// Three consecutive 5-minute periods with Maximum <= 0 means no active
	// transfers for at least 15 minutes.
	scaleInAlarmName := dep.Resource("scalein")
	scaleInAlarm := awscloudwatch.NewAlarm(stack, jsii.String("ScaleInAlarm"), &awscloudwatch.AlarmProps{
		AlarmName: jsii.String(scaleInAlarmName),
		AlarmDescription: jsii.String(
			"Trigger DTN scale-in when no active Globus transfers for 15 minutes"),
		Metric: awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String(names.MetricNamespace),
			MetricName: jsii.String("ActiveTransfers"),
			DimensionsMap: &map[string]*string{
				"Region":     jsii.String(region),
				"Deployment": jsii.String(dep.Name),
			},
			Period:    awscdk.Duration_Minutes(jsii.Number(5)),
			Statistic: jsii.String("Maximum"),
		}),
		Threshold:          jsii.Number(0),
		ComparisonOperator: awscloudwatch.ComparisonOperator_LESS_THAN_OR_EQUAL_TO_THRESHOLD,
		EvaluationPeriods:  jsii.Number(3),                               // 3 × 5min = 15 min with no active transfers
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING, // demand monitor down = don't scale in
	})
	scaleInAlarm.AddAlarmAction(awscloudwatchactions.NewSnsAction(scaleInTopic))

	// Throughput scale-out alarm: fires when TransferThroughput (sum of
	// effective_bytes_per_second across all ACTIVE tasks) exceeds the threshold
	// for 2 consecutive minutes. This drives 1→2 DTN and beyond. The 2-period
	// requirement (vs 1 for the first alarm) avoids spinning up a second DTN for
	// brief throughput spikes during task start/stop transitions.
	throughputAlarmName := dep.Resource("scaleout-throughput")
	throughputAlarm := awscloudwatch.NewAlarm(stack, jsii.String("ThroughputScaleOutAlarm"), &awscloudwatch.AlarmProps{
		AlarmName: jsii.String(throughputAlarmName),
		AlarmDescription: jsii.String(fmt.Sprintf(
			"Trigger additional DTN when transfer throughput exceeds %d MB/s for 2 minutes",
			throughputThreshold/1_000_000)),
		Metric: awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String(names.MetricNamespace),
			MetricName: jsii.String("TransferThroughput"),
			DimensionsMap: &map[string]*string{
				"Region":     jsii.String(region),
				"Deployment": jsii.String(dep.Name),
			},
			Period:    awscdk.Duration_Minutes(jsii.Number(1)),
			Statistic: jsii.String("Maximum"),
		}),
		Threshold:          jsii.Number(float64(throughputThreshold)),
		ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
		EvaluationPeriods:  jsii.Number(2),
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
	})
	throughputAlarm.AddAlarmAction(awscloudwatchactions.NewSnsAction(scaleOutTopic))

	// Operator notifications (#27): health alarms go here, never to the
	// scale-out topic. Subscribe an email with -c elastic-gcs:operatorEmail=...
	operatorTopic := awssns.NewTopic(stack, jsii.String("OperatorTopic"), &awssns.TopicProps{
		TopicName:   jsii.String(dep.Resource("operator")),
		DisplayName: jsii.String("Elastic GCS Operator Alerts"),
	})
	if props.OperatorEmail != "" {
		operatorTopic.AddSubscription(awssnssubscriptions.NewEmailSubscription(jsii.String(props.OperatorEmail), nil))
	}
	awscdk.NewCfnOutput(stack, jsii.String("OperatorTopicArn"), &awscdk.CfnOutputProps{
		Value:       operatorTopic.TopicArn(),
		Description: jsii.String("SNS topic for operator alerts (version drift)"),
	})

	// GCS Version Drift alarm: fires when GCSVersionDrift > 0 (any drift detected).
	versionDriftAlarmName := dep.Resource("version-drift")
	versionDriftAlarm := awscloudwatch.NewAlarm(stack, jsii.String("VersionDriftAlarm"), &awscloudwatch.AlarmProps{
		AlarmName:        jsii.String(versionDriftAlarmName),
		AlarmDescription: jsii.String("GCS version drift detected — nodes running different GCS versions"),
		Metric: awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String(names.MetricNamespace),
			MetricName: jsii.String("GCSVersionDrift"),
			DimensionsMap: &map[string]*string{
				"Region":     jsii.String(region),
				"Deployment": jsii.String(dep.Name),
			},
			Period:    awscdk.Duration_Hours(jsii.Number(6)),
			Statistic: jsii.String("Maximum"),
		}),
		Threshold:          jsii.Number(0),
		ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
		EvaluationPeriods:  jsii.Number(1),
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
	})
	versionDriftAlarm.AddAlarmAction(awscloudwatchactions.NewSnsAction(operatorTopic))

	// -------------------------------------------------------------------------
	// CloudWatch Dashboard — ElasticGCS-{region}
	//
	// Three rows × three 8-wide columns = full 24-column CloudWatch layout.
	// Row 1: Transfer demand (active transfers, DTN scale, alarm status)
	// Row 2: Scaling activity (scale events, lifecycle latency, Lambda errors)
	// Row 3: Health (version drift, demand monitor, cache eviction)
	// -------------------------------------------------------------------------
	dashboard := awscloudwatch.NewDashboard(stack, jsii.String("Dashboard"), &awscloudwatch.DashboardProps{
		DashboardName: jsii.String(dep.Dashboard()),
	})

	// Row 1: Transfer demand
	dashboard.AddWidgets(
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("Active Transfers & Throughput"),
			Width:  jsii.Number(8),
			Height: jsii.Number(6),
			Left: &[]awscloudwatch.IMetric{
				awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
					Namespace:  jsii.String(names.MetricNamespace),
					MetricName: jsii.String("ActiveTransfers"),
					DimensionsMap: &map[string]*string{
						"Region":     jsii.String(region),
						"Deployment": jsii.String(dep.Name),
					},
					Period:    awscdk.Duration_Minutes(jsii.Number(1)),
					Statistic: jsii.String("Maximum"),
					Label:     jsii.String("Active Transfers"),
				}),
			},
			Right: &[]awscloudwatch.IMetric{
				awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
					Namespace:  jsii.String(names.MetricNamespace),
					MetricName: jsii.String("TransferThroughput"),
					DimensionsMap: &map[string]*string{
						"Region":     jsii.String(region),
						"Deployment": jsii.String(dep.Name),
					},
					Period:    awscdk.Duration_Minutes(jsii.Number(1)),
					Statistic: jsii.String("Maximum"),
					Label:     jsii.String("Throughput (bytes/s)"),
				}),
			},
		}),
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("DTN Scale"),
			Width:  jsii.Number(8),
			Height: jsii.Number(6),
			Left: &[]awscloudwatch.IMetric{
				awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
					Namespace:  jsii.String("AWS/AutoScaling"),
					MetricName: jsii.String("GroupInServiceInstances"),
					DimensionsMap: &map[string]*string{
						"AutoScalingGroupName": jsii.String(asgName),
					},
					Period:    awscdk.Duration_Minutes(jsii.Number(1)),
					Statistic: jsii.String("Average"),
					Label:     jsii.String("In Service"),
				}),
				awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
					Namespace:  jsii.String("AWS/AutoScaling"),
					MetricName: jsii.String("WarmPoolMinSize"),
					DimensionsMap: &map[string]*string{
						"AutoScalingGroupName": jsii.String(asgName),
					},
					Period:    awscdk.Duration_Minutes(jsii.Number(1)),
					Statistic: jsii.String("Average"),
					Label:     jsii.String("Warm Pool"),
				}),
			},
		}),
		awscloudwatch.NewAlarmStatusWidget(&awscloudwatch.AlarmStatusWidgetProps{
			Title:  jsii.String("Alarm Status"),
			Width:  jsii.Number(8),
			Height: jsii.Number(6),
			Alarms: &[]interfacesawscloudwatch.IAlarmRef{scaleOutAlarm, throughputAlarm, scaleInAlarm, versionDriftAlarm},
		}),
	)

	// Row 2: Scaling activity
	dashboard.AddWidgets(
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("Scale Events (24h)"),
			Width:  jsii.Number(8),
			Height: jsii.Number(6),
			View:   awscloudwatch.GraphWidgetView_BAR,
			Left: &[]awscloudwatch.IMetric{
				scaleOutFn.MetricInvocations(&awscloudwatch.MetricOptions{
					Period:    awscdk.Duration_Hours(jsii.Number(1)),
					Statistic: jsii.String("Sum"),
					Label:     jsii.String("Scale-Out"),
				}),
				scaleInFn.MetricInvocations(&awscloudwatch.MetricOptions{
					Period:    awscdk.Duration_Hours(jsii.Number(1)),
					Statistic: jsii.String("Sum"),
					Label:     jsii.String("Scale-In"),
				}),
			},
		}),
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("Lifecycle Latency P99 (24h)"),
			Width:  jsii.Number(8),
			Height: jsii.Number(6),
			Left: &[]awscloudwatch.IMetric{
				lifecycleFn.MetricDuration(&awscloudwatch.MetricOptions{
					Period:    awscdk.Duration_Hours(jsii.Number(1)),
					Statistic: jsii.String("p99"),
					Label:     jsii.String("lifecycle p99"),
				}),
			},
		}),
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("Lambda Errors (24h)"),
			Width:  jsii.Number(8),
			Height: jsii.Number(6),
			View:   awscloudwatch.GraphWidgetView_BAR,
			Left: &[]awscloudwatch.IMetric{
				lifecycleFn.MetricErrors(&awscloudwatch.MetricOptions{
					Period:    awscdk.Duration_Hours(jsii.Number(1)),
					Statistic: jsii.String("Sum"),
					Label:     jsii.String("lifecycle"),
				}),
				scaleOutFn.MetricErrors(&awscloudwatch.MetricOptions{
					Period:    awscdk.Duration_Hours(jsii.Number(1)),
					Statistic: jsii.String("Sum"),
					Label:     jsii.String("scaleout"),
				}),
				scaleInFn.MetricErrors(&awscloudwatch.MetricOptions{
					Period:    awscdk.Duration_Hours(jsii.Number(1)),
					Statistic: jsii.String("Sum"),
					Label:     jsii.String("scalein"),
				}),
				cacheEvictionFn.MetricErrors(&awscloudwatch.MetricOptions{
					Period:    awscdk.Duration_Hours(jsii.Number(1)),
					Statistic: jsii.String("Sum"),
					Label:     jsii.String("cache-eviction"),
				}),
				demandMonitorFn.MetricErrors(&awscloudwatch.MetricOptions{
					Period:    awscdk.Duration_Hours(jsii.Number(1)),
					Statistic: jsii.String("Sum"),
					Label:     jsii.String("demand-monitor"),
				}),
				versionCheckFn.MetricErrors(&awscloudwatch.MetricOptions{
					Period:    awscdk.Duration_Hours(jsii.Number(1)),
					Statistic: jsii.String("Sum"),
					Label:     jsii.String("version-check"),
				}),
			},
		}),
	)

	// Row 3: Health
	dashboard.AddWidgets(
		awscloudwatch.NewSingleValueWidget(&awscloudwatch.SingleValueWidgetProps{
			Title:  jsii.String("GCS Version Drift"),
			Width:  jsii.Number(8),
			Height: jsii.Number(6),
			Metrics: &[]awscloudwatch.IMetric{
				awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
					Namespace:  jsii.String(names.MetricNamespace),
					MetricName: jsii.String("GCSVersionDrift"),
					DimensionsMap: &map[string]*string{
						"Region":     jsii.String(region),
						"Deployment": jsii.String(dep.Name),
					},
					Period:    awscdk.Duration_Hours(jsii.Number(6)),
					Statistic: jsii.String("Maximum"),
					Label:     jsii.String("Drift Count"),
				}),
			},
		}),
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("Demand Monitor (1h)"),
			Width:  jsii.Number(8),
			Height: jsii.Number(6),
			Left: &[]awscloudwatch.IMetric{
				demandMonitorFn.MetricInvocations(&awscloudwatch.MetricOptions{
					Period:    awscdk.Duration_Minutes(jsii.Number(5)),
					Statistic: jsii.String("Sum"),
					Label:     jsii.String("invocations"),
				}),
				demandMonitorFn.MetricErrors(&awscloudwatch.MetricOptions{
					Period:    awscdk.Duration_Minutes(jsii.Number(5)),
					Statistic: jsii.String("Sum"),
					Label:     jsii.String("errors"),
				}),
			},
		}),
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("Cache Eviction Duration (24h)"),
			Width:  jsii.Number(8),
			Height: jsii.Number(6),
			Left: &[]awscloudwatch.IMetric{
				cacheEvictionFn.MetricDuration(&awscloudwatch.MetricOptions{
					Period:    awscdk.Duration_Hours(jsii.Number(1)),
					Statistic: jsii.String("Maximum"),
					Label:     jsii.String("duration"),
				}),
			},
		}),
	)

	// -------------------------------------------------------------------------
	// Waker Lambda + Function URL
	//
	// Always created. Starts the control node on demand and optionally triggers
	// DTN scale-out.
	//
	// Auth: AWS_IAM — the Lambda service enforces SigV4 authentication before
	// the function code runs. Only IAM principals with lambda:InvokeFunctionUrl
	// permission on this function can call the URL.
	//
	// aws-elastic-gcs-ctl wake uses direct Lambda Invoke (also IAM-signed).
	// Globus Flows HTTP actions cannot call AWS_IAM URLs directly; see
	// docs/ephemeral-control-node.md for the proxy pattern.
	// -------------------------------------------------------------------------
	wakerFn := awslambda.NewFunction(stack, jsii.String("WakerFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(dep.Resource("waker")),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/waker"), nil),
		Timeout:      awscdk.Duration_Minutes(jsii.Number(6)),
		MemorySize:   jsii.Number(128),
		Environment: &map[string]*string{
			"ASG_NAME":              asg.AutoScalingGroupName(),
			"CONTROL_NODE_ID_PARAM": jsii.String(dep.Param(names.ParamControlNodeID)),
		},
		LogRetention: awslogs.RetentionDays_ONE_WEEK,
	})
	wk := scoped("Waker")
	grant(wakerFn, wk.readParams(dep.ParamPrefix()))
	grant(wakerFn, wk.runShellOnRole(names.RoleControl)...)
	grant(wakerFn,
		wk.onRoleInstances("StartInstances", names.RoleControl, "ec2:StartInstances"),
		wk.onASG("autoscaling:SetDesiredCapacity"),
		wk.unscoped(
			"ec2:DescribeInstances",
			"ssm:DescribeInstanceInformation",
			"autoscaling:DescribeAutoScalingGroups",
			"autoscaling:DescribeWarmPool",
		),
	)

	// Every Lambda derives its names, SSM paths and metric dimensions from
	// AWS_REGION (set by the runtime) and DEPLOYMENT via names.FromEnv.
	for _, fn := range []awslambda.Function{lifecycleFn, scaleOutFn, scaleInFn,
		cacheEvictionFn, demandMonitorFn, versionCheckFn, wakerFn} {
		fn.AddEnvironment(jsii.String("DEPLOYMENT"), jsii.String(dep.Name), nil)
	}

	// Lambda Function URL with AWS_IAM auth — enforced by the Lambda service
	// before code runs. Only callers with SigV4-signed requests and
	// lambda:InvokeFunctionUrl permission on this function are admitted.
	wakerURL := wakerFn.AddFunctionUrl(&awslambda.FunctionUrlOptions{
		AuthType: awslambda.FunctionUrlAuthType_AWS_IAM,
	})

	// -------------------------------------------------------------------------
	// Idle Shutdown Lambda (optional — only created when IdleShutdownMinutes > 0)
	//
	// Runs every 15 minutes via EventBridge. Stops the control node when all
	// conditions are met: desired=0, all warm pool instances Warmed:Stopped,
	// no active transfers for IdleShutdownMinutes, min uptime elapsed.
	//
	// IMPORTANT: When enabled, the endpoint becomes unreachable while stopped.
	// Read docs/ephemeral-control-node.md before enabling.
	// -------------------------------------------------------------------------
	if cfg.IdleShutdownMinutes > 0 {
		minUptime := cfg.MinUptimeMinutes

		idleShutdownFn := awslambda.NewFunction(stack, jsii.String("IdleShutdownFn"), &awslambda.FunctionProps{
			FunctionName: jsii.String(dep.Resource("idle-shutdown")),
			Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
			Architecture: awslambda.Architecture_ARM_64(),
			Handler:      jsii.String("bootstrap"),
			Code:         awslambda.Code_FromAsset(jsii.String("lambda/idle_shutdown"), nil),
			Timeout:      awscdk.Duration_Minutes(jsii.Number(6)), // up to ~4 min waiting for node disable + DNS
			MemorySize:   jsii.Number(128),
			Environment: &map[string]*string{
				"ASG_NAME":              asg.AutoScalingGroupName(),
				"CONTROL_NODE_ID_PARAM": jsii.String(dep.Param(names.ParamControlNodeID)),
				"IDLE_MINUTES":          jsii.String(fmt.Sprintf("%d", cfg.IdleShutdownMinutes)),
				"MIN_UPTIME_MINUTES":    jsii.String(fmt.Sprintf("%d", minUptime)),
				"METRIC_NAMESPACE":      jsii.String(names.MetricNamespace),
			},
			LogRetention: awslogs.RetentionDays_ONE_WEEK,
		})
		idleShutdownFn.AddEnvironment(jsii.String("DEPLOYMENT"), jsii.String(dep.Name), nil)
		is := scoped("IdleShutdown")
		grant(idleShutdownFn, is.readParams(dep.ParamPrefix()))
		grant(idleShutdownFn, is.runShellOnRole(names.RoleControl)...) // node disable before stop (#30)
		grant(idleShutdownFn,
			is.onRoleInstances("StopInstances", names.RoleControl, "ec2:StopInstances"),
			is.unscoped(
				"ec2:DescribeInstances",
				"autoscaling:DescribeAutoScalingGroups",
				"autoscaling:DescribeWarmPool",
				"cloudwatch:GetMetricStatistics",
			),
		)

		awsevents.NewRule(stack, jsii.String("IdleShutdownRule"), &awsevents.RuleProps{
			Description: jsii.String(fmt.Sprintf(
				"Check for idle control node every 15 min in %s (threshold: %dmin)",
				region, cfg.IdleShutdownMinutes)),
			Schedule: awsevents.Schedule_Rate(awscdk.Duration_Minutes(jsii.Number(15))),
		}).AddTarget(awseventstargets.NewLambdaFunction(idleShutdownFn, nil))
	}

	// -------------------------------------------------------------------------
	// Stack Outputs
	// -------------------------------------------------------------------------
	awscdk.NewCfnOutput(stack, jsii.String("ASGName"), &awscdk.CfnOutputProps{
		Value:       asg.AutoScalingGroupName(),
		Description: jsii.String("DTN Auto Scaling Group name"),
		ExportName:  jsii.String(fmt.Sprintf("%s-ASGName", id)),
	})

	awscdk.NewCfnOutput(stack, jsii.String("WarmPoolSize"), &awscdk.CfnOutputProps{
		Value:       jsii.String(fmt.Sprintf("%d", warmPoolSize)),
		Description: jsii.String("Number of DTN slots in the warm pool"),
	})

	awscdk.NewCfnOutput(stack, jsii.String("MaxDTNs"), &awscdk.CfnOutputProps{
		Value:       jsii.String(fmt.Sprintf("%d", maxDTNs)),
		Description: jsii.String("Maximum number of simultaneously active DTNs"),
	})

	awscdk.NewCfnOutput(stack, jsii.String("ScaleOutAlarmName"), &awscdk.CfnOutputProps{
		Value:       jsii.String(scaleOutAlarmName),
		Description: jsii.String("CW alarm to trigger manual scale-out: aws cloudwatch set-alarm-state ..."),
	})

	awscdk.NewCfnOutput(stack, jsii.String("ScaleOutFunctionArn"), &awscdk.CfnOutputProps{
		Value:       scaleOutFn.FunctionArn(),
		Description: jsii.String("Scale-out Lambda ARN — invoke directly to trigger scale-out"),
		ExportName:  jsii.String(fmt.Sprintf("%s-ScaleOutFnArn", id)),
	})

	awscdk.NewCfnOutput(stack, jsii.String("DTNLogGroup"), &awscdk.CfnOutputProps{
		Value:       jsii.String("/aws/lambda/" + dep.Resource("lifecycle")),
		Description: jsii.String("CloudWatch log group for DTN lifecycle events"),
	})

	awscdk.NewCfnOutput(stack, jsii.String("WakerFunctionUrl"), &awscdk.CfnOutputProps{
		Value:       wakerURL.Url(),
		Description: jsii.String("Waker Lambda Function URL — POST here (or use aws-elastic-gcs-ctl wake) to start the control node"),
	})

	return stack
}
