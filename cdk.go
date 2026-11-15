package main

import (
	"fmt"
	"os"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/config"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
	"github.com/scttfrdmn/aws-elastic-gcs/stacks"
)

func main() {
	defer jsii.Close()

	app := awscdk.NewApp(nil)

	// Load region configuration
	regions, err := config.LoadRegions("config/regions.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load region config: %v\n", err)
		os.Exit(1)
	}

	// Load Open Data bucket configuration
	buckets, err := config.LoadOpenDataBuckets("config/open_data_buckets.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load Open Data bucket config: %v\n", err)
		os.Exit(1)
	}

	// Read Globus and deployment configuration from CDK context.
	// Set these in cdk.json or pass via: cdk deploy -c key=value
	globusSubscriptionID := ctxString(app, "globus:subscriptionId")
	globusOwnerIdentity := ctxString(app, "globus:endpointOwner")
	endpointDisplayName := ctxString(app, "globus:endpointDisplayName")
	orgName := ctxString(app, "globus:organizationName")
	adminCIDR := ctxString(app, "adminCIDR")
	operatorEmail := ctxString(app, "elastic-gcs:operatorEmail")
	// GCS package version pinned on every node. All nodes in an endpoint must
	// run the same GCS version, so the control node, DTN UserData, and DTN AMI
	// (scripts/build_ami.sh) all key off this one value.
	// Deployment-wide defaults from CDK context; per-deployment settings in
	// regions.yaml win. config.Resolve validates everything (#49).
	defaults := config.Defaults{GCSVersion: ctxString(app, "globus:gcsVersion"), AdminCIDR: adminCIDR}
	for key, dst := range map[string]*int{
		"elastic-gcs:maxDTNs":                &defaults.MaxDTNs,
		"elastic-gcs:cacheEvictionHours":     &defaults.CacheEvictionHours,
		"elastic-gcs:throughputScaleOutMBps": &defaults.ThroughputMBps,
	} {
		n, err := config.CtxInt(key, ctxString(app, key))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		*dst = n
	}

	// CDK_DEFAULT_ACCOUNT is injected by the CDK CLI when running cdk commands.
	// It is required for context lookups (e.g. MachineImage_Lookup for AMI IDs).
	account := os.Getenv("CDK_DEFAULT_ACCOUNT")

	// Deploy a control stack and a DTN pool stack per deployment. Several
	// deployments may share a region; (region, deployment) must be unique.
	seen := map[string]bool{}
	for _, region := range regions.Regions {
		dep, err := names.New(region.Name, region.Deployment)
		if err != nil {
			fmt.Fprintf(os.Stderr, "config/regions.yaml: %v\n", err)
			os.Exit(1)
		}
		if seen[dep.ID()] {
			fmt.Fprintf(os.Stderr, "config/regions.yaml: duplicate deployment %s\n", dep.ID())
			os.Exit(1)
		}
		seen[dep.ID()] = true

		env := &awscdk.Environment{
			Account: jsii.String(account),
			Region:  jsii.String(region.Name),
		}

		regionBuckets := buckets.BucketsFor(dep.Region, dep.Name)

		// Phase 1 + 2: Network + Control Node
		controlStackName := dep.Stack("Control")
		cfg, err := config.Resolve(region, defaults)
		if err != nil {
			fmt.Fprintf(os.Stderr, "config/regions.yaml: %v\n", err)
			os.Exit(1)
		}

		controlStack := stacks.NewControlNodeStack(app, controlStackName, &stacks.ControlNodeStackProps{
			StackProps: awscdk.StackProps{
				Env:         env,
				Description: jsii.String(fmt.Sprintf("Elastic GCS Control Node - %s", dep.ID())),
			},
			Deployment:           dep,
			Config:               cfg,
			OpenDataBuckets:      regionBuckets,
			GlobusSubscriptionID: globusSubscriptionID,
			GlobusOwnerIdentity:  globusOwnerIdentity,
			EndpointDisplayName:  endpointDisplayName,
			OrganizationName:     orgName,
		})
		dtnStackName := dep.Stack("DTNPool")
		stacks.NewDTNPoolStack(app, dtnStackName, &stacks.DTNPoolStackProps{
			StackProps: awscdk.StackProps{
				Env:         env,
				Description: jsii.String(fmt.Sprintf("Elastic GCS DTN Pool - %s", dep.ID())),
			},
			Deployment:      dep,
			Config:          cfg,
			ControlStack:    controlStack,
			OpenDataBuckets: regionBuckets,
			OperatorEmail:   operatorEmail,
		})
	}

	app.Synth(nil)
}

// ctxString reads a string value from CDK context. Returns empty string if
// the key is not set. Use cdk.json or -c key=value to set context values.
func ctxString(app awscdk.App, key string) string {
	val := app.Node().TryGetContext(jsii.String(key))
	if val == nil {
		return ""
	}
	if s, ok := val.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", val)
}
