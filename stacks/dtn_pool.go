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
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/elastic-gcs/config"
)

// DTNPoolStackProps extends the basic props with Open Data bucket access
// and cache eviction configuration.
type DTNPoolStackProps struct {
	awscdk.StackProps
	RegionConfig       config.RegionConfig
	ControlStack       *ControlNodeStack
	OpenDataBuckets    []config.OpenDataBucket
	CacheEvictionHours int // default 72h if 0
}

// NewDTNPoolStack creates the Phase 3 DTN pool infrastructure:
//   - Elastic IPs (one per warm pool slot, persist through stop/start)
//   - IAM role for DTN instances
//   - EC2 Launch Template with dtn_boot.sh UserData
//   - Auto Scaling Group (min=0, max=WarmPoolSize+1) with stopped warm pool
//   - Lifecycle hooks for graceful GCS node enable/disable
//   - Lambda functions: lifecycle, scaleout, scalein, cache_eviction
//   - EventBridge rules wiring it all together
//   - CloudWatch alarms for scale-out/in triggers
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
	// Max capacity = warm pool + 1 active burst slot. The warm pool itself
	// holds up to warmPoolSize stopped instances; ASG desired goes 0→1→... .
	maxCapacity := warmPoolSize + 1

	cacheEvictionHours := props.CacheEvictionHours
	if cacheEvictionHours <= 0 {
		cacheEvictionHours = 72
	}

	// ASG name is hardcoded so EventBridge lifecycle rules can filter on it
	// without relying on CDK tokens.
	asgName := fmt.Sprintf("elastic-gcs-dtn-%s", region)

	// -------------------------------------------------------------------------
	// Elastic IPs
	// One EIP per possible instance slot (warmPoolSize + 1 for InService).
	// When an InService instance holds its EIP and warm pool instances hold
	// theirs, a replacement warm pool instance needs a free slot to claim.
	// So total EIPs needed = maxCapacity = warmPoolSize + 1.
	// Tagged with slot number so the DTN boot script can discover and claim
	// one at runtime (tag-based association).
	// Free when attached to a running instance; $3.60/month when unattached.
	// -------------------------------------------------------------------------
	for i := 1; i <= maxCapacity; i++ {
		awsec2.NewCfnEIP(stack, jsii.String(fmt.Sprintf("DTNSlot%dEIP", i)), &awsec2.CfnEIPProps{
			Domain: jsii.String("vpc"),
			Tags: &[]*awscdk.CfnTag{
				{Key: jsii.String("Name"), Value: jsii.String(fmt.Sprintf("elastic-gcs-dtn-%s-slot%d", region, i))},
				{Key: jsii.String("elastic-gcs:region"), Value: jsii.String(region)},
				{Key: jsii.String("elastic-gcs:role"), Value: jsii.String("dtn")},
				{Key: jsii.String("elastic-gcs:slot"), Value: jsii.String(fmt.Sprintf("%d", i))},
				{Key: jsii.String("elastic-gcs:asg"), Value: jsii.String(asgName)},
			},
		})
	}

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

	// EC2: associate EIP at boot, describe available EIPs for slot discovery
	dtnRole.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("EIPAndDescribe"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"ec2:AssociateAddress",
			"ec2:DescribeAddresses",
			"ec2:DescribeInstances",
			"ec2:DescribeTags",
		),
		Resources: jsii.Strings("*"),
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
	//   2. Installs dtn_boot.sh + dtn_shutdown.sh to /opt/elastic-gcs/scripts/
	//   3. Installs and starts the elastic-gcs-dtn systemd service
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

	// Strip comments and blank lines before encoding to stay within the 16 KB
	// EC2 UserData limit. The source scripts are kept readable with full comments;
	// only the embedded version is stripped. Shebang lines are preserved.
	bootScriptB64 := base64.StdEncoding.EncodeToString(stripShellComments(bootScriptBytes))
	shutdownScriptB64 := base64.StdEncoding.EncodeToString(stripShellComments(shutdownScriptBytes))

	// Systemd service with region/secrets env vars baked in. DTN_SLOT_NUMBER
	// and EIP_ALLOCATION_ID are NOT included — the boot script discovers them
	// at runtime via EC2 tag-based EIP discovery.
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

[Install]
WantedBy=multi-user.target
`, region, secretsPrefix)
	svcB64 := base64.StdEncoding.EncodeToString([]byte(svcContent))

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
		// Install and start systemd service
		jsii.String(fmt.Sprintf("echo '%s' | base64 -d > /etc/systemd/system/elastic-gcs-dtn.service", svcB64)),
		jsii.String("systemctl daemon-reload"),
		jsii.String("systemctl enable elastic-gcs-dtn"),
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
		SecurityGroup: props.ControlStack.SecurityGroup,
		Role:          dtnRole,
		UserData:      ud,
		RequireImdsv2: jsii.Bool(true),
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
	lt := awsec2.NewLaunchTemplate(stack, jsii.String("DTNLaunchTemplate"), ltProps)

	// -------------------------------------------------------------------------
	// Auto Scaling Group
	// Min=0 so there are no running DTNs when idle (only stopped in warm pool).
	// Max=warmPoolSize+1 allows one DTN beyond the warm pool to handle bursts.
	// DefaultInstanceWarmup=0 disables the built-in warmup delay; our lifecycle
	// hook handles readiness signaling instead.
	// -------------------------------------------------------------------------
	asg := awsautoscaling.NewAutoScalingGroup(stack, jsii.String("DTNASG"), &awsautoscaling.AutoScalingGroupProps{
		AutoScalingGroupName: jsii.String(asgName),
		Vpc:                  props.ControlStack.Vpc,
		VpcSubnets: &awsec2.SubnetSelection{
			SubnetType: awsec2.SubnetType_PUBLIC,
		},
		LaunchTemplate:      lt,
		MinCapacity:         jsii.Number(0),
		MaxCapacity:         jsii.Number(float64(maxCapacity)),
		DesiredCapacity:     jsii.Number(0),
		DefaultInstanceWarmup: awscdk.Duration_Seconds(jsii.Number(0)),
	})

	// -------------------------------------------------------------------------
	// Warm Pool — created via CLI post-deploy, NOT managed by CloudFormation.
	//
	// AWS::AutoScaling::WarmPool has a known CFN bug where the "eventual
	// consistency check" loops indefinitely, leaving the stack stuck in
	// CREATE_IN_PROGRESS. The warm pool is created correctly but CFN never
	// sees it as complete.
	//
	// Workaround: omit from CDK and create with the CLI after deploy:
	//   aws autoscaling put-warm-pool \
	//     --auto-scaling-group-name elastic-gcs-dtn-us-east-1 \
	//     --max-group-prepared-capacity 2 \
	//     --min-size 2 \
	//     --pool-state Stopped \
	//     --instance-reuse-policy ReuseOnScaleIn=true
	//
	// Use --min-size 2 (not 0) so the ASG proactively maintains 2 stopped
	// instances. With min-size=0, instances evicted from the warm pool are
	// not automatically replaced until a scale-in event returns one.
	// -------------------------------------------------------------------------

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
		"ASG_NAME":      asg.AutoScalingGroupName(),
		"REGION":        jsii.String(region),
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
	// Lifecycle Lambda needs SSM (node disable), ASG (complete hook), EC2 (describe)
	lifecycleFn.AddToRolePolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("LifecycleLambdaPerms"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"autoscaling:CompleteLifecycleAction",
			"autoscaling:DescribeAutoScalingInstances",
			"ssm:SendCommand",
			"ssm:GetCommandInvocation",
			"ec2:DescribeInstances",
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
			"MAX_DTNS":       jsii.String(fmt.Sprintf("%d", maxCapacity)),
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
		Timeout:      awscdk.Duration_Minutes(jsii.Number(5)),
		MemorySize:   jsii.Number(128),
		Environment:  commonLambdaEnv,
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
			"ASG_NAME":             asg.AutoScalingGroupName(),
			"REGION":               jsii.String(region),
			"SECRETS_PREFIX":       jsii.String(secretsPrefix),
			"CACHE_EVICTION_HOURS": jsii.String(fmt.Sprintf("%d", cacheEvictionHours)),
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
	// CloudWatch Alarms + SNS → Scale-Out / Scale-In Lambdas
	//
	// Scale-out trigger: custom metric ElasticGCS/ActiveTransfers > 0
	//   Published by the control node when it detects active Globus transfers.
	//   Use the Globus webhook path (future) or a polling script on the control
	//   node that calls `globus-connect-server endpoint show` and publishes.
	//
	// Scale-in trigger: ASG NetworkIn < threshold for 15 consecutive minutes
	//   When all running DTNs are idle, scale the ASG desired capacity back to 0.
	//   Instances move to the warm pool (stopped) via ReuseOnScaleIn.
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
	// The control node publishes this custom metric (implement in a monitoring
	// script or Lambda that polls the Globus Transfer API for active tasks).
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
			Statistic: jsii.String("Sum"),
		}),
		Threshold:          jsii.Number(0),
		ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
		EvaluationPeriods:  jsii.Number(1),
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
	})
	scaleOutAlarm.AddAlarmAction(awscloudwatchactions.NewSnsAction(scaleOutTopic))

	// Scale-in alarm: fires when DTN aggregate NetworkIn is very low for 15 min.
	// Threshold: 5MB aggregate over 5 min (3 periods = 15 min). If all running
	// DTNs together receive < 5MB in 15 min, the endpoint is effectively idle.
	scaleInAlarmName := fmt.Sprintf("elastic-gcs-scalein-%s", region)
	scaleInAlarm := awscloudwatch.NewAlarm(stack, jsii.String("ScaleInAlarm"), &awscloudwatch.AlarmProps{
		AlarmName: jsii.String(scaleInAlarmName),
		AlarmDescription: jsii.String(
			"Trigger DTN scale-in when network throughput is below idle threshold"),
		Metric: awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
			Namespace:  jsii.String("AWS/EC2"),
			MetricName: jsii.String("NetworkIn"),
			DimensionsMap: &map[string]*string{
				"AutoScalingGroupName": jsii.String(asgName),
			},
			Period:    awscdk.Duration_Minutes(jsii.Number(5)),
			Statistic: jsii.String("Sum"),
		}),
		Threshold:          jsii.Number(5_000_000), // 5 MB aggregate
		ComparisonOperator: awscloudwatch.ComparisonOperator_LESS_THAN_OR_EQUAL_TO_THRESHOLD,
		EvaluationPeriods:  jsii.Number(3), // 15 minutes of low activity
		TreatMissingData:   awscloudwatch.TreatMissingData_BREACHING, // no data = no DTNs needed
	})
	scaleInAlarm.AddAlarmAction(awscloudwatchactions.NewSnsAction(scaleInTopic))

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
