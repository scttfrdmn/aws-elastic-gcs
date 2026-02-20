package stacks

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/elastic-gcs/config"
)

// ControlNodeStackProps defines configuration for the control node stack.
// Globus-specific settings come from CDK context (cdk.json or -c flags).
type ControlNodeStackProps struct {
	awscdk.StackProps
	RegionConfig         config.RegionConfig
	OpenDataBuckets      []config.OpenDataBucket

	// Globus endpoint configuration
	GlobusSubscriptionID string // Globus subscription ID (required for managed endpoint)
	GlobusOwnerIdentity  string // Globus identity of endpoint owner (e.g. user@globus.org)
	EndpointDisplayName  string // Human-readable endpoint name
	OrganizationName     string // Organization shown in Globus endpoint listing

	// SSH access restriction. Empty string allows 0.0.0.0/0 (not recommended for prod).
	AdminCIDR string
}

// ControlNodeStack is the CDK stack for the GCS control node.
// It exports Vpc, SecurityGroup, and ControlInstance for the DTN pool stack.
type ControlNodeStack struct {
	awscdk.Stack
	Vpc             awsec2.IVpc
	SecurityGroup   awsec2.ISecurityGroup
	ControlInstance awsec2.IInstance
	SecretsPrefix   string
}

// NewControlNodeStack creates the Phase 1 control node infrastructure:
//   - VPC with public subnets in 2 AZs (no NAT gateways)
//   - Security group for GCS (TCP 443, 50000-51000, SSH)
//   - IAM role with least-privilege S3/Secrets Manager/SSM/CloudWatch access
//   - EC2 t4g.medium (Graviton, 8GB RAM — GCS minimum) with AL2023 ARM64
//   - UserData that runs control_node_init.sh on first boot
func NewControlNodeStack(scope constructs.Construct, id string, props *ControlNodeStackProps) *ControlNodeStack {
	var sprops awscdk.StackProps
	if props != nil {
		sprops = props.StackProps
	}
	stack := awscdk.NewStack(scope, &id, &sprops)
	s := &ControlNodeStack{Stack: stack}

	region := props.RegionConfig.Name
	secretsPrefix := fmt.Sprintf("elastic-gcs/%s", region)
	s.SecretsPrefix = secretsPrefix

	// -------------------------------------------------------------------------
	// VPC
	// Public subnets only — GCS instances need direct internet access for
	// Globus service communication and GridFTP data channels. No NAT gateways
	// needed (and no NAT cost) since all instances have public IPs.
	// Two AZs for ASG spread in Phase 3.
	// -------------------------------------------------------------------------
	vpc := awsec2.NewVpc(stack, jsii.String("VPC"), &awsec2.VpcProps{
		MaxAzs: jsii.Number(2),
		SubnetConfiguration: &[]*awsec2.SubnetConfiguration{
			{
				Name:       jsii.String("Public"),
				SubnetType: awsec2.SubnetType_PUBLIC,
				CidrMask:   jsii.Number(24),
			},
		},
		NatGateways:        jsii.Number(0),
		EnableDnsSupport:   jsii.Bool(true),
		EnableDnsHostnames: jsii.Bool(true),
	})
	s.Vpc = vpc

	// -------------------------------------------------------------------------
	// Security Group
	// GCS requires:
	//   TCP 443 inbound:         GCS Manager (HTTPS control channel)
	//   TCP 50000-51000 inbound: GridFTP data channels
	//   TCP 443 outbound:        Communication with Globus service
	//   TCP 50000-51000 outbound: GridFTP to remote endpoints
	//   SSH (22):                Admin access, restricted to AdminCIDR
	// AllowAllOutbound covers the outbound requirements.
	// -------------------------------------------------------------------------
	sg := awsec2.NewSecurityGroup(stack, jsii.String("GCSSg"), &awsec2.SecurityGroupProps{
		Vpc:              vpc,
		Description:      jsii.String("Elastic GCS node security group - TCP 443 and 50000-51000"),
		AllowAllOutbound: jsii.Bool(true),
	})

	sg.AddIngressRule(
		awsec2.Peer_AnyIpv4(),
		awsec2.Port_Tcp(jsii.Number(443)),
		jsii.String("GCS Manager HTTPS"),
		jsii.Bool(false),
	)
	sg.AddIngressRule(
		awsec2.Peer_AnyIpv6(),
		awsec2.Port_Tcp(jsii.Number(443)),
		jsii.String("GCS Manager HTTPS IPv6"),
		jsii.Bool(false),
	)
	sg.AddIngressRule(
		awsec2.Peer_AnyIpv4(),
		awsec2.Port_TcpRange(jsii.Number(50000), jsii.Number(51000)),
		jsii.String("GridFTP data channels"),
		jsii.Bool(false),
	)
	sg.AddIngressRule(
		awsec2.Peer_AnyIpv6(),
		awsec2.Port_TcpRange(jsii.Number(50000), jsii.Number(51000)),
		jsii.String("GridFTP data channels IPv6"),
		jsii.Bool(false),
	)

	adminCIDR := props.AdminCIDR
	if adminCIDR == "" {
		adminCIDR = "0.0.0.0/0"
	}
	sg.AddIngressRule(
		awsec2.Peer_Ipv4(jsii.String(adminCIDR)),
		awsec2.Port_Tcp(jsii.Number(22)),
		jsii.String("SSH admin access"),
		jsii.Bool(false),
	)

	s.SecurityGroup = sg

	// -------------------------------------------------------------------------
	// IAM Role
	// Least-privilege policy for the control node:
	//   - S3 GetObject/ListBucket on Open Data buckets (for GCS S3 connector)
	//   - Secrets Manager: read Globus creds, write deployment key and node slots
	//   - SSM Parameter Store: write endpoint ID and instance ID
	//   - CloudWatch: publish custom ElasticGCS metrics
	//   - SSM Session Manager: for maintenance without SSH key
	// -------------------------------------------------------------------------
	role := awsiam.NewRole(stack, jsii.String("ControlNodeRole"), &awsiam.RoleProps{
		AssumedBy:   awsiam.NewServicePrincipal(jsii.String("ec2.amazonaws.com"), nil),
		Description: jsii.String("IAM role for Elastic GCS control node"),
		RoleName:    jsii.String(fmt.Sprintf("elastic-gcs-control-%s", region)),
	})

	// SSM Session Manager — enables SSH-free maintenance
	role.AddManagedPolicy(awsiam.ManagedPolicy_FromAwsManagedPolicyName(
		jsii.String("AmazonSSMManagedInstanceCore")))

	// CloudWatch agent
	role.AddManagedPolicy(awsiam.ManagedPolicy_FromAwsManagedPolicyName(
		jsii.String("CloudWatchAgentServerPolicy")))

	// S3 read access to Open Data buckets (for GCS S3 connector)
	if len(props.OpenDataBuckets) > 0 {
		bucketArns := make([]*string, 0, len(props.OpenDataBuckets)*2)
		for _, b := range props.OpenDataBuckets {
			bucketArns = append(bucketArns,
				jsii.String(fmt.Sprintf("arn:aws:s3:::%s", b.Bucket)),
				jsii.String(fmt.Sprintf("arn:aws:s3:::%s/*", b.Bucket)),
			)
		}
		role.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid:    jsii.String("OpenDataS3Read"),
			Effect: awsiam.Effect_ALLOW,
			Actions: jsii.Strings(
				"s3:GetObject",
				"s3:ListBucket",
				"s3:GetBucketLocation",
				"s3:GetBucketRequestPayment", // for requester-pays buckets
			),
			Resources: &bucketArns,
		}))
	}

	// Secrets Manager: read Globus credentials (pre-created by operator),
	// write deployment key and node slot identities (written by init script)
	role.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("SecretsManagerAccess"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"secretsmanager:GetSecretValue",
			"secretsmanager:CreateSecret",
			"secretsmanager:PutSecretValue",
			"secretsmanager:DescribeSecret",
		),
		Resources: jsii.Strings(
			fmt.Sprintf("arn:aws:secretsmanager:%s:*:secret:%s/*", region, secretsPrefix),
		),
	}))

	// SSM Parameter Store: write endpoint ID, control node ID, DTN AMI ID
	role.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("SSMParameterWrite"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"ssm:PutParameter",
			"ssm:GetParameter",
			"ssm:GetParameters",
			"ssm:DeleteParameter",
		),
		Resources: jsii.Strings(
			fmt.Sprintf("arn:aws:ssm:%s:*:parameter/elastic-gcs/%s/*", region, region),
		),
	}))

	// CloudWatch: publish custom ElasticGCS metrics (scale timing, node counts)
	role.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String("CloudWatchPutMetrics"),
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("cloudwatch:PutMetricData"),
		Resources: jsii.Strings("*"),
	}))

	// EC2 describe — needed by the init script to get instance metadata
	role.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:    jsii.String("EC2Describe"),
		Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings(
			"ec2:DescribeInstances",
			"ec2:DescribeTags",
		),
		Resources: jsii.Strings("*"),
	}))

	// -------------------------------------------------------------------------
	// UserData
	// Inject required environment variables, then run control_node_init.sh.
	// The script handles idempotency: if endpoint is already configured
	// (deployment-key.json exists), it skips setup and is safe to re-run.
	// -------------------------------------------------------------------------
	scriptBytes, err := os.ReadFile("scripts/userdata/control_node_init.sh")
	if err != nil {
		panic(fmt.Sprintf("cannot read scripts/userdata/control_node_init.sh: %v\n"+
			"(ensure cdk synth is run from the project root)", err))
	}

	bucketsJSON, err := json.Marshal(props.OpenDataBuckets)
	if err != nil {
		bucketsJSON = []byte("[]")
	}

	endpointName := props.EndpointDisplayName
	if endpointName == "" {
		endpointName = fmt.Sprintf("Elastic GCS Open Data - %s", region)
	}
	orgName := props.OrganizationName
	if orgName == "" {
		orgName = "Elastic GCS"
	}

	ud := awsec2.UserData_ForLinux(nil)
	ud.AddCommands(
		// Install SSM Agent first so we can connect via Session Manager.
		// Rocky Linux 9 does not include SSM Agent by default.
		jsii.String("dnf install -y https://s3.amazonaws.com/ec2-downloads-windows/SSMAgent/latest/linux_amd64/amazon-ssm-agent.rpm 2>/dev/null || true"),
		jsii.String("systemctl enable amazon-ssm-agent && systemctl start amazon-ssm-agent"),
		// Environment variables consumed by control_node_init.sh
		jsii.String(fmt.Sprintf("export AWS_REGION=%q", region)),
		jsii.String(fmt.Sprintf("export SECRETS_PREFIX=%q", secretsPrefix)),
		jsii.String(fmt.Sprintf("export ENDPOINT_DISPLAY_NAME=%q", endpointName)),
		jsii.String(fmt.Sprintf("export ORGANIZATION_NAME=%q", orgName)),
		jsii.String(fmt.Sprintf("export GLOBUS_OWNER_IDENTITY=%q", props.GlobusOwnerIdentity)),
		jsii.String(fmt.Sprintf("export GLOBUS_SUBSCRIPTION_ID=%q", props.GlobusSubscriptionID)),
		jsii.String(fmt.Sprintf("export WARM_POOL_SIZE=%d", props.RegionConfig.WarmPoolSize)),
		// JSON-encode the buckets array — shell-safe quoting via printf
		jsii.String(fmt.Sprintf("export OPEN_DATA_BUCKETS_JSON=%q", string(bucketsJSON))),
		// Run the init script (shebang on line 1 is a no-op comment here)
		jsii.String(string(scriptBytes)),
	)

	// -------------------------------------------------------------------------
	// EC2 Instance for control node.
	// GCS 5.4 requires x86-64 and 8GB RAM minimum. The instance type comes from
	// regions.yaml (control_instance field); default is t3a.large (2 vCPU, 8GB).
	// AMI: Rocky Linux 9 x86-64 (RHEL 9 compatible, supported by GCS 5.4).
	// Rocky Linux publishes official AMIs via AWS SSM parameters.
	// -------------------------------------------------------------------------
	controlInstanceStr := props.RegionConfig.ControlInstance
	if controlInstanceStr == "" {
		controlInstanceStr = "t3a.large"
	}
	// Rocky Linux 9 public AMIs from RESF (Rocky Enterprise Software Foundation).
	// Owner 792107900819 publishes official Rocky Linux AMIs without Marketplace subscription.
	ami := awsec2.MachineImage_Lookup(&awsec2.LookupMachineImageProps{
		Name:   jsii.String("Rocky-9-EC2-Base-*.x86_64"),
		Owners: jsii.Strings("792107900819"),
	})

	instanceProps := &awsec2.InstanceProps{
		Vpc:          vpc,
		InstanceType: awsec2.NewInstanceType(jsii.String(controlInstanceStr)),
		MachineImage: ami,
		SecurityGroup: sg,
		Role:         role,
		UserData:     ud,
		VpcSubnets: &awsec2.SubnetSelection{
			SubnetType: awsec2.SubnetType_PUBLIC,
		},
		AssociatePublicIpAddress: jsii.Bool(true),
		RequireImdsv2:            jsii.Bool(true), // enforce IMDSv2 for security
		// 30GB gp3 root volume — enough for GCS installation + logs
		BlockDevices: &[]*awsec2.BlockDevice{
			{
				DeviceName: jsii.String("/dev/sda1"),
				Volume: awsec2.BlockDeviceVolume_Ebs(jsii.Number(30), &awsec2.EbsDeviceOptions{
					VolumeType:          awsec2.EbsDeviceVolumeType_GP3,
					Encrypted:           jsii.Bool(true),
					DeleteOnTermination: jsii.Bool(true),
				}),
			},
		},
	}

	// Key pair is optional — Session Manager access works without one
	if props.RegionConfig.KeyPairName != "" {
		instanceProps.KeyPair = awsec2.KeyPair_FromKeyPairName(
			stack, jsii.String("KeyPair"),
			jsii.String(props.RegionConfig.KeyPairName),
		)
	}

	instance := awsec2.NewInstance(stack, jsii.String("ControlNode"), instanceProps)
	s.ControlInstance = instance

	// -------------------------------------------------------------------------
	// Stack Outputs
	// VpcId and SecurityGroupId are consumed by the DTN Pool stack.
	// InstanceId and PublicIP are for operational reference.
	// -------------------------------------------------------------------------
	awscdk.NewCfnOutput(stack, jsii.String("VpcId"), &awscdk.CfnOutputProps{
		Value:       vpc.VpcId(),
		Description: jsii.String("VPC ID for Elastic GCS"),
		ExportName:  jsii.String(fmt.Sprintf("%s-VpcId", id)),
	})

	awscdk.NewCfnOutput(stack, jsii.String("SecurityGroupId"), &awscdk.CfnOutputProps{
		Value:       sg.SecurityGroupId(),
		Description: jsii.String("GCS security group ID (shared by control node and DTNs)"),
		ExportName:  jsii.String(fmt.Sprintf("%s-SgId", id)),
	})

	awscdk.NewCfnOutput(stack, jsii.String("ControlNodeInstanceId"), &awscdk.CfnOutputProps{
		Value:       instance.InstanceId(),
		Description: jsii.String("Control node EC2 instance ID"),
		ExportName:  jsii.String(fmt.Sprintf("%s-ControlInstanceId", id)),
	})

	awscdk.NewCfnOutput(stack, jsii.String("ControlNodePublicIp"), &awscdk.CfnOutputProps{
		Value:       instance.InstancePublicIp(),
		Description: jsii.String("Control node public IP — SSH and Globus Manager endpoint"),
	})

	awscdk.NewCfnOutput(stack, jsii.String("SecretsPrefix"), &awscdk.CfnOutputProps{
		Value:       jsii.String(secretsPrefix),
		Description: jsii.String("Secrets Manager prefix for this deployment"),
	})

	awscdk.NewCfnOutput(stack, jsii.String("SetupLogLocation"), &awscdk.CfnOutputProps{
		Value:       jsii.String("/var/log/elastic-gcs-setup.log"),
		Description: jsii.String("SSH to control node and tail this log to monitor setup progress"),
	})

	return s
}
