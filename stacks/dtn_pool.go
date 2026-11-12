package stacks

import (
	"encoding/base64"
	"fmt"
	"os"
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
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssnssubscriptions"
	"github.com/aws/aws-cdk-go/awscdk/v2/customresources"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/config"
)

// DTNPoolStackProps extends the basic props with Open Data bucket access
// and cache eviction configuration.
type DTNPoolStackProps struct {
	awscdk.StackProps
	RegionConfig       config.RegionConfig
	ControlStack       *ControlNodeStack
	OpenDataBuckets    []config.OpenDataBucket
	CacheEvictionHours int // default 72h if 0
	MaxDTNs            int // default WarmPoolSize+1 if 0
	// ThroughputScaleOutThreshold is the bytes/sec threshold for the multi-DTN
	// scale-out alarm (ElasticGCS/TransferThroughput). When the sum of
	// effective_bytes_per_second across all ACTIVE tasks exceeds this for 2
	// consecutive minutes, the scaleout Lambda fires again to add another DTN.
	// 0 = use default (500 MB/s = 500_000_000 bytes/sec).
	ThroughputScaleOutThreshold int64

	// IdleShutdownMinutes > 0 enables the idle_shutdown Lambda and EventBridge rule.
	// 0 = disabled (control node runs continuously). See docs/ephemeral-control-node.md.
	IdleShutdownMinutes int
	MinUptimeMinutes    int // default 30 when IdleShutdownMinutes > 0
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

	region := props.RegionConfig.Name
	secretsPrefix := props.ControlStack.SecretsPrefix

	warmPoolSize := props.RegionConfig.WarmPoolSize
	if warmPoolSize <= 0 {
		warmPoolSize = 2
	}

	// MaxDTNs: maximum number of simultaneously running (InService) DTNs.
	// Defaults to warmPoolSize+1 if not set.
	maxDTNs := props.MaxDTNs
	if maxDTNs <= 0 {
		if props.RegionConfig.MaxDTNs > 0 {
			maxDTNs = props.RegionConfig.MaxDTNs
		} else {
			maxDTNs = warmPoolSize + 1
		}
	}

	// Total slots = maxDTNs (InService) + warmPoolSize (Warmed:Stopped).
	// Each slot needs a pre-created GCS node identity in Secrets Manager
	// and an SSM slot-lock parameter path (/elastic-gcs/{region}/slot-lock/{N}).
	totalSlots := maxDTNs + warmPoolSize

	cacheEvictionHours := props.CacheEvictionHours
	if cacheEvictionHours <= 0 {
		cacheEvictionHours = 72
	}

	// ThroughputScaleOutThreshold: bytes/sec above which a second (or third) DTN
	// is triggered. Falls back to the per-region config, then to the default of
	// 500 MB/s. For m6i.xlarge (12.5 Gbps NIC), 500 MB/s ≈ 32% utilization —
	// conservative enough to avoid thrash while ensuring headroom for new tasks.
	throughputThreshold := props.ThroughputScaleOutThreshold
	if throughputThreshold <= 0 && props.RegionConfig.ThroughputScaleOutMBps > 0 {
		throughputThreshold = int64(props.RegionConfig.ThroughputScaleOutMBps) * 1_000_000
	}
	if throughputThreshold <= 0 {
		throughputThreshold = 500_000_000 // 500 MB/s default
	}

	// ASG name is hardcoded so EventBridge lifecycle rules can filter on it
	// without relying on CDK tokens.
	asgName := fmt.Sprintf("elastic-gcs-dtn-%s", region)

	// -------------------------------------------------------------------------
	// DTN IAM Role
	// DTNs run the GCS S3 connector — they need the same S3 read access as
	// the control node, plus EC2 EIP association and lifecycle hook signaling.
	// -------------------------------------------------------------------------
	dtnRole := awsiam.NewRole(stack, jsii.String("DTNRole"), &awsiam.RoleProps{
		AssumedBy:   awsiam.NewServicePrincipal(jsii.String("ec2.amazonaws.com"), nil),
		Description: jsii.String("IAM role for Elastic GCS DTN instances"),
		RoleName:    jsii.String(fmt.Sprintf("elastic-gcs-dtn-%s", region)),
	})

	dtnRole.AddManagedPolicy(awsiam.ManagedPolicy_FromAwsManagedPolicyName(
		jsii.String("AmazonSSMManagedInstanceCore")))
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
		Resources: jsii.Strings(
			fmt.Sprintf("arn:aws:ec2:%s:%s:instance/*", region, *stack.Account()),
		),
		Conditions: &map[string]interface{}{
			"StringEquals": map[string]interface{}{
				"ec2:ResourceTag/aws:autoscaling:groupName": asgName,
			},
			"ForAllValues:StringEquals": map[string]interface{}{
				"aws:TagKeys": []string{"elastic-gcs:slot", "elastic-gcs:gcs-node-id"},
			},
		},
	}))

	// SSM slot-lock: atomic slot claim (PutParameter without --overwrite) and
	// release (DeleteParameter) on shutdown. Read access is already covered by
	// SSMParameterRead below (used to check for an existing lock on restart).
	dtnRole.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("SlotLockSSM"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"ssm:PutParameter",
			"ssm:DeleteParameter",
		),
		Resources: jsii.Strings(
			fmt.Sprintf("arn:aws:ssm:%s:*:parameter/elastic-gcs/%s/slot-lock/*", region, region),
		),
	}))

	// AutoScaling: signal lifecycle hook completion from boot script
	dtnRole.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("LifecycleHookComplete"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"autoscaling:CompleteLifecycleAction",
			"autoscaling:DescribeAutoScalingInstances",
		),
		Resources: jsii.Strings("*"),
	}))

	// CloudWatch: publish timing and health metrics
	dtnRole.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String("CloudWatchMetrics"),
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("cloudwatch:PutMetricData"),
		Resources: jsii.Strings("*"),
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
			fmt.Sprintf("arn:aws:ssm:%s:*:parameter/elastic-gcs/%s/*", region, region),
		),
	}))

	// -------------------------------------------------------------------------
	// DTN UserData
	// On first EC2 boot (when entering the warm pool), this script:
	//   1. Installs GCS if not pre-baked into the AMI
	//   2. Installs dtn_boot.sh + dtn_shutdown.sh + spot_interrupt_monitor.sh
	//   3. Installs and starts the elastic-gcs-dtn systemd service
	//   4. Installs the elastic-gcs-spot-monitor systemd service
	//
	// The systemd service runs dtn_boot.sh on every subsequent start (resume),
	// which handles first-boot vs warm-resume detection automatically.
	// -------------------------------------------------------------------------
	bootScriptBytes, err := os.ReadFile("scripts/userdata/dtn_boot.sh")
	if err != nil {
		panic(fmt.Sprintf("cannot read scripts/userdata/dtn_boot.sh: %v", err))
	}
	shutdownScriptBytes, err := os.ReadFile("scripts/userdata/dtn_shutdown.sh")
	if err != nil {
		panic(fmt.Sprintf("cannot read scripts/userdata/dtn_shutdown.sh: %v", err))
	}
	spotMonitorBytes, err := os.ReadFile("scripts/userdata/spot_interrupt_monitor.sh")
	if err != nil {
		panic(fmt.Sprintf("cannot read scripts/userdata/spot_interrupt_monitor.sh: %v", err))
	}

	// Strip comments and blank lines before encoding to stay within the 16 KB
	// EC2 UserData limit. The source scripts are kept readable with full comments;
	// only the embedded version is stripped. Shebang lines are preserved.
	bootScriptB64 := base64.StdEncoding.EncodeToString(stripShellComments(bootScriptBytes))
	shutdownScriptB64 := base64.StdEncoding.EncodeToString(stripShellComments(shutdownScriptBytes))
	spotMonitorB64 := base64.StdEncoding.EncodeToString(stripShellComments(spotMonitorBytes))

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
ExecStart=/opt/elastic-gcs/scripts/dtn_boot.sh
ExecStop=/opt/elastic-gcs/scripts/dtn_shutdown.sh
TimeoutStartSec=600
TimeoutStopSec=300
Environment=AWS_REGION=%s
Environment=SECRETS_PREFIX=%s
Environment=TOTAL_SLOTS=%d

[Install]
WantedBy=multi-user.target
`, region, secretsPrefix, totalSlots)
	svcB64 := base64.StdEncoding.EncodeToString([]byte(svcContent))

	// Spot interruption monitor — polls IMDS every 5 seconds for a spot
	// termination/stop notice and disables the GCS node before shutdown.
	// Harmless on on-demand instances (IMDS returns 404 for spot endpoint).
	// Tied to elastic-gcs-dtn.service lifecycle via BindsTo.
	spotMonitorSvcContent := `[Unit]
Description=Spot Instance Interruption Monitor for Elastic GCS DTN
After=elastic-gcs-dtn.service
BindsTo=elastic-gcs-dtn.service

[Service]
Type=simple
ExecStart=/opt/elastic-gcs/scripts/spot_interrupt_monitor.sh
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=elastic-gcs-dtn.service
`
	spotMonitorSvcB64 := base64.StdEncoding.EncodeToString([]byte(spotMonitorSvcContent))

	ud := awsec2.UserData_ForLinux(nil)
	ud.AddCommands(
		// Install SSM Agent first — Rocky Linux 9 does not include it by default.
		jsii.String("dnf install -y https://s3.amazonaws.com/ec2-downloads-windows/SSMAgent/latest/linux_amd64/amazon-ssm-agent.rpm 2>/dev/null || true"),
		jsii.String("systemctl enable amazon-ssm-agent && systemctl start amazon-ssm-agent"),
		// Install AWS CLI if not already present (no-op if DTN AMI has it pre-baked)
		jsii.String(strings.TrimSpace(`
if ! command -v aws &>/dev/null; then
    echo ">>> Installing AWS CLI v2 (not found in AMI)..."
    dnf install -y unzip
    curl -s https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o /tmp/awscliv2.zip
    cd /tmp && unzip -q awscliv2.zip
    /tmp/aws/install
    ln -sf /usr/local/bin/aws /usr/bin/aws
    cd /
fi
`)),
		// Install GCS if not already present (no-op if DTN AMI has it pre-baked)
		jsii.String(strings.TrimSpace(`
if ! command -v globus-connect-server &>/dev/null; then
    echo ">>> Installing GCS (not found in AMI)..."
    . /etc/os-release
    OS_VER="${VERSION_ID%%.*}"
    dnf install -y https://dl.fedoraproject.org/pub/epel/epel-release-latest-${OS_VER}.noarch.rpm || true
    dnf install -y https://downloads.globus.org/globus-connect-server/stable/installers/repo/rpm/globus-repo-latest.noarch.rpm
    rpm --import /etc/pki/rpm-gpg/RPM-GPG-KEY-Globus-2024 || true
    dnf install -y globus-connect-server54
fi
`)),
		// Install scripts via base64 to avoid heredoc escaping issues
		jsii.String("mkdir -p /opt/elastic-gcs/scripts"),
		jsii.String(fmt.Sprintf("echo '%s' | base64 -d > /opt/elastic-gcs/scripts/dtn_boot.sh", bootScriptB64)),
		jsii.String("chmod +x /opt/elastic-gcs/scripts/dtn_boot.sh"),
		jsii.String(fmt.Sprintf("echo '%s' | base64 -d > /opt/elastic-gcs/scripts/dtn_shutdown.sh", shutdownScriptB64)),
		jsii.String("chmod +x /opt/elastic-gcs/scripts/dtn_shutdown.sh"),
		jsii.String(fmt.Sprintf("echo '%s' | base64 -d > /opt/elastic-gcs/scripts/spot_interrupt_monitor.sh", spotMonitorB64)),
		jsii.String("chmod +x /opt/elastic-gcs/scripts/spot_interrupt_monitor.sh"),
		// Install systemd services
		jsii.String(fmt.Sprintf("echo '%s' | base64 -d > /etc/systemd/system/elastic-gcs-dtn.service", svcB64)),
		jsii.String(fmt.Sprintf("echo '%s' | base64 -d > /etc/systemd/system/elastic-gcs-spot-monitor.service", spotMonitorSvcB64)),
		jsii.String("systemctl daemon-reload"),
		jsii.String("systemctl enable elastic-gcs-dtn elastic-gcs-spot-monitor"),
		jsii.String("systemctl start elastic-gcs-dtn"),
	)

	// -------------------------------------------------------------------------
	// Launch Template
	// Instance type from config (typically m6i.xlarge for x86-64, or m6g.xlarge
	// for Graviton). CPU type auto-detected from instance class suffix.
	// Uses the same security group as the control node (GCS requires same ports).
	// -------------------------------------------------------------------------
	dtnInstanceStr := props.RegionConfig.DTNInstance
	if dtnInstanceStr == "" {
		dtnInstanceStr = "m6i.xlarge"
	}

	// Use the pre-built DTN AMI (GCS 5.4 pre-installed) built by scripts/build_ami.sh.
	// AMIs are named "elastic-gcs-dtn-YYYYMMDD-HHMMSS" and owned by this account.
	// MachineImage_Lookup picks the latest matching AMI at synth time.
	// If no pre-built AMI exists yet, falls back to base Rocky Linux 9 (UserData installs GCS).
	account := awscdk.Stack_Of(stack).Account()
	dtnAMI := awsec2.MachineImage_Lookup(&awsec2.LookupMachineImageProps{
		Name:   jsii.String("elastic-gcs-dtn-*"),
		Owners: &[]*string{account},
	})
	// If lookup returns no result (pre-built AMI not yet created), fall back to Rocky Linux 9.
	// The UserData will install GCS on first boot in that case.
	_ = dtnAMI // may be overridden below if no pre-built AMI found
	if account == nil || *account == "" {
		dtnAMI = awsec2.MachineImage_Lookup(&awsec2.LookupMachineImageProps{
			Name:   jsii.String("Rocky-9-EC2-Base-*.x86_64"),
			Owners: jsii.Strings("792107900819"),
		})
	}

	ltProps := &awsec2.LaunchTemplateProps{
		LaunchTemplateName: jsii.String(fmt.Sprintf("elastic-gcs-dtn-%s", region)),
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
	if props.RegionConfig.KeyPairName != "" {
		ltProps.KeyPair = awsec2.KeyPair_FromKeyPairName(
			stack, jsii.String("DTNKeyPair"),
			jsii.String(props.RegionConfig.KeyPairName),
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
		MinCapacity:           jsii.Number(0),
		MaxCapacity:           jsii.Number(float64(maxDTNs)),
		DesiredCapacity:       jsii.Number(0),
		DefaultInstanceWarmup: awscdk.Duration_Seconds(jsii.Number(0)),
	}
	if props.RegionConfig.UseSpot {
		instancesDist := &awsautoscaling.InstancesDistribution{
			OnDemandBaseCapacity:                jsii.Number(0),
			OnDemandPercentageAboveBaseCapacity: jsii.Number(0),
			SpotAllocationStrategy:              awsautoscaling.SpotAllocationStrategy_PRICE_CAPACITY_OPTIMIZED,
		}
		if props.RegionConfig.SpotMaxPrice != "" {
			instancesDist.SpotMaxPrice = jsii.String(props.RegionConfig.SpotMaxPrice)
		}
		asgProps.MixedInstancesPolicy = &awsautoscaling.MixedInstancesPolicy{
			LaunchTemplate:        lt,
			InstancesDistribution: instancesDist,
		}
	} else {
		asgProps.LaunchTemplate = lt
	}
	asg := awsautoscaling.NewAutoScalingGroup(stack, jsii.String("DTNASG"), asgProps)

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
	if !props.RegionConfig.UseSpot {
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
						Resources: customresources.AwsCustomResourcePolicy_ANY_RESOURCE(),
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
						Resources: customresources.AwsCustomResourcePolicy_ANY_RESOURCE(),
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
		LifecycleHookName:   jsii.String("dtn-ready-hook"),
		HeartbeatTimeout:    awscdk.Duration_Minutes(jsii.Number(15)),
		DefaultResult:       awsautoscaling.DefaultResult_ABANDON,
	})
	asg.AddLifecycleHook(jsii.String("DTNTerminateHook"), &awsautoscaling.BasicLifecycleHookProps{
		LifecycleTransition: awsautoscaling.LifecycleTransition_INSTANCE_TERMINATING,
		LifecycleHookName:   jsii.String("dtn-disable-hook"),
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
		"REGION":         jsii.String(region),
		"SECRETS_PREFIX": jsii.String(secretsPrefix),
	}

	lifecycleFn := awslambda.NewFunction(stack, jsii.String("LifecycleFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(fmt.Sprintf("elastic-gcs-lifecycle-%s", region)),
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
	lifecycleFn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("LifecycleLambdaPerms"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"autoscaling:CompleteLifecycleAction",
			"autoscaling:DescribeAutoScalingInstances",
			"ssm:SendCommand",
			"ssm:GetCommandInvocation",
			"ssm:GetParameter",
			"ssm:DeleteParameter",
			"ec2:DescribeInstances",
			"ec2:CreateTags",
		),
		Resources: jsii.Strings("*"),
	}))

	scaleOutFn := awslambda.NewFunction(stack, jsii.String("ScaleOutFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(fmt.Sprintf("elastic-gcs-scaleout-%s", region)),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/scaleout"), nil),
		Timeout:      awscdk.Duration_Minutes(jsii.Number(5)),
		MemorySize:   jsii.Number(128),
		Environment: &map[string]*string{
			"ASG_NAME":       asg.AutoScalingGroupName(),
			"MAX_DTNS":       jsii.String(fmt.Sprintf("%d", maxDTNs)),
			"REGION":         jsii.String(region),
			"SECRETS_PREFIX": jsii.String(secretsPrefix),
		},
		LogRetention: awslogs.RetentionDays_ONE_WEEK,
	})
	scaleOutFn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("ScaleOutLambdaPerms"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"autoscaling:SetDesiredCapacity",
			"autoscaling:DescribeAutoScalingGroups",
			"autoscaling:DescribeWarmPool",
			"cloudwatch:PutMetricData",
		),
		Resources: jsii.Strings("*"),
	}))

	scaleInFn := awslambda.NewFunction(stack, jsii.String("ScaleInFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(fmt.Sprintf("elastic-gcs-scalein-%s", region)),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/scalein"), nil),
		Timeout:      awscdk.Duration_Minutes(jsii.Number(10)), // up to 8 min of ScalingActivityInProgress retries
		MemorySize:   jsii.Number(128),
		Environment: &map[string]*string{
			"ASG_NAME":         asg.AutoScalingGroupName(),
			"REGION":           jsii.String(region),
			"SECRETS_PREFIX":   jsii.String(secretsPrefix),
			"METRIC_NAMESPACE": jsii.String("ElasticGCS"),
		},
		LogRetention: awslogs.RetentionDays_ONE_WEEK,
	})
	scaleInFn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("ScaleInLambdaPerms"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"autoscaling:SetDesiredCapacity",
			"autoscaling:DescribeAutoScalingGroups",
			"cloudwatch:GetMetricStatistics",
			"cloudwatch:PutMetricData",
		),
		Resources: jsii.Strings("*"),
	}))

	cacheEvictionFn := awslambda.NewFunction(stack, jsii.String("CacheEvictionFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(fmt.Sprintf("elastic-gcs-cache-eviction-%s", region)),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/cache_eviction"), nil),
		Timeout:      awscdk.Duration_Minutes(jsii.Number(10)),
		MemorySize:   jsii.Number(128),
		Environment: &map[string]*string{
			"ASG_NAME":              asg.AutoScalingGroupName(),
			"REGION":                jsii.String(region),
			"SECRETS_PREFIX":        jsii.String(secretsPrefix),
			"CACHE_EVICTION_HOURS":  jsii.String(fmt.Sprintf("%d", cacheEvictionHours)),
			"CONTROL_NODE_ID_PARAM": jsii.String(fmt.Sprintf("/elastic-gcs/%s/control-node-id", region)),
		},
		LogRetention: awslogs.RetentionDays_ONE_WEEK,
	})
	cacheEvictionFn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("CacheEvictionLambdaPerms"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"autoscaling:DescribeWarmPool",
			"autoscaling:DescribeAutoScalingInstances",
			"ec2:TerminateInstances",
			"ec2:DescribeInstances",
			"ssm:SendCommand",
			"ssm:GetCommandInvocation",
			"ssm:GetParameter",
			"cloudwatch:PutMetricData",
		),
		Resources: jsii.Strings("*"),
	}))
	// Cache eviction also needs to release slot locks for instances it terminates,
	// since those instances may not run their shutdown script before being killed.
	cacheEvictionFn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String("CacheEvictionSlotLockCleanup"),
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("ssm:DeleteParameter"),
		Resources: jsii.Strings(fmt.Sprintf("arn:aws:ssm:%s:*:parameter/elastic-gcs/%s/slot-lock/*", region, region)),
	}))

	// Demand monitor Lambda: polls Globus Transfer API every 1 minute and
	// publishes ElasticGCS/ActiveTransfers metric. This metric drives both
	// the scale-out alarm (> 0) and the scale-in alarm (== 0 for 15 min).
	demandMonitorFn := awslambda.NewFunction(stack, jsii.String("DemandMonitorFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(fmt.Sprintf("elastic-gcs-demand-monitor-%s", region)),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/demand_monitor"), nil),
		Timeout:      awscdk.Duration_Minutes(jsii.Number(1)),
		MemorySize:   jsii.Number(128),
		Environment: &map[string]*string{
			"REGION":              jsii.String(region),
			"SECRETS_PREFIX":      jsii.String(secretsPrefix),
			"ENDPOINT_ID_PARAM":   jsii.String(fmt.Sprintf("/elastic-gcs/%s/endpoint-id", region)),
			"METRIC_NAMESPACE":    jsii.String("ElasticGCS"),
			"WAKER_FUNCTION_NAME": jsii.String(fmt.Sprintf("elastic-gcs-waker-%s", region)),
		},
		LogRetention: awslogs.RetentionDays_ONE_WEEK,
	})
	demandMonitorFn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("DemandMonitorLambdaPerms"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"ssm:GetParameter",
			"secretsmanager:GetSecretValue",
			"cloudwatch:PutMetricData",
			"lambda:InvokeFunction",
		),
		Resources: jsii.Strings("*"),
	}))

	// -------------------------------------------------------------------------
	// Version Check Lambda
	// Runs every 6h via EventBridge. Queries each running node for its GCS
	// version, then publishes ElasticGCS/GCSVersionDrift (0=healthy, >0=drift).
	// Only InService DTNs are checked — stopped warm pool instances can't run SSM.
	// -------------------------------------------------------------------------
	versionCheckFn := awslambda.NewFunction(stack, jsii.String("VersionCheckFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(fmt.Sprintf("elastic-gcs-version-check-%s", region)),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/version_check"), nil),
		Timeout:      awscdk.Duration_Minutes(jsii.Number(2)),
		MemorySize:   jsii.Number(128),
		Environment: &map[string]*string{
			"ASG_NAME":              asg.AutoScalingGroupName(),
			"REGION":                jsii.String(region),
			"CONTROL_NODE_ID_PARAM": jsii.String(fmt.Sprintf("/elastic-gcs/%s/control-node-id", region)),
		},
		LogRetention: awslogs.RetentionDays_ONE_WEEK,
	})
	versionCheckFn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("VersionCheckLambdaPerms"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"ssm:SendCommand",
			"ssm:GetCommandInvocation",
			"ssm:GetParameter",
			"autoscaling:DescribeAutoScalingGroups",
			"ec2:DescribeInstances",
			"cloudwatch:PutMetricData",
		),
		Resources: jsii.Strings("*"),
	}))

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
		TopicName:   jsii.String(fmt.Sprintf("elastic-gcs-scaleout-%s", region)),
		DisplayName: jsii.String("Elastic GCS Scale-Out Trigger"),
	})
	scaleOutTopic.AddSubscription(awssnssubscriptions.NewLambdaSubscription(scaleOutFn, nil))

	scaleInTopic := awssns.NewTopic(stack, jsii.String("ScaleInTopic"), &awssns.TopicProps{
		TopicName:   jsii.String(fmt.Sprintf("elastic-gcs-scalein-%s", region)),
		DisplayName: jsii.String("Elastic GCS Scale-In Trigger"),
	})
	scaleInTopic.AddSubscription(awssnssubscriptions.NewLambdaSubscription(scaleInFn, nil))

	// Scale-out alarm: fires when ElasticGCS/ActiveTransfers > 0 for 1 minute.
	scaleOutAlarmName := fmt.Sprintf("elastic-gcs-scaleout-%s", region)
	scaleOutAlarm := awscloudwatch.NewAlarm(stack, jsii.String("ScaleOutAlarm"), &awscloudwatch.AlarmProps{
		AlarmName:        jsii.String(scaleOutAlarmName),
		AlarmDescription: jsii.String("Trigger DTN scale-out when active Globus transfers detected"),
		Metric: awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String("ElasticGCS"),
			MetricName: jsii.String("ActiveTransfers"),
			DimensionsMap: &map[string]*string{
				"Region": jsii.String(region),
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
	scaleInAlarmName := fmt.Sprintf("elastic-gcs-scalein-%s", region)
	scaleInAlarm := awscloudwatch.NewAlarm(stack, jsii.String("ScaleInAlarm"), &awscloudwatch.AlarmProps{
		AlarmName: jsii.String(scaleInAlarmName),
		AlarmDescription: jsii.String(
			"Trigger DTN scale-in when no active Globus transfers for 15 minutes"),
		Metric: awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String("ElasticGCS"),
			MetricName: jsii.String("ActiveTransfers"),
			DimensionsMap: &map[string]*string{
				"Region": jsii.String(region),
			},
			Period:    awscdk.Duration_Minutes(jsii.Number(5)),
			Statistic: jsii.String("Maximum"),
		}),
		Threshold:          jsii.Number(0),
		ComparisonOperator: awscloudwatch.ComparisonOperator_LESS_THAN_OR_EQUAL_TO_THRESHOLD,
		EvaluationPeriods:  jsii.Number(3), // 3 × 5min = 15 min with no active transfers
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING, // demand monitor down = don't scale in
	})
	scaleInAlarm.AddAlarmAction(awscloudwatchactions.NewSnsAction(scaleInTopic))

	// Throughput scale-out alarm: fires when TransferThroughput (sum of
	// effective_bytes_per_second across all ACTIVE tasks) exceeds the threshold
	// for 2 consecutive minutes. This drives 1→2 DTN and beyond. The 2-period
	// requirement (vs 1 for the first alarm) avoids spinning up a second DTN for
	// brief throughput spikes during task start/stop transitions.
	throughputAlarmName := fmt.Sprintf("elastic-gcs-scaleout-throughput-%s", region)
	throughputAlarm := awscloudwatch.NewAlarm(stack, jsii.String("ThroughputScaleOutAlarm"), &awscloudwatch.AlarmProps{
		AlarmName: jsii.String(throughputAlarmName),
		AlarmDescription: jsii.String(fmt.Sprintf(
			"Trigger additional DTN when transfer throughput exceeds %d MB/s for 2 minutes",
			throughputThreshold/1_000_000)),
		Metric: awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String("ElasticGCS"),
			MetricName: jsii.String("TransferThroughput"),
			DimensionsMap: &map[string]*string{
				"Region": jsii.String(region),
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

	// GCS Version Drift alarm: fires when GCSVersionDrift > 0 (any drift detected).
	// Uses the scaleOutTopic for operator notification (same SNS infrastructure).
	versionDriftAlarmName := fmt.Sprintf("elastic-gcs-version-drift-%s", region)
	versionDriftAlarm := awscloudwatch.NewAlarm(stack, jsii.String("VersionDriftAlarm"), &awscloudwatch.AlarmProps{
		AlarmName:        jsii.String(versionDriftAlarmName),
		AlarmDescription: jsii.String("GCS version drift detected — nodes running different GCS versions"),
		Metric: awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String("ElasticGCS"),
			MetricName: jsii.String("GCSVersionDrift"),
			DimensionsMap: &map[string]*string{
				"Region": jsii.String(region),
			},
			Period:    awscdk.Duration_Hours(jsii.Number(6)),
			Statistic: jsii.String("Maximum"),
		}),
		Threshold:          jsii.Number(0),
		ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
		EvaluationPeriods:  jsii.Number(1),
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
	})
	versionDriftAlarm.AddAlarmAction(awscloudwatchactions.NewSnsAction(scaleOutTopic))

	// -------------------------------------------------------------------------
	// CloudWatch Dashboard — ElasticGCS-{region}
	//
	// Three rows × three 8-wide columns = full 24-column CloudWatch layout.
	// Row 1: Transfer demand (active transfers, DTN scale, alarm status)
	// Row 2: Scaling activity (scale events, lifecycle latency, Lambda errors)
	// Row 3: Health (version drift, demand monitor, cache eviction)
	// -------------------------------------------------------------------------
	dashboard := awscloudwatch.NewDashboard(stack, jsii.String("Dashboard"), &awscloudwatch.DashboardProps{
		DashboardName: jsii.String(fmt.Sprintf("ElasticGCS-%s", region)),
	})

	// Row 1: Transfer demand
	dashboard.AddWidgets(
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("Active Transfers & Throughput"),
			Width:  jsii.Number(8),
			Height: jsii.Number(6),
			Left: &[]awscloudwatch.IMetric{
				awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
					Namespace:  jsii.String("ElasticGCS"),
					MetricName: jsii.String("ActiveTransfers"),
					DimensionsMap: &map[string]*string{
						"Region": jsii.String(region),
					},
					Period:    awscdk.Duration_Minutes(jsii.Number(1)),
					Statistic: jsii.String("Maximum"),
					Label:     jsii.String("Active Transfers"),
				}),
			},
			Right: &[]awscloudwatch.IMetric{
				awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
					Namespace:  jsii.String("ElasticGCS"),
					MetricName: jsii.String("TransferThroughput"),
					DimensionsMap: &map[string]*string{
						"Region": jsii.String(region),
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
			Alarms: &[]awscloudwatch.IAlarm{scaleOutAlarm, throughputAlarm, scaleInAlarm, versionDriftAlarm},
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
					Namespace:  jsii.String("ElasticGCS"),
					MetricName: jsii.String("GCSVersionDrift"),
					DimensionsMap: &map[string]*string{
						"Region": jsii.String(region),
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
		FunctionName: jsii.String(fmt.Sprintf("elastic-gcs-waker-%s", region)),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/waker"), nil),
		Timeout:      awscdk.Duration_Minutes(jsii.Number(6)),
		MemorySize:   jsii.Number(128),
		Environment: &map[string]*string{
			"ASG_NAME":              asg.AutoScalingGroupName(),
			"REGION":                jsii.String(region),
			"CONTROL_NODE_ID_PARAM": jsii.String(fmt.Sprintf("/elastic-gcs/%s/control-node-id", region)),
		},
		LogRetention: awslogs.RetentionDays_ONE_WEEK,
	})
	wakerFn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("WakerLambdaPerms"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"ec2:DescribeInstances",
			"ec2:StartInstances",
			"ssm:GetParameter",
			"ssm:SendCommand",
			"ssm:GetCommandInvocation",
			"ssm:DescribeInstanceInformation",
			"autoscaling:DescribeAutoScalingGroups",
			"autoscaling:DescribeWarmPool",
			"autoscaling:SetDesiredCapacity",
		),
		Resources: jsii.Strings("*"),
	}))

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
	if props.IdleShutdownMinutes > 0 {
		minUptime := props.MinUptimeMinutes
		if minUptime <= 0 {
			minUptime = 30
		}

		idleShutdownFn := awslambda.NewFunction(stack, jsii.String("IdleShutdownFn"), &awslambda.FunctionProps{
			FunctionName: jsii.String(fmt.Sprintf("elastic-gcs-idle-shutdown-%s", region)),
			Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
			Architecture: awslambda.Architecture_ARM_64(),
			Handler:      jsii.String("bootstrap"),
			Code:         awslambda.Code_FromAsset(jsii.String("lambda/idle_shutdown"), nil),
			Timeout:      awscdk.Duration_Minutes(jsii.Number(2)),
			MemorySize:   jsii.Number(128),
			Environment: &map[string]*string{
				"ASG_NAME":              asg.AutoScalingGroupName(),
				"REGION":                jsii.String(region),
				"CONTROL_NODE_ID_PARAM": jsii.String(fmt.Sprintf("/elastic-gcs/%s/control-node-id", region)),
				"IDLE_MINUTES":          jsii.String(fmt.Sprintf("%d", props.IdleShutdownMinutes)),
				"MIN_UPTIME_MINUTES":    jsii.String(fmt.Sprintf("%d", minUptime)),
				"METRIC_NAMESPACE":      jsii.String("ElasticGCS"),
			},
			LogRetention: awslogs.RetentionDays_ONE_WEEK,
		})
		idleShutdownFn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid:    jsii.String("IdleShutdownLambdaPerms"),
			Effect: awsiam.Effect_ALLOW,
			Actions: jsii.Strings(
				"ec2:DescribeInstances",
				"ec2:StopInstances",
				"ssm:GetParameter",
				"autoscaling:DescribeAutoScalingGroups",
				"autoscaling:DescribeWarmPool",
				"cloudwatch:GetMetricStatistics",
			),
			Resources: jsii.Strings("*"),
		}))

		awsevents.NewRule(stack, jsii.String("IdleShutdownRule"), &awsevents.RuleProps{
			Description: jsii.String(fmt.Sprintf(
				"Check for idle control node every 15 min in %s (threshold: %dmin)",
				region, props.IdleShutdownMinutes)),
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
		Value:       jsii.String(fmt.Sprintf("/aws/lambda/elastic-gcs-lifecycle-%s", region)),
		Description: jsii.String("CloudWatch log group for DTN lifecycle events"),
	})

	awscdk.NewCfnOutput(stack, jsii.String("WakerFunctionUrl"), &awscdk.CfnOutputProps{
		Value:       wakerURL.Url(),
		Description: jsii.String("Waker Lambda Function URL — POST here (or use aws-elastic-gcs-ctl wake) to start the control node"),
	})

	return stack
}

// stripShellComments removes comment lines (starting with #) and blank lines
// from a shell script, preserving the shebang on the first line if present.
// This keeps embedded UserData scripts under the 16 KB EC2 limit while
// allowing the source files to remain fully documented.
func stripShellComments(src []byte) []byte {
	lines := strings.Split(string(src), "\n")
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if i == 0 && strings.HasPrefix(trimmed, "#!") {
			out = append(out, line) // always keep shebang
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, line)
	}
	return []byte(strings.Join(out, "\n"))
}
