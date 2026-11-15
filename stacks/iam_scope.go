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
