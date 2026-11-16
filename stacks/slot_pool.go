package stacks

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsevents"
	"github.com/aws/aws-cdk-go/awscdk/v2/awseventstargets"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awss3assets"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// buildSlots builds the pool: slots variant (#65, #128): no ASG, warm pool or
// lifecycle hooks. Instances and Elastic IPs belong to the slot controller, not
// to CloudFormation, so a deploy can never resize or replace them (#71).
func (p *dtnPool) buildSlots() {
	runtimeRole, setupRole := p.slotRoles()
	runtimeProfile := awsiam.NewInstanceProfile(p.stack, jsii.String("SlotRuntimeProfile"), &awsiam.InstanceProfileProps{
		Role: runtimeRole, InstanceProfileName: jsii.String(p.dep.Resource("dtn-runtime")),
	})
	setupProfile := awsiam.NewInstanceProfile(p.stack, jsii.String("SlotSetupProfile"), &awsiam.InstanceProfileProps{
		Role: setupRole, InstanceProfileName: jsii.String(p.dep.Resource("dtn-setup")),
	})
	lt := p.dtnLaunchTemplate(runtimeProfile, p.slotUserData(runtimeRole))

	ssmLogs := awslogs.NewLogGroup(p.stack, jsii.String("SSMCommandLogs"), &awslogs.LogGroupProps{
		LogGroupName:  jsii.String("/elastic-gcs/" + p.dep.ID() + "/ssm-commands"),
		Retention:     awslogs.RetentionDays_THREE_MONTHS,
		RemovalPolicy: awscdk.RemovalPolicy_RETAIN,
	})
	ctl := p.slotControllerFn(lt, runtimeRole, setupRole, runtimeProfile, setupProfile, ssmLogs)
	demand := p.demandMonitorFn(ssmLogs)

	// Converge every minute, and as soon as demand_monitor changes desired-dtns.
	// Durable functions can only be invoked by a qualified ARN. Target a "live"
	// alias on the published version: CDK grants invoke on it (it doesn't for
	// $LATEST), and in-flight executions keep the code version they started on.
	live := awslambda.NewAlias(p.stack, jsii.String("SlotControllerLive"), &awslambda.AliasProps{
		AliasName: jsii.String("live"), Version: ctl.CurrentVersion(),
	})
	awsevents.NewRule(p.stack, jsii.String("SlotControllerSchedule"), &awsevents.RuleProps{
		RuleName: jsii.String(p.dep.Resource("slot-controller")),
		Schedule: awsevents.Schedule_Rate(minutes(1)),
		Targets:  &[]awsevents.IRuleTarget{awseventstargets.NewLambdaFunction(live, nil)},
	})
	awsevents.NewRule(p.stack, jsii.String("SlotControllerOnDesired"), &awsevents.RuleProps{
		RuleName: jsii.String(p.dep.Resource("slot-desired")),
		EventPattern: &awsevents.EventPattern{
			Source:     jsii.Strings("aws.ssm"),
			DetailType: jsii.Strings("Parameter Store Change"),
			Detail:     &map[string]interface{}{"name": []string{p.dep.Param(names.ParamDesiredDTNs)}},
		},
		Targets: &[]awsevents.IRuleTarget{awseventstargets.NewLambdaFunction(live, nil)},
	})
	awsevents.NewRule(p.stack, jsii.String("DemandMonitorSchedule"), &awsevents.RuleProps{
		RuleName: jsii.String(p.dep.Resource("demand-monitor")),
		Schedule: awsevents.Schedule_Rate(minutes(1)),
		Targets:  &[]awsevents.IRuleTarget{awseventstargets.NewLambdaFunction(demand, nil)},
	})

	awscdk.NewCfnOutput(p.stack, jsii.String("SlotControllerFunction"), &awscdk.CfnOutputProps{
		Value: live.FunctionArn(), Description: jsii.String("Slot controller (Lambda durable function); invoke this qualified ARN"),
	})
}

// dtnLaunchTemplate creates the DTN launch template.
// Instance type from config (default c8in.2xlarge; see docs/instance-sizing.md).
// Uses the same security group as the control node (GCS requires same ports).
// It names the runtime profile directly: the slot controller swaps instances
// between the named runtime and setup profiles (#127).
func (p *dtnPool) dtnLaunchTemplate(profile awsiam.IInstanceProfile, ud awsec2.UserData) awsec2.LaunchTemplate {
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
		InstanceProfile:    profile,
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
	return awsec2.NewLaunchTemplate(p.stack, jsii.String("DTNLaunchTemplate"), ltProps)
}

// slotRoles returns the DTN runtime role (no secret access, #42) and the setup
// role the controller attaches only for a slot's one-time node setup.
func (p *dtnPool) slotRoles() (runtime, setup awsiam.Role) {
	mk := func(id, kind, desc string) awsiam.Role {
		r := awsiam.NewRole(p.stack, jsii.String(id), &awsiam.RoleProps{
			AssumedBy:   awsiam.NewServicePrincipal(jsii.String("ec2.amazonaws.com"), nil),
			Description: jsii.String(desc),
			RoleName:    jsii.String(p.dep.Resource(kind)),
		})
		addSSMCore(r) // never the managed core policy: it reads every parameter (#45, #127)
		r.AddManagedPolicy(awsiam.ManagedPolicy_FromAwsManagedPolicyName(jsii.String("CloudWatchAgentServerPolicy")))
		p.openDataRead(r)
		r.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid: jsii.String("DTNMetrics"), Effect: awsiam.Effect_ALLOW,
			Actions:    jsii.Strings("cloudwatch:PutMetricData"),
			Resources:  jsii.Strings("*"),
			Conditions: &map[string]interface{}{"StringEquals": map[string]interface{}{"cloudwatch:namespace": names.DTNMetricNamespace}},
		}))
		return r
	}
	runtime = mk("SlotRuntimeRole", "dtn-runtime", "Elastic GCS slot DTN: runtime (no secrets)")
	setup = mk("SlotSetupRole", "dtn-setup", "Elastic GCS slot DTN: one-time node setup")
	setup.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid: jsii.String("SetupSecrets"), Effect: awsiam.Effect_ALLOW,
		Actions: jsii.Strings("secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"),
		Resources: jsii.Strings(
			fmt.Sprintf("arn:aws:secretsmanager:%s:%s:secret:%s/deployment-key-*", p.dep.Region, *p.stack.Account(), p.secretsPrefix),
			fmt.Sprintf("arn:aws:secretsmanager:%s:%s:secret:%s/node-slot-*", p.dep.Region, *p.stack.Account(), p.secretsPrefix),
		),
	}))
	return runtime, setup
}

func (p *dtnPool) openDataRead(r awsiam.Role) {
	if len(p.props.OpenDataBuckets) == 0 {
		return
	}
	var arns []*string
	for _, b := range p.props.OpenDataBuckets {
		arns = append(arns, jsii.String("arn:aws:s3:::"+b.Bucket), jsii.String("arn:aws:s3:::"+b.Bucket+"/*"))
	}
	r.AddToPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid: jsii.String("OpenDataS3Read"), Effect: awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("s3:GetObject", "s3:ListBucket", "s3:GetBucketLocation", "s3:GetBucketRequestPayment"),
		Resources: &arns,
	}))
}

// slotUserData installs the node boot/shutdown unit: enable the GCS node on
// boot, disable it on shutdown, both with endpoint-DNS waits. First-boot node
// setup is the controller's job (identity.Setup), not UserData's.
func (p *dtnPool) slotUserData(role awsiam.Role) awsec2.UserData {
	asset := awss3assets.NewAsset(p.stack, jsii.String("SlotNodeBootScript"),
		&awss3assets.AssetProps{Path: jsii.String("scripts/userdata/control_node_boot.sh")})
	asset.GrantRead(role)
	ud := awsec2.UserData_ForLinux(nil)
	ud.AddCommands(jsii.String("set -euo pipefail"), jsii.String("systemctl enable --now amazon-ssm-agent || true"))
	local := ud.AddS3DownloadCommand(&awsec2.S3DownloadOptions{
		Bucket: asset.Bucket(), BucketKey: asset.S3ObjectKey(),
		LocalFile: jsii.String("/opt/elastic-gcs/scripts/node_boot.sh"), Region: jsii.String(p.dep.Region),
	})
	unit := `[Unit]
Description=Elastic GCS slot node lifecycle (node enable on boot, disable on shutdown)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/opt/elastic-gcs/scripts/node_boot.sh start
ExecStop=/opt/elastic-gcs/scripts/node_boot.sh stop
TimeoutStartSec=300
TimeoutStopSec=240

[Install]
WantedBy=multi-user.target
`
	ud.AddCommands(
		jsii.String("chmod 0755 "+*local),
		jsii.String("cat > /etc/systemd/system/elastic-gcs-node.service <<'UNIT'\n"+unit+"UNIT"),
		jsii.String("systemctl daemon-reload"),
		jsii.String("systemctl enable --now elastic-gcs-node"),
	)
	return ud
}

func (p *dtnPool) slotControllerFn(lt awsec2.LaunchTemplate, runtime, setup awsiam.Role,
	runtimeProfile, setupProfile awsiam.InstanceProfile, ssmLogs awslogs.LogGroup) awslambda.Function {
	dep, cfg := p.dep, p.cfg
	types := ""
	for i, t := range cfg.InstanceTypes {
		if i > 0 {
			types += ","
		}
		types += t
	}
	fn := awslambda.NewFunction(p.stack, jsii.String("SlotControllerFn"), &awslambda.FunctionProps{
		FunctionName: jsii.String(dep.Resource("slot-controller")),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		Architecture: awslambda.Architecture_ARM_64(),
		Handler:      jsii.String("bootstrap"),
		Code:         awslambda.Code_FromAsset(jsii.String("lambda/slot_controller"), nil),
		Timeout:      minutes(15), // a revive (launch + node setup) runs in one invocation
		MemorySize:   jsii.Number(256),
		DurableConfig: &awslambda.DurableConfig{
			ExecutionTimeout: awscdk.Duration_Hours(jsii.Number(1)),
			RetentionPeriod:  awscdk.Duration_Days(jsii.Number(3)),
		},
		Environment: &map[string]*string{
			"DEPLOYMENT":         jsii.String(dep.Name),
			"SSM_LOG_GROUP":      ssmLogs.LogGroupName(),
			"MAX_DTNS":           itoa(cfg.MaxDTNs),
			"WARM_SLOTS":         itoa(cfg.WarmSlots),
			"DRAIN_TIMEOUT":      jsii.String(cfg.DrainTimeout.String()),
			"COLD_TTL":           jsii.String(cfg.ColdTTL.String()),
			"UNHEALTHY_AFTER":    jsii.String(cfg.UnhealthyAfter.String()),
			"MAX_RESTARTS":       itoa(cfg.MaxRestarts),
			"LAUNCH_TEMPLATE_ID": lt.LaunchTemplateId(),
			"INSTANCE_TYPES":     jsii.String(types),
			"SUBNET_IDS":         awscdk.Fn_Join(jsii.String(","), p.props.ControlStack.Vpc.SelectSubnets(&awsec2.SubnetSelection{SubnetType: awsec2.SubnetType_PUBLIC}).SubnetIds),
			"RUNTIME_PROFILE":    runtimeProfile.InstanceProfileName(),
			"SETUP_PROFILE":      setupProfile.InstanceProfileName(),
			"SETUP_ROLE":         setup.RoleName(),
		},
		LogRetention: awslogs.RetentionDays_THREE_MONTHS,
	})
	acct := *p.stack.Account()
	fnArn := fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s:*", dep.Region, acct, dep.Resource("slot-controller"))
	grantAll := func(sid string, actions []string, resources []string, cond map[string]interface{}) {
		st := &awsiam.PolicyStatementProps{Sid: jsii.String(sid), Effect: awsiam.Effect_ALLOW,
			Actions: jsii.Strings(actions...), Resources: jsii.Strings(resources...)}
		if cond != nil {
			st.Conditions = &cond
		}
		fn.AddToRolePolicy(awsiam.NewPolicyStatement(st))
	}
	slotTags := map[string]interface{}{"StringEquals": map[string]interface{}{
		"aws:ResourceTag/" + names.TagDeployment: dep.ID(), "aws:ResourceTag/elastic-gcs:pool": "slots"}}
	instances := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/*", dep.Region, acct)

	grantAll("Durable", []string{"lambda:CheckpointDurableExecution", "lambda:GetDurableExecutionState"}, []string{fnArn}, nil)
	// Launch: only with this deployment's slot tags on the new instance.
	grantAll("LaunchSlots", []string{"ec2:RunInstances"}, []string{instances}, map[string]interface{}{
		"StringEquals": map[string]interface{}{"aws:RequestTag/" + names.TagDeployment: dep.ID(), "aws:RequestTag/elastic-gcs:pool": "slots"}})
	grantAll("LaunchResources", []string{"ec2:RunInstances"}, []string{
		fmt.Sprintf("arn:aws:ec2:%s:%s:launch-template/*", dep.Region, acct),
		fmt.Sprintf("arn:aws:ec2:%s::image/*", dep.Region),
		fmt.Sprintf("arn:aws:ec2:%s:%s:subnet/*", dep.Region, acct),
		fmt.Sprintf("arn:aws:ec2:%s:%s:network-interface/*", dep.Region, acct),
		fmt.Sprintf("arn:aws:ec2:%s:%s:security-group/*", dep.Region, acct),
		fmt.Sprintf("arn:aws:ec2:%s:%s:volume/*", dep.Region, acct),
	}, nil)
	grantAll("TagOnLaunch", []string{"ec2:CreateTags"}, []string{instances, fmt.Sprintf("arn:aws:ec2:%s:%s:volume/*", dep.Region, acct)},
		map[string]interface{}{"StringEquals": map[string]interface{}{"ec2:CreateAction": "RunInstances"}})
	grantAll("ManageSlots", []string{"ec2:StartInstances", "ec2:StopInstances", "ec2:TerminateInstances", "ec2:CreateTags", "ec2:DeleteTags",
		"ec2:ReplaceIamInstanceProfileAssociation", "ec2:AssociateAddress"}, []string{instances}, slotTags)
	grantAll("SlotAddresses", []string{"ec2:AllocateAddress", "ec2:AssociateAddress", "ec2:ReleaseAddress", "ec2:CreateTags"},
		[]string{fmt.Sprintf("arn:aws:ec2:%s:%s:elastic-ip/*", dep.Region, acct)}, nil)
	grantAll("Describe", []string{"ec2:DescribeInstances", "ec2:DescribeAddresses", "ec2:DescribeIamInstanceProfileAssociations",
		"ssm:DescribeInstanceInformation", "ssm:GetCommandInvocation"}, []string{"*"}, nil)
	grantAll("PassSlotRoles", []string{"iam:PassRole"}, []string{*runtime.RoleArn(), *setup.RoleArn()}, nil)
	grantAll("RevokeSetupSessions", []string{"iam:PutRolePolicy"}, []string{*setup.RoleArn()}, nil)
	grantAll("Params", []string{"ssm:GetParameter", "ssm:PutParameter", "ssm:DeleteParameter"},
		[]string{fmt.Sprintf("arn:aws:ssm:%s:%s:parameter%s/*", dep.Region, acct, dep.ParamPrefix())}, nil)
	for _, st := range p.scoped("SlotCtlDTN").runShellOnRole(names.RoleDTN) {
		fn.AddToRolePolicy(st)
	}
	for _, st := range p.scoped("SlotCtlControl").runShellOnRole(names.RoleControl) {
		fn.AddToRolePolicy(st)
	}
	fn.AddToRolePolicy(p.scoped("SlotCtlControl").onRoleInstances("StartControl", names.RoleControl, "ec2:StartInstances"))
	return fn
}
