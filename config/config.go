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
}

// RegionsConfig is the top-level regions configuration
type RegionsConfig struct {
	Regions []RegionConfig `yaml:"regions"`
}

// OpenDataBucket defines a single Open Data bucket
type OpenDataBucket struct {
	Name          string `yaml:"name"           json:"name"`
	Bucket        string `yaml:"bucket"         json:"bucket"`
	Region        string `yaml:"region"         json:"region"`
	Description   string `yaml:"description"    json:"description"`
	RequesterPays bool   `yaml:"requester_pays" json:"requester_pays"`
	ReadOnly      bool   `yaml:"read_only"      json:"read_only"`
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
