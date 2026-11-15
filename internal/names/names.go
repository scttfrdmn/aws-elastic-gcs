// Package names derives every resource name for an Elastic GCS deployment.
//
// A deployment is identified by (region, name), so several independent
// endpoints can run in one region and account. All AWS names, Secrets
// Manager and SSM paths, and CloudWatch dimensions come from here; the CDK
// stacks pass the resolved values to Lambdas and shell scripts through
// environment variables.
package names

import (
	"fmt"
	"os"
	"regexp"
)

// MetricNamespace is the CloudWatch namespace for all custom metrics. Metrics
// carry both Region and Deployment dimensions (see Dimensions).
const MetricNamespace = "ElasticGCS"

// DTNMetricNamespace holds metrics DTNs publish about themselves (boot
// latency). It is separate so the DTN role can be limited to it: DTNs must not
// be able to forge the demand metrics that drive scaling (#43).
const DTNMetricNamespace = "ElasticGCS/DTN"

// DefaultDeployment is used when regions.yaml doesn't name a deployment.
const DefaultDeployment = "default"

var (
	nameRe   = regexp.MustCompile(`^[a-z][a-z0-9]{0,11}$`)
	regionRe = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
)

// Deployment identifies one Elastic GCS endpoint.
type Deployment struct {
	Region string
	Name   string
}

// New validates region and name. An empty name means DefaultDeployment.
// Names are 1-12 lowercase alphanumerics starting with a letter, which keeps
// every derived name within AWS limits (64 chars for Lambda and IAM role
// names) and safe to interpolate into shell.
func New(region, name string) (Deployment, error) {
	if name == "" {
		name = DefaultDeployment
	}
	if !regionRe.MatchString(region) {
		return Deployment{}, fmt.Errorf("invalid region %q", region)
	}
	if !nameRe.MatchString(name) {
		return Deployment{}, fmt.Errorf("invalid deployment name %q: want 1-12 lowercase letters/digits, starting with a letter", name)
	}
	return Deployment{Region: region, Name: name}, nil
}

// ID is the region-qualified deployment ID, e.g. "us-west-2-src".
func (d Deployment) ID() string { return d.Region + "-" + d.Name }

// Resource names an AWS resource of the given kind, e.g. Resource("dtn") is
// "elastic-gcs-dtn-us-west-2-src" (ASG, launch template, DTN IAM role).
func (d Deployment) Resource(kind string) string {
	return fmt.Sprintf("elastic-gcs-%s-%s", kind, d.ID())
}

// Stack names a CloudFormation stack, e.g. Stack("Control") is
// "ElasticGCS-Control-us-west-2-src".
func (d Deployment) Stack(kind string) string {
	return fmt.Sprintf("ElasticGCS-%s-%s", kind, d.ID())
}

// SecretsPrefix is the Secrets Manager name prefix, "elastic-gcs/<region>/<name>".
func (d Deployment) SecretsPrefix() string {
	return fmt.Sprintf("elastic-gcs/%s/%s", d.Region, d.Name)
}

// ParamPrefix is the SSM Parameter Store prefix, "/elastic-gcs/<region>/<name>".
func (d Deployment) ParamPrefix() string {
	return fmt.Sprintf("/elastic-gcs/%s/%s", d.Region, d.Name)
}

// Param is a parameter under ParamPrefix, e.g. Param("endpoint-id").
func (d Deployment) Param(leaf string) string { return d.ParamPrefix() + "/" + leaf }

// AMIParam is the region-wide SSM parameter holding the DTN AMI ID for a GCS
// version, e.g. "/elastic-gcs/us-west-2/dtn-ami/gcs-5.4.101". It is written by
// scripts/build_ami.sh and shared by every deployment in the region, because
// the AMI depends only on the GCS version.
func (d Deployment) AMIParam(gcsVersion string) string {
	return fmt.Sprintf("/elastic-gcs/%s/dtn-ami/gcs-%s", d.Region, gcsVersion)
}

// Dashboard names the CloudWatch dashboard.
func (d Deployment) Dashboard() string { return "ElasticGCS-" + d.ID() }

// Dimensions returns the CloudWatch dimensions attached to every custom metric.
func (d Deployment) Dimensions() map[string]string {
	return map[string]string{"Region": d.Region, "Deployment": d.Name}
}

// Well-known parameter leaves and tag keys shared by stacks, Lambdas and scripts.
const (
	ParamEndpointID    = "endpoint-id"
	ParamControlNodeID = "control-node-id"
	ParamSlotLock      = "slot-lock"
	ParamSetupLock     = "setup-lock"

	TagDeployment = "elastic-gcs:deployment"
	TagRegion     = "elastic-gcs:region"
	TagRole       = "elastic-gcs:role"

	// Written by dtn_boot.sh (its IAM allows exactly these two keys).
	TagSlot      = "elastic-gcs:slot"
	TagGCSNodeID = "elastic-gcs:gcs-node-id"
	// Written by the lifecycle Lambda when an instance enters the warm pool.
	TagWarmPoolEntryTime = "elastic-gcs:warm-pool-entry-time"

	// Instance roles, in the TagRole tag. Lambdas that act on the control node
	// (or on DTNs) require the matching role tag (#44).
	RoleControl = "control"
	RoleDTN     = "dtn"

	// ASG lifecycle hooks (dtn_boot.sh gets HookReady via LIFECYCLE_HOOK).
	HookReady   = "dtn-ready-hook"
	HookDisable = "dtn-disable-hook"
)

// FromEnv builds the Deployment from AWS_REGION (set by the Lambda runtime)
// and DEPLOYMENT (set by the DTN pool stack).
func FromEnv() (Deployment, error) {
	return New(os.Getenv("AWS_REGION"), os.Getenv("DEPLOYMENT"))
}
