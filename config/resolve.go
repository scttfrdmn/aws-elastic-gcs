package config

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// Defaults are deployment-wide settings from CDK context (cdk.json or -c).
// Zero values mean "not set".
type Defaults struct {
	MaxDTNs            int
	CacheEvictionHours int
	ThroughputMBps     int
	GCSVersion         string
	AdminCIDR          string
}

// Resolved is every derived per-deployment value, computed once so the control
// and DTN pool stacks can't disagree (#49).
type Resolved struct {
	ControlInstance string
	DTNInstance     string

	// WarmPoolSize stopped DTNs (0 with Spot: AWS forbids warm pools for Spot).
	WarmPoolSize int
	// MaxDTNs is the most DTNs InService at once.
	MaxDTNs int
	// TotalSlots = MaxDTNs + WarmPoolSize: node identities pre-created by the
	// control node, and slots 1..TotalSlots that DTNs claim.
	TotalSlots int

	CacheEvictionHours int
	// ThroughputThresholdBps triggers the multi-DTN scale-out alarm.
	ThroughputThresholdBps int64
	// ThroughputDerivedFallback is true when the DTN type has no known sustained
	// throughput and the 500 MB/s fallback was used (the stack warns).
	ThroughputDerivedFallback bool

	IdleShutdownMinutes int
	MinUptimeMinutes    int

	UseSpot      bool
	SpotMaxPrice string

	GCSVersion  string
	AdminCIDR   string
	KeyPairName string
}

// Default values (docs/instance-sizing.md, docs/configuration.md).
const (
	DefaultControlInstance    = "t3a.large"
	DefaultDTNInstance        = "c8in.2xlarge"
	DefaultWarmPoolSize       = 2
	DefaultCacheEvictionHours = 72
	DefaultMinUptimeMinutes   = 30
	DefaultGCSVersion         = "5.4.101"
	fallbackThresholdBps      = 500_000_000
)

// DTNSustainedBytesPerSec is the sustained multi-flow internet-gateway
// throughput of recommended DTN types; the default scale-out threshold is 70%
// of it. The *8in families get their full baseline; most other types under 32
// vCPUs are capped at 5 Gbps. Regenerate with scripts/instance_sizing.py.
var DTNSustainedBytesPerSec = map[string]int64{
	"m8in.xlarge":  781_250_000,   // 6.25 Gbps
	"c8in.2xlarge": 1_562_500_000, // 12.5 Gbps
	"m8in.2xlarge": 1_562_500_000, // 12.5 Gbps
	"c8in.4xlarge": 3_125_000_000, // 25 Gbps
	"m8in.4xlarge": 3_125_000_000, // 25 Gbps
	"c8in.8xlarge": 6_250_000_000, // 50 Gbps
	"m6in.xlarge":  625_000_000,   // 5 Gbps IGW cap
	"c6in.2xlarge": 625_000_000,   // 5 Gbps IGW cap
	"m6i.xlarge":   195_250_000,   // 1.562 Gbps baseline
}

var gcsVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// Resolve applies defaults to one regions.yaml entry and validates the result.
// Per-deployment settings in regions.yaml win over CDK context defaults.
func Resolve(rc RegionConfig, d Defaults) (Resolved, error) {
	r := Resolved{
		ControlInstance:     or(rc.ControlInstance, DefaultControlInstance),
		DTNInstance:         or(rc.DTNInstance, DefaultDTNInstance),
		WarmPoolSize:        rc.WarmPoolSize,
		MaxDTNs:             rc.MaxDTNs,
		CacheEvictionHours:  d.CacheEvictionHours,
		IdleShutdownMinutes: rc.IdleShutdownMinutes,
		MinUptimeMinutes:    rc.MinUptimeMinutes,
		UseSpot:             rc.UseSpot,
		SpotMaxPrice:        rc.SpotMaxPrice,
		GCSVersion:          or(d.GCSVersion, DefaultGCSVersion),
		AdminCIDR:           d.AdminCIDR,
		KeyPairName:         rc.KeyPairName,
	}
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if rc.WarmPoolSize < 0 || rc.MaxDTNs < 0 || rc.IdleShutdownMinutes < 0 || rc.MinUptimeMinutes < 0 || rc.ThroughputScaleOutMBps < 0 {
		bad("warm_pool_size, max_dtns, idle_shutdown_minutes, min_uptime_minutes and throughput_scaleout_mbps must be >= 0")
	}
	if r.WarmPoolSize == 0 {
		r.WarmPoolSize = DefaultWarmPoolSize
	}
	if r.UseSpot {
		r.WarmPoolSize = 0 // AWS rejects warm pools with Spot
	}
	if r.MaxDTNs == 0 {
		r.MaxDTNs = d.MaxDTNs
	}
	if r.MaxDTNs == 0 {
		r.MaxDTNs = max(r.WarmPoolSize+1, 1)
	}
	r.TotalSlots = r.MaxDTNs + r.WarmPoolSize

	if r.CacheEvictionHours <= 0 {
		r.CacheEvictionHours = DefaultCacheEvictionHours
	}
	if r.IdleShutdownMinutes > 0 && r.MinUptimeMinutes == 0 {
		r.MinUptimeMinutes = DefaultMinUptimeMinutes
	}

	switch {
	case d.ThroughputMBps > 0:
		r.ThroughputThresholdBps = int64(d.ThroughputMBps) * 1_000_000
	case rc.ThroughputScaleOutMBps > 0:
		r.ThroughputThresholdBps = int64(rc.ThroughputScaleOutMBps) * 1_000_000
	default:
		if bps, ok := DTNSustainedBytesPerSec[r.DTNInstance]; ok {
			r.ThroughputThresholdBps = bps * 7 / 10
		} else {
			r.ThroughputThresholdBps = fallbackThresholdBps
			r.ThroughputDerivedFallback = true
		}
	}

	if !gcsVersionRe.MatchString(r.GCSVersion) {
		bad("globus:gcsVersion %q is not an exact version like 5.4.101", r.GCSVersion)
	}
	if r.AdminCIDR != "" {
		if _, _, err := net.ParseCIDR(r.AdminCIDR); err != nil {
			bad("adminCIDR %q is not a CIDR block", r.AdminCIDR)
		} else if r.AdminCIDR == "0.0.0.0/0" {
			bad("adminCIDR 0.0.0.0/0 opens SSH to the internet; use SSM Session Manager or a narrow CIDR")
		}
	}
	if r.SpotMaxPrice != "" {
		if _, err := strconv.ParseFloat(r.SpotMaxPrice, 64); err != nil {
			bad("spot_max_price %q is not a number", r.SpotMaxPrice)
		}
	}
	if len(errs) > 0 {
		return r, fmt.Errorf("%s/%s: %s", rc.Name, or(rc.Deployment, "default"), strings.Join(errs, "; "))
	}
	return r, nil
}

// CtxInt parses an integer CDK context value; "" is 0.
func CtxInt(key, v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("context %s=%q: want a non-negative integer", key, v)
	}
	return n, nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
