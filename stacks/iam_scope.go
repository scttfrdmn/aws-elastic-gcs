package stacks

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// iamScope builds policy statements confined to one deployment's resources.
//
// Every instance a deployment owns (control node and DTNs) carries the
// elastic-gcs:deployment tag (stack-level tag, propagated at launch by the
// ASG), so instance-level permissions are conditioned on it. ASG mutations are
// scoped to the deployment's ASG ARN. Describe* and GetCommandInvocation don't
// support resource-level permissions and stay on "*" (#35).
type iamScope struct {
	sid     string // statement ID prefix
	region  string
	account string
	asgName string
	depID   string // value of the elastic-gcs:deployment tag
}

func (s iamScope) instanceARN() string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:instance/*", s.region, s.account)
}

func (s iamScope) asgARN() string {
	return fmt.Sprintf("arn:aws:autoscaling:%s:%s:autoScalingGroup:*:autoScalingGroupName/%s", s.region, s.account, s.asgName)
}

// runShell allows ssm:SendCommand with AWS-RunShellScript, only on instances
// of this deployment.
func (s iamScope) runShell() []awsiam.PolicyStatement {
	return []awsiam.PolicyStatement{
		awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid:       jsii.String(s.sid + "RunShellDocument"),
			Effect:    awsiam.Effect_ALLOW,
			Actions:   jsii.Strings("ssm:SendCommand"),
			Resources: jsii.Strings(fmt.Sprintf("arn:aws:ssm:%s::document/AWS-RunShellScript", s.region)),
		}),
		awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid:       jsii.String(s.sid + "RunShellOnDeployment"),
			Effect:    awsiam.Effect_ALLOW,
			Actions:   jsii.Strings("ssm:SendCommand"),
			Resources: jsii.Strings(s.instanceARN()),
			Conditions: &map[string]interface{}{
				"StringEquals": map[string]interface{}{
					"ssm:resourceTag/" + names.TagDeployment: s.depID,
				},
			},
		}),
		awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid:       jsii.String(s.sid + "RunShellResults"),
			Effect:    awsiam.Effect_ALLOW,
			Actions:   jsii.Strings("ssm:GetCommandInvocation"),
			Resources: jsii.Strings("*"),
		}),
	}
}

// runShellOnRole is runShell restricted further to instances whose TagRole is
// role, so a Lambda that targets "the control node" can't be pointed at any
// other instance through the SSM parameter it reads (#44).
func (s iamScope) runShellOnRole(role string) []awsiam.PolicyStatement {
	return []awsiam.PolicyStatement{
		awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid:       jsii.String(s.sid + "RunShellDocument"),
			Effect:    awsiam.Effect_ALLOW,
			Actions:   jsii.Strings("ssm:SendCommand"),
			Resources: jsii.Strings(fmt.Sprintf("arn:aws:ssm:%s::document/AWS-RunShellScript", s.region)),
		}),
		awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid:       jsii.String(s.sid + "RunShellOnRole"),
			Effect:    awsiam.Effect_ALLOW,
			Actions:   jsii.Strings("ssm:SendCommand"),
			Resources: jsii.Strings(s.instanceARN()),
			Conditions: &map[string]interface{}{
				"StringEquals": map[string]interface{}{
					"ssm:resourceTag/" + names.TagDeployment: s.depID,
					"ssm:resourceTag/" + names.TagRole:       role,
				},
			},
		}),
		awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid:       jsii.String(s.sid + "RunShellResults"),
			Effect:    awsiam.Effect_ALLOW,
			Actions:   jsii.Strings("ssm:GetCommandInvocation"),
			Resources: jsii.Strings("*"),
		}),
	}
}

// onRoleInstances is onInstances restricted to instances whose TagRole is role.
func (s iamScope) onRoleInstances(name, role string, actions ...string) awsiam.PolicyStatement {
	return awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String(s.sid + name),
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings(actions...),
		Resources: jsii.Strings(s.instanceARN()),
		Conditions: &map[string]interface{}{
			"StringEquals": map[string]interface{}{
				"aws:ResourceTag/" + names.TagDeployment: s.depID,
				"aws:ResourceTag/" + names.TagRole:       role,
			},
		},
	})
}

// onInstances allows EC2 instance actions (Start/Stop/Terminate/CreateTags)
// only on instances of this deployment.
func (s iamScope) onInstances(name string, actions ...string) awsiam.PolicyStatement {
	return awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String(s.sid + name),
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings(actions...),
		Resources: jsii.Strings(s.instanceARN()),
		Conditions: &map[string]interface{}{
			"StringEquals": map[string]interface{}{
				"aws:ResourceTag/" + names.TagDeployment: s.depID,
			},
		},
	})
}

// onASG allows ASG mutations (SetDesiredCapacity, CompleteLifecycleAction)
// only on this deployment's ASG.
func (s iamScope) onASG(actions ...string) awsiam.PolicyStatement {
	return awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String(s.sid + "OwnASG"),
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings(actions...),
		Resources: jsii.Strings(s.asgARN()),
	})
}

// unscoped is for actions without resource-level permissions (Describe*,
// CloudWatch metrics) or still to be scoped (SSM parameters, #44).
func (s iamScope) unscoped(actions ...string) awsiam.PolicyStatement {
	return awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String(s.sid + "Unscoped"),
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings(actions...),
		Resources: jsii.Strings("*"),
	})
}

// grant adds the statements to a Lambda function's role.
func grant(fn interface {
	AddToRolePolicy(awsiam.PolicyStatement)
}, stmts ...awsiam.PolicyStatement) {
	for _, st := range stmts {
		fn.AddToRolePolicy(st)
	}
}

// ssmCoreStatements is AmazonSSMManagedInstanceCore without ssm:GetParameter(s)
// on "*" (#45): what Run Command and Session Manager need. Roles that read
// parameters get their own statements scoped to the deployment's prefix.
func ssmCoreStatements() []awsiam.PolicyStatement {
	return []awsiam.PolicyStatement{
		awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid:    jsii.String("SSMCoreAgent"),
			Effect: awsiam.Effect_ALLOW,
			Actions: jsii.Strings(
				"ssm:DescribeAssociation", "ssm:GetDeployablePatchSnapshotForInstance",
				"ssm:GetDocument", "ssm:DescribeDocument", "ssm:GetManifest",
				"ssm:ListAssociations", "ssm:ListInstanceAssociations",
				"ssm:PutInventory", "ssm:PutComplianceItems", "ssm:PutConfigurePackageResult",
				"ssm:UpdateAssociationStatus", "ssm:UpdateInstanceAssociationStatus",
				"ssm:UpdateInstanceInformation",
			),
			Resources: jsii.Strings("*"),
		}),
		awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
			Sid:    jsii.String("SSMCoreMessages"),
			Effect: awsiam.Effect_ALLOW,
			Actions: jsii.Strings(
				"ssmmessages:CreateControlChannel", "ssmmessages:CreateDataChannel",
				"ssmmessages:OpenControlChannel", "ssmmessages:OpenDataChannel",
				"ec2messages:AcknowledgeMessage", "ec2messages:DeleteMessage", "ec2messages:FailMessage",
				"ec2messages:GetEndpoint", "ec2messages:GetMessages", "ec2messages:SendReply",
			),
			Resources: jsii.Strings("*"),
		}),
	}
}

// addSSMCore attaches ssmCoreStatements to a role.
func addSSMCore(role awsiam.Role) {
	for _, st := range ssmCoreStatements() {
		role.AddToPolicy(st)
	}
}
