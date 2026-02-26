package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

// RegionConfig defines per-region deployment settings
type RegionConfig struct {
	Name            string `yaml:"name"`
	ControlInstance string `yaml:"control_instance"`
	DTNInstance     string `yaml:"dtn_instance"`
	WarmPoolSize    int    `yaml:"warm_pool_size"`
	MaxDTNs         int    `yaml:"max_dtns"`
	KeyPairName     string `yaml:"key_pair_name"`
	// Spot instance settings for DTNs
	UseSpot      bool   `yaml:"use_spot"`
	SpotMaxPrice string `yaml:"spot_max_price"` // empty = up to on-demand price

	// Ephemeral control node: automatically stop when idle.
	// 0 = disabled (control node runs continuously, the safe default).
	// When non-zero the idle_shutdown Lambda runs every 15 minutes and stops
	// the control node after IdleShutdownMinutes of inactivity.
	// See docs/ephemeral-control-node.md for availability implications before
	// enabling this — the endpoint becomes unreachable while stopped.
	IdleShutdownMinutes int `yaml:"idle_shutdown_minutes"`
	// MinUptimeMinutes prevents stop/start thrash: the control node must have
	// been running for at least this long before idle shutdown is eligible.
	// Default 30 minutes when IdleShutdownMinutes > 0.
	MinUptimeMinutes int `yaml:"min_uptime_minutes"`
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

// BucketsForRegion returns buckets available in a specific region
func (c *OpenDataBucketsConfig) BucketsForRegion(region string) []OpenDataBucket {
	var result []OpenDataBucket
	for _, b := range c.Buckets {
		if b.Region == region {
			result = append(result, b)
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
