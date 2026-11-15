package stacks

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// dtnRole creates the IAM role for DTN instances.
// DTNs run the GCS S3 connector — they need the same S3 read access as
// the control node, plus EC2 EIP association and lifecycle hook signaling.
func (p *dtnPool) dtnRole() awsiam.Role {
	dep, region := p.dep, p.dep.Region
	dtnRole := awsiam.NewRole(p.stack, jsii.String("DTNRole"), &awsiam.RoleProps{
		AssumedBy:   awsiam.NewServicePrincipal(jsii.String("ec2.amazonaws.com"), nil),
		Description: jsii.String("IAM role for Elastic GCS DTN instances"),
		RoleName:    jsii.String(dep.Resource("dtn")),
	})

	// SSM agent permissions without GetParameter on "*" (#45).
	addSSMCore(dtnRole)
	dtnRole.AddManagedPolicy(awsiam.ManagedPolicy_FromAwsManagedPolicyName(
		jsii.String("CloudWatchAgentServerPolicy")))

	// S3 read for Open Data buckets — DTNs do the actual data movement
	if len(p.props.OpenDataBuckets) > 0 {
		bucketArns := make([]*string, 0, len(p.props.OpenDataBuckets)*2)
		for _, b := range p.props.OpenDataBuckets {
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
			fmt.Sprintf("arn:aws:secretsmanager:%s:*:secret:%s/*", region, p.secretsPrefix),
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
		// Instances of this ASG, two tag keys. IAM doesn't accept a policy
		// variable such as ${ec2:SourceInstanceARN} as a resource, so a DTN could
		// still retag another DTN of this pool; the worst case is a misdirected
		// orphan-node delete, which the slot re-imports on its next boot (#43).
		Resources: jsii.Strings(fmt.Sprintf("arn:aws:ec2:%s:%s:instance/*", p.dep.Region, *p.stack.Account())),
		Conditions: &map[string]interface{}{
			"StringEquals": map[string]interface{}{
				"ec2:ResourceTag/aws:autoscaling:groupName": p.asgName,
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
	dtnRole.AddToPolicy(p.scoped("LifecycleHook").onASG("autoscaling:CompleteLifecycleAction"))
	dtnRole.AddToPolicy(p.scoped("LifecycleHook").unscoped("autoscaling:DescribeAutoScalingInstances"))

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

	return dtnRole
}
