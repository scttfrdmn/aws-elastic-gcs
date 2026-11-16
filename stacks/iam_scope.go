package stacks

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// iamScope builds policy statements confined to one deployment's resources.
//
// Every instance a deployment owns (control node and DTN slots) carries the
// elastic-gcs:deployment tag (the control stack's tag, and the slot
// controller's launch tags), so instance-level permissions are conditioned on
// it. GetCommandInvocation doesn't support resource-level permissions and
// stays on "*" (#35).
type iamScope struct {
	sid     string // statement ID prefix
	region  string
	account string
	depID   string // value of the elastic-gcs:deployment tag
}

func (s iamScope) instanceARN() string {
	return fmt.Sprintf("arn:aws:ec2:%s:%s:instance/*", s.region, s.account)
}

// runShellOnRole allows ssm:SendCommand with AWS-RunShellScript, only on this
// deployment's instances whose TagRole is role, so a Lambda that targets "the
// control node" can't be pointed at any other instance through the SSM
// parameter it reads (#44).
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

// onRoleInstances allows EC2 instance actions only on this deployment's
// instances whose TagRole is role.
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

// readParams allows ssm:GetParameter on this deployment's parameters only.
func (s iamScope) readParams(prefix string) awsiam.PolicyStatement {
	return awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Sid:       jsii.String(s.sid + "ReadParams"),
		Effect:    awsiam.Effect_ALLOW,
		Actions:   jsii.Strings("ssm:GetParameter"),
		Resources: jsii.Strings(fmt.Sprintf("arn:aws:ssm:%s:%s:parameter%s/*", s.region, s.account, prefix)),
	})
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
