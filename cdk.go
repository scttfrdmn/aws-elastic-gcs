package main

import (
	"fmt"
	"os"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/elastic-gcs/config"
	"github.com/scttfrdmn/elastic-gcs/stacks"
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

	// CDK_DEFAULT_ACCOUNT is injected by the CDK CLI when running cdk commands.
	// It is required for context lookups (e.g. MachineImage_Lookup for AMI IDs).
	account := os.Getenv("CDK_DEFAULT_ACCOUNT")

	// Deploy stacks per region
	for _, region := range regions.Regions {
		env := &awscdk.Environment{
			Account: jsii.String(account),
			Region:  jsii.String(region.Name),
		}

		regionBuckets := buckets.BucketsForRegion(region.Name)

		// Phase 1 + 2: Network + Control Node
		controlStackName := fmt.Sprintf("ElasticGCS-Control-%s", region.Name)
		// Phase 3: DTN Pool (depends on control stack)
		cacheHours := 72
		if h := ctxString(app, "elastic-gcs:cacheEvictionHours"); h != "" {
			fmt.Sscanf(h, "%d", &cacheHours)
		}
		maxDTNs := 0
		if m := ctxString(app, "elastic-gcs:maxDTNs"); m != "" {
			fmt.Sscanf(m, "%d", &maxDTNs)
		}
		if maxDTNs == 0 {
			maxDTNs = region.WarmPoolSize + 1
		}

		controlStack := stacks.NewControlNodeStack(app, controlStackName, &stacks.ControlNodeStackProps{
			StackProps: awscdk.StackProps{
				Env:         env,
				Description: jsii.String(fmt.Sprintf("Elastic GCS Control Node - %s", region.Name)),
			},
			RegionConfig:         region,
			OpenDataBuckets:      regionBuckets,
			GlobusSubscriptionID: globusSubscriptionID,
			GlobusOwnerIdentity:  globusOwnerIdentity,
			EndpointDisplayName:  endpointDisplayName,
			OrganizationName:     orgName,
			AdminCIDR:            adminCIDR,
			MaxDTNs:              maxDTNs,
		})
		dtnStackName := fmt.Sprintf("ElasticGCS-DTNPool-%s", region.Name)
		stacks.NewDTNPoolStack(app, dtnStackName, &stacks.DTNPoolStackProps{
			StackProps: awscdk.StackProps{
				Env:         env,
				Description: jsii.String(fmt.Sprintf("Elastic GCS DTN Pool - %s", region.Name)),
			},
			RegionConfig:       region,
			ControlStack:       controlStack,
			OpenDataBuckets:    regionBuckets,
			CacheEvictionHours: cacheHours,
			MaxDTNs:            maxDTNs,
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
