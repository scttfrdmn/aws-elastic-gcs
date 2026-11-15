package stacks

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awss3assets"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/config"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// ControlNodeStackProps defines configuration for the control node stack.
// Globus-specific settings come from CDK context (cdk.json or -c flags).
type ControlNodeStackProps struct {
	awscdk.StackProps
	Deployment names.Deployment
	// Config is the resolved, validated deployment config (config.Resolve).
	Config          config.Resolved
	OpenDataBuckets []config.OpenDataBucket

	// Globus endpoint configuration
	GlobusSubscriptionID string // Globus subscription ID (required for managed endpoint)
	GlobusOwnerIdentity  string // Globus identity of endpoint owner (e.g. user@globus.org)
	EndpointDisplayName  string // Human-readable endpoint name
	OrganizationName     string // Organization shown in Globus endpoint listing
}

// ControlNodeStack is the CDK stack for the GCS control node.
// It exports Vpc, SecurityGroup, and ControlInstance for the DTN pool stack.
type ControlNodeStack struct {
	awscdk.Stack
	Vpc             awsec2.IVpc
	SecurityGroup   awsec2.ISecurityGroup
	ControlInstance awsec2.IInstance
	SecretsPrefix   string
	Deployment      names.Deployment
}

// NewControlNodeStack creates the Phase 1 control node infrastructure:
//   - VPC with public subnets in 2 AZs (no NAT gateways)
//   - Security group for GCS (TCP 443, 50000-51000, SSH)
//   - IAM role with least-privilege S3/Secrets Manager/SSM/CloudWatch access
//   - EC2 t3a.large (x86-64, 8GB RAM — GCS minimum) with Rocky Linux 9
//   - UserData that runs control_node_init.sh on first boot
func NewControlNodeStack(scope constructs.Construct, id string, props *ControlNodeStackProps) *ControlNodeStack {
	var sprops awscdk.StackProps
	if props != nil {
		sprops = props.StackProps
	}
	stack := awscdk.NewStack(scope, &id, &sprops)
	s := &ControlNodeStack{Stack: stack}

	dep := props.Deployment
	region := dep.Region
	secretsPrefix := dep.SecretsPrefix()
	s.SecretsPrefix = secretsPrefix
	s.Deployment = dep

	// Tag everything in the stack with its deployment, for cost tracking and
	// so several deployments in one region stay distinguishable.
	awscdk.Tags_Of(stack).Add(jsii.String(names.TagDeployment), jsii.String(dep.ID()), nil)
	awscdk.Tags_Of(stack).Add(jsii.String(names.TagRegion), jsii.String(region), nil)

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
	// S3 Gateway Endpoint
	// Routes S3 traffic from DTNs through the VPC gateway endpoint instead of
	// over the public internet. S3 gateway endpoints are free (no hourly charge,
	// no data processing charge) and eliminate S3 data transfer costs for traffic
	// that stays within the same region. For DTNs reading Open Data buckets in
	// the same region this is a significant cost saving at high throughput.
	// -------------------------------------------------------------------------
	vpc.AddGatewayEndpoint(jsii.String("S3Endpoint"), &awsec2.GatewayVpcEndpointOptions{
		Service: awsec2.GatewayVpcEndpointAwsService_S3(),
	})

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

	// SSH only when an admin CIDR is configured (#35). Access is normally via
	// SSM Session Manager; there is no SSH ingress by default.
	if props.Config.AdminCIDR != "" {
		sg.AddIngressRule(
			awsec2.Peer_Ipv4(jsii.String(props.Config.AdminCIDR)),
			awsec2.Port_Tcp(jsii.Number(22)),
			jsii.String("SSH admin access"),
			jsii.Bool(false),
		)
	}

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
		RoleName:    jsii.String(dep.Resource("control")),
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
			fmt.Sprintf("arn:aws:ssm:%s:*:parameter%s/*", region, dep.ParamPrefix()),
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
	// Inject required environment variables, download control_node_init.sh from
	// the CDK asset bucket, and run it. The script is an S3 asset rather than
	// inline UserData because it is close to the 16 KB EC2 UserData limit on
	// its own. The script handles idempotency: if endpoint is already
	// configured (deployment-key.json exists), it skips setup.
	// -------------------------------------------------------------------------
	initScript := awss3assets.NewAsset(stack, jsii.String("ControlNodeInitScript"),
		&awss3assets.AssetProps{Path: jsii.String("scripts/userdata/control_node_init.sh")})
	initScript.GrantRead(role)
	// Boot/shutdown hook + its systemd unit (#30, #31): cloud-init runs UserData
	// once per instance, so re-enabling the node after a stop/start is the unit's job.
	bootScript := awss3assets.NewAsset(stack, jsii.String("ControlNodeBootScript"),
		&awss3assets.AssetProps{Path: jsii.String("scripts/userdata/control_node_boot.sh")})
	bootScript.GrantRead(role)
	bootUnit := awss3assets.NewAsset(stack, jsii.String("ControlNodeBootUnit"),
		&awss3assets.AssetProps{Path: jsii.String("scripts/userdata/elastic-gcs-control.service")})
	bootUnit.GrantRead(role)

	bucketsJSON, err := json.Marshal(props.OpenDataBuckets)
	if err != nil {
		bucketsJSON = []byte("[]")
	}

	// The deployment ID is part of the display name so endpoints from several
	// deployments are distinguishable in the Globus web app.
	endpointName := props.EndpointDisplayName
	if endpointName == "" {
		endpointName = "Elastic GCS Open Data"
	}
	endpointName = fmt.Sprintf("%s (%s)", endpointName, dep.ID())
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
		jsii.String(fmt.Sprintf("export PARAM_PREFIX=%q", dep.ParamPrefix())),
		jsii.String(fmt.Sprintf("export ENDPOINT_DISPLAY_NAME=%q", endpointName)),
		jsii.String(fmt.Sprintf("export ORGANIZATION_NAME=%q", orgName)),
		jsii.String(fmt.Sprintf("export GLOBUS_OWNER_IDENTITY=%q", props.GlobusOwnerIdentity)),
		jsii.String(fmt.Sprintf("export GLOBUS_SUBSCRIPTION_ID=%q", props.GlobusSubscriptionID)),
		jsii.String(fmt.Sprintf("export GCS_VERSION=%q", props.Config.GCSVersion)),
		jsii.String(fmt.Sprintf("export WARM_POOL_SIZE=%d", props.Config.WarmPoolSize)),
		// TOTAL_SLOTS = maxDTNs + warmPoolSize: every EIP slot needs a node identity.
		jsii.String(fmt.Sprintf("export TOTAL_SLOTS=%d", props.Config.TotalSlots)),
		// JSON-encode the buckets array — shell-safe quoting via printf
		jsii.String(fmt.Sprintf("export OPEN_DATA_BUCKETS_JSON=%q", string(bucketsJSON))),
		// The Rocky base AMI has no AWS CLI; install v2 so the init script can
		// be downloaded from S3.
		jsii.String(strings.TrimSpace(`
if ! command -v aws >/dev/null; then
    dnf install -y unzip
    curl -sSf https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o /tmp/awscliv2.zip
    (cd /tmp && unzip -q awscliv2.zip && ./aws/install)
    ln -sf /usr/local/bin/aws /usr/bin/aws
    rm -rf /tmp/aws /tmp/awscliv2.zip
fi
`)),
	)
	initPath := ud.AddS3DownloadCommand(&awsec2.S3DownloadOptions{
		Bucket:    initScript.Bucket(),
		BucketKey: initScript.S3ObjectKey(),
		LocalFile: jsii.String("/opt/elastic-gcs/scripts/control_node_init.sh"),
		Region:    jsii.String(region),
	})
	bootPath := ud.AddS3DownloadCommand(&awsec2.S3DownloadOptions{
		Bucket:    bootScript.Bucket(),
		BucketKey: bootScript.S3ObjectKey(),
		LocalFile: jsii.String("/opt/elastic-gcs/scripts/control_node_boot.sh"),
		Region:    jsii.String(region),
	})
	unitPath := ud.AddS3DownloadCommand(&awsec2.S3DownloadOptions{
		Bucket:    bootUnit.Bucket(),
		BucketKey: bootUnit.S3ObjectKey(),
		LocalFile: jsii.String("/etc/systemd/system/elastic-gcs-control.service"),
		Region:    jsii.String(region),
	})
	ud.AddCommands(
		jsii.String("chmod 0755 "+*bootPath),
		jsii.String("chmod 0644 "+*unitPath),
		jsii.String("systemctl daemon-reload"),
		jsii.String("systemctl enable elastic-gcs-control"),
		jsii.String("bash "+*initPath),
		// Start the unit once setup is done, so its ExecStop (node disable) runs
		// on the first shutdown too. Its start action is an idempotent node enable.
		jsii.String("systemctl start elastic-gcs-control"),
	)

	// -------------------------------------------------------------------------
	// EC2 Instance for control node.
	// GCS 5.4 only publishes RPM packages for x86-64 (EL8/EL9 aarch64 returns 404).
	// Default: t3a.large (AMD EPYC, 2 vCPU, 8GB) — cheapest x86-64 meeting 8GB minimum.
	// Note: t4g (Graviton) is NOT usable until Globus publishes aarch64 EL9 packages.
	// Instance type comes from regions.yaml (control_instance field).
	// -------------------------------------------------------------------------
	controlInstanceStr := props.Config.ControlInstance
	// Rocky Linux 9 public AMIs from RESF (Rocky Enterprise Software Foundation).
	// Owner 792107900819 publishes official Rocky Linux AMIs without Marketplace subscription.
	ami := awsec2.MachineImage_Lookup(&awsec2.LookupMachineImageProps{
		Name:   jsii.String("Rocky-9-EC2-Base-*.x86_64"),
		Owners: jsii.Strings("792107900819"),
	})

	instanceProps := &awsec2.InstanceProps{
		InstanceName:  jsii.String(dep.Resource("control")),
		Vpc:           vpc,
		InstanceType:  awsec2.NewInstanceType(jsii.String(controlInstanceStr)),
		MachineImage:  ami,
		SecurityGroup: sg,
		Role:          role,
		UserData:      ud,
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
	if props.Config.KeyPairName != "" {
		instanceProps.KeyPair = awsec2.KeyPair_FromKeyPairName(
			stack, jsii.String("KeyPair"),
			jsii.String(props.Config.KeyPairName),
		)
	}

	instance := awsec2.NewInstance(stack, jsii.String("ControlNode"), instanceProps)
	awscdk.Tags_Of(instance).Add(jsii.String(names.TagRole), jsii.String("control"), nil)
	s.ControlInstance = instance

	// -------------------------------------------------------------------------
	// Elastic IP for the control node.
	// Without an EIP, every stop/start (including CDK deploys that change
	// UserData) assigns a new ephemeral public IP. GCS nodes are registered
	// by IP in Globus DNS, so an IP change requires a manual `node update`.
	// An EIP costs nothing when attached to a running instance (~$3.60/mo if
	// stopped), and the control node should always be running.
	// -------------------------------------------------------------------------
	eip := awsec2.NewCfnEIP(stack, jsii.String("ControlNodeEIP"), &awsec2.CfnEIPProps{
		Tags: &[]*awscdk.CfnTag{
			{Key: jsii.String("Name"), Value: jsii.String(dep.Resource("control"))},
			{Key: jsii.String(names.TagRole), Value: jsii.String("control")},
		},
	})

	eipAssoc := awsec2.NewCfnEIPAssociation(stack, jsii.String("ControlNodeEIPAssoc"), &awsec2.CfnEIPAssociationProps{
		AllocationId: eip.AttrAllocationId(),
		InstanceId:   instance.InstanceId(),
	})
	// Explicit dependency ensures the instance exists before association.
	eipAssoc.AddDependency(instance.Node().DefaultChild().(awscdk.CfnResource))

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
		Value:       eip.AttrPublicIp(),
		Description: jsii.String("Control node Elastic IP — stable across stop/start and CDK deploys"),
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
