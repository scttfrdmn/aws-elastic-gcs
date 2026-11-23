package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// RegionConfig defines per-region deployment settings
type RegionConfig struct {
	Name string `yaml:"name"`
	// Deployment distinguishes independent endpoints in the same region
	// (1-12 lowercase letters/digits; default "default"). Every resource name,
	// secret, SSM path and metric dimension includes it; see internal/names.
	Deployment string `yaml:"deployment"`
	// The Globus endpoint this deployment creates (#162). Owner and
	// subscription are required: there are no defaults, so nobody deploys
	// under someone else's subscription or ownership.
	GlobusEndpointOwner  string `yaml:"globus_endpoint_owner"`  // Globus username, e.g. you@globusid.org
	GlobusSubscriptionID string `yaml:"globus_subscription_id"` // subscription UUID (multi-node endpoints, S3 connector)
	GlobusEndpointName   string `yaml:"globus_endpoint_name"`   // display name (default "Elastic GCS <region>-<deployment>")
	GlobusOrganization   string `yaml:"globus_organization"`    // organization shown on the endpoint (default "Elastic GCS")
	ControlInstance      string `yaml:"control_instance"`
	DTNInstance          string `yaml:"dtn_instance"`
	MaxDTNs              int    `yaml:"max_dtns"`
	KeyPairName          string `yaml:"key_pair_name"`

	// ThroughputScaleOutMBps: aggregate throughput (MB/s) per DTN. demand_monitor
	// sets desired-dtns to ceil(total throughput of ACTIVE tasks / threshold).
	// 0 = derive ~70% of the DTN type's sustained throughput (docs/instance-sizing.md).
	ThroughputScaleOutMBps int `yaml:"throughput_scaleout_mbps"`

	// Removed with the ASG pool (#133). Kept only so config.Resolve can reject
	// them with a pointer to their replacement instead of silently ignoring them.
	WarmPoolSize        int    `yaml:"warm_pool_size"`        // use warm_slots
	UseSpot             bool   `yaml:"use_spot"`              // not yet supported on slots
	SpotMaxPrice        string `yaml:"spot_max_price"`        // not yet supported on slots
	IdleShutdownMinutes int    `yaml:"idle_shutdown_minutes"` // see control_node (#130)
	MinUptimeMinutes    int    `yaml:"min_uptime_minutes"`    // see control_node (#130)

	// NetworkUse sets the endpoint's Globus network use (#56). A transfer's
	// concurrency is the lower of the two endpoints' maximums, so leaving this
	// endpoint at Globus's default ("normal", concurrency 4) caps every task.
	// Zero fields are derived from max_dtns; see config.Resolve.
	NetworkUse NetworkUse `yaml:"network_use"`

	// Slot controller (#65). See docs/configuration.md for defaults.
	Pool           string `yaml:"pool"`            // optional; slots is the only pool (#133)
	CostProfile    string `yaml:"cost_profile"`    // balanced | minimal (default balanced)
	WarmSlots      *int   `yaml:"warm_slots"`      // WARM slots kept when idle
	DrainTimeout   string `yaml:"drain_timeout"`   // e.g. 10m
	ColdTTL        string `yaml:"cold_ttl"`        // e.g. 72h
	UnhealthyAfter string `yaml:"unhealthy_after"` // e.g. 5m
	// ControlDemoteAfter: take the control node out of the data path once a
	// DTN has been healthy this long (#146); "off" keeps it serving.
	ControlDemoteAfter string   `yaml:"control_demote_after"`
	MaxRestarts        *int     `yaml:"max_restarts"`
	ScaleInIdle        string   `yaml:"scale_in_idle"`  // no active transfers this long -> scale to 0
	InstanceTypes      []string `yaml:"instance_types"` // DTN types in fallback order (first wins)
	ControlNode        string   `yaml:"control_node"`   // always | on_demand | none
	Dashboard          *bool    `yaml:"dashboard"`
}

// NetworkUse is the endpoint's custom network-use policy.
type NetworkUse struct {
	PreferredConcurrency int `yaml:"preferred_concurrency"`
	MaxConcurrency       int `yaml:"max_concurrency"`
	PreferredParallelism int `yaml:"preferred_parallelism"`
	MaxParallelism       int `yaml:"max_parallelism"`
}

// Args are the `globus-connect-server endpoint update` flags for n.
func (n NetworkUse) Args() string {
	return fmt.Sprintf("--network-use custom --preferred-concurrency %d --max-concurrency %d --preferred-parallelism %d --max-parallelism %d",
		n.PreferredConcurrency, n.MaxConcurrency, n.PreferredParallelism, n.MaxParallelism)
}

// RegionsConfig is the top-level regions configuration
type RegionsConfig struct {
	Regions []RegionConfig `yaml:"regions"`
}

// OpenDataBucket defines a single Open Data bucket
type OpenDataBucket struct {
	Name          string `yaml:"name"            json:"name"`
	Bucket        string `yaml:"bucket"          json:"bucket"`
	Region        string `yaml:"region"          json:"region"`
	Description   string `yaml:"description"     json:"description"`
	RequesterPays bool   `yaml:"requester_pays"  json:"requester_pays"`
	ReadOnly      bool   `yaml:"read_only"       json:"read_only"`
	// CollectionName is the display name for the mapped collection.
	// Defaults to "Open Data: {Name}" if empty.
	CollectionName string `yaml:"collection_name" json:"collection_name,omitempty"`
	// Domain restricts which Globus identity provider is accepted by the storage gateway.
	// Defaults to "globusid.org" if empty.
	Domain string `yaml:"domain" json:"domain,omitempty"`
	// Deployments limits the bucket to the named deployments in its region.
	// Empty means every deployment in the region.
	Deployments []string `yaml:"deployments" json:"-"`
}

// GatewayName returns the display name to use for the storage gateway.
func (b OpenDataBucket) GatewayName() string { return b.Name }

// ColName returns the display name to use for the mapped collection.
func (b OpenDataBucket) ColName() string {
	if b.CollectionName != "" {
		return b.CollectionName
	}
	return "Open Data: " + b.Name
}

// GWDomain returns the identity domain for the storage gateway.
func (b OpenDataBucket) GWDomain() string {
	if b.Domain != "" {
		return b.Domain
	}
	return "globusid.org"
}

// OpenDataBucketsConfig is the top-level Open Data configuration
type OpenDataBucketsConfig struct {
	Buckets []OpenDataBucket `yaml:"buckets"`
}

// BucketsFor returns the buckets for one deployment: those in its region whose
// Deployments list is empty or includes it.
func (c *OpenDataBucketsConfig) BucketsFor(region, deployment string) []OpenDataBucket {
	var result []OpenDataBucket
	for _, b := range c.Buckets {
		if b.Region != region {
			continue
		}
		if len(b.Deployments) == 0 {
			result = append(result, b)
			continue
		}
		for _, d := range b.Deployments {
			if d == deployment {
				result = append(result, b)
				break
			}
		}
	}
	return result
}

// LoadRegions reads the regions configuration from a YAML file
func LoadRegions(path string) (*RegionsConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg RegionsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadOpenDataBuckets reads the Open Data bucket configuration from a YAML file
func LoadOpenDataBuckets(path string) (*OpenDataBucketsConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg OpenDataBucketsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
