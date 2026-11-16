package stacks

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/config"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// DTNPoolStackProps extends the basic props with Open Data bucket access.
type DTNPoolStackProps struct {
	awscdk.StackProps
	Deployment names.Deployment
	// Config is the resolved, validated deployment config (config.Resolve).
	Config          config.Resolved
	ControlStack    *ControlNodeStack
	OpenDataBuckets []config.OpenDataBucket

	// OperatorEmail, if set, is subscribed to the operator alert topic. The
	// slots stack has no alert topic yet (#144).
	OperatorEmail string
}

// dtnPool carries what every part of the DTN pool stack needs. Each concern
// lives in its own file:
//
//	slot_pool.go     launch template, slot roles and profiles, the slot
//	                 controller Lambda and its triggers
//	dtn_lambdas.go   Go Lambda helper and the demand monitor
//	iam_scope.go     IAM statements confined to one deployment
type dtnPool struct {
	stack         awscdk.Stack
	id            string // stack ID
	props         *DTNPoolStackProps
	dep           names.Deployment
	cfg           config.Resolved
	secretsPrefix string
}

// scoped builds IAM statements confined to this deployment's instances (#35).
func (p *dtnPool) scoped(sid string) iamScope {
	return iamScope{sid: sid, region: p.dep.Region, account: *p.stack.Account(), depID: p.dep.ID()}
}

// NewDTNPoolStack creates a deployment's DTN pool: DTN slots owned by the slot
// controller (#65). CloudFormation creates the controller, its roles and the
// launch template; instances and Elastic IPs belong to the controller, so a
// deploy never resizes or replaces them.
func NewDTNPoolStack(scope constructs.Construct, id string, props *DTNPoolStackProps) awscdk.Stack {
	var sprops awscdk.StackProps
	if props != nil {
		sprops = props.StackProps
	}
	stack := awscdk.NewStack(scope, &id, &sprops)

	dep := props.Deployment
	awscdk.Tags_Of(stack).Add(jsii.String(names.TagDeployment), jsii.String(dep.ID()), nil)
	awscdk.Tags_Of(stack).Add(jsii.String(names.TagRegion), jsii.String(dep.Region), nil)

	// All sizing comes from config.Resolve, shared with the control stack (#49).
	p := &dtnPool{
		stack:         stack,
		id:            id,
		props:         props,
		dep:           dep,
		cfg:           props.Config,
		secretsPrefix: props.ControlStack.SecretsPrefix,
	}
	if p.cfg.ThroughputDerivedFallback {
		awscdk.Annotations_Of(stack).AddWarningV2(jsii.String("elastic-gcs:throughputThreshold"),
			jsii.String(fmt.Sprintf("DTN type %s has no known sustained throughput; using a "+
				"500 MB/s scale-out threshold. Set throughput_scaleout_mbps for this region "+
				"(see docs/instance-sizing.md).", p.cfg.DTNInstance)), nil)
	}

	p.buildSlots()
	return stack
}
