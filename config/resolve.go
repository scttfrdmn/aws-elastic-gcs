package config

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
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

	// MaxDTNs is the number of DTN slots (1..MaxDTNs), and so the most DTNs
	// serving at once.
	MaxDTNs int
	// TotalSlots is the number of node identities the control node pre-creates
	// at setup: one per slot, so MaxDTNs.
	TotalSlots int

	// ThroughputThresholdBps is the throughput one DTN is expected to carry;
	// demand_monitor asks for ceil(total / threshold) DTNs.
	ThroughputThresholdBps int64
	// ThroughputDerivedFallback is true when the DTN type has no known sustained
	// throughput and the 500 MB/s fallback was used (the stack warns).
	ThroughputDerivedFallback bool

	GCSVersion  string
	AdminCIDR   string
	KeyPairName string

	NetworkUse NetworkUse

	// Slot controller (#65, #126).
	CostProfile    string // balanced | minimal
	WarmSlots      int
	DrainTimeout   time.Duration
	ColdTTL        time.Duration
	UnhealthyAfter time.Duration
	MaxRestarts    int
	ScaleInIdle    time.Duration
	InstanceTypes  []string // DTN types in fallback order; [0] == DTNInstance
	ControlNode    string   // always | on_demand | none
	Dashboard      bool
}

// Cost profiles (#65): presets for the idle-cost settings; any setting given
// explicitly in regions.yaml wins.
//
//	balanced (default): 2 warm slots, control node always on, dashboard.
//	minimal: no warm slots, no control node, no dashboard: about $0-1/month
//	         idle, but the first transfer after idle waits for a cold revival.
var costProfiles = map[string]struct {
	WarmSlots   int
	ControlNode string
	Dashboard   bool
	ColdTTL     time.Duration
}{
	"balanced": {WarmSlots: 2, ControlNode: "always", Dashboard: true, ColdTTL: 72 * time.Hour},
	"minimal":  {WarmSlots: 0, ControlNode: "none", Dashboard: false, ColdTTL: 24 * time.Hour},
}

// Default values (docs/instance-sizing.md, docs/configuration.md).
const (
	DefaultControlInstance = "t3a.large"
	DefaultDTNInstance     = "c8in.2xlarge"
	DefaultMaxDTNs         = 4
	DefaultGCSVersion      = "5.4.101"
	fallbackThresholdBps   = 500_000_000
	// MaxTransferConcurrency is the most concurrent files Transfer allows per task.
	MaxTransferConcurrency = 128
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
		ControlInstance: or(rc.ControlInstance, DefaultControlInstance),
		DTNInstance:     or(rc.DTNInstance, DefaultDTNInstance),
		MaxDTNs:         rc.MaxDTNs,
		GCSVersion:      or(d.GCSVersion, DefaultGCSVersion),
		AdminCIDR:       d.AdminCIDR,
		KeyPairName:     rc.KeyPairName,
	}
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	// Settings of the removed ASG pool (#133): fail loudly rather than deploy
	// something other than what the file asks for.
	if rc.Pool != "" && rc.Pool != "slots" {
		bad("pool %q: the ASG pool was removed (#133); slots is the only pool, so drop the setting", rc.Pool)
	}
	if rc.WarmPoolSize != 0 {
		bad("warm_pool_size was removed with the ASG pool (#133); use warm_slots")
	}
	if rc.UseSpot || rc.SpotMaxPrice != "" {
		bad("use_spot and spot_max_price were removed with the ASG pool (#133); Spot isn't supported on slots yet")
	}
	if rc.IdleShutdownMinutes != 0 || rc.MinUptimeMinutes != 0 {
		bad("idle_shutdown_minutes and min_uptime_minutes were removed with the ASG pool (#133); see control_node (#130)")
	}

	if rc.MaxDTNs < 0 || rc.ThroughputScaleOutMBps < 0 {
		bad("max_dtns and throughput_scaleout_mbps must be >= 0")
	}
	if r.MaxDTNs == 0 {
		r.MaxDTNs = d.MaxDTNs
	}
	if r.MaxDTNs == 0 {
		r.MaxDTNs = DefaultMaxDTNs
	}
	r.TotalSlots = r.MaxDTNs

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

	// Network use (#56). Measured in #58: one c8in.2xlarge DTN sustains ~9.2 Gbps
	// at concurrency 32 (~0.3 Gbps per file in flight), so derive 32 preferred /
	// 64 max per DTN, capped at Transfer's 128 per task; parallelism 8/16.
	nu := rc.NetworkUse
	if nu.MaxConcurrency == 0 {
		nu.MaxConcurrency = min(MaxTransferConcurrency, 64*r.MaxDTNs)
	}
	if nu.PreferredConcurrency == 0 {
		nu.PreferredConcurrency = min(nu.MaxConcurrency, 32*r.MaxDTNs)
	}
	if nu.MaxParallelism == 0 {
		nu.MaxParallelism = 16
	}
	if nu.PreferredParallelism == 0 {
		nu.PreferredParallelism = min(8, nu.MaxParallelism)
	}
	r.NetworkUse = nu
	switch {
	case nu.MaxConcurrency > MaxTransferConcurrency:
		bad("network_use.max_concurrency %d exceeds Transfer's limit of %d", nu.MaxConcurrency, MaxTransferConcurrency)
	case nu.PreferredConcurrency > nu.MaxConcurrency || nu.PreferredParallelism > nu.MaxParallelism:
		bad("network_use preferred values must not exceed the max values")
	case nu.PreferredConcurrency < 0 || nu.PreferredParallelism < 0 || nu.MaxParallelism < 0:
		bad("network_use values must be >= 0")
	}

	// Slot controller settings (#126).
	r.CostProfile = or(rc.CostProfile, "balanced")
	prof, ok := costProfiles[r.CostProfile]
	if !ok {
		bad("cost_profile %q: want balanced or minimal", rc.CostProfile)
		prof = costProfiles["balanced"]
	}
	r.WarmSlots = min(prof.WarmSlots, r.MaxDTNs) // a profile default never exceeds max_dtns
	if rc.WarmSlots != nil {
		r.WarmSlots = *rc.WarmSlots
	}
	if r.WarmSlots < 0 || r.WarmSlots > r.MaxDTNs {
		bad("warm_slots %d: want 0..max_dtns (%d)", r.WarmSlots, r.MaxDTNs)
	}
	r.ControlNode = or(rc.ControlNode, prof.ControlNode)
	if r.ControlNode != "always" && r.ControlNode != "on_demand" && r.ControlNode != "none" {
		bad("control_node %q: want always, on_demand or none", rc.ControlNode)
	}
	r.Dashboard = prof.Dashboard
	if rc.Dashboard != nil {
		r.Dashboard = *rc.Dashboard
	}
	dur := func(field, v string, def time.Duration) time.Duration {
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			bad("%s %q: want a positive duration like 10m or 72h", field, v)
			return def
		}
		return d
	}
	coldDefault := prof.ColdTTL
	if d.CacheEvictionHours > 0 { // legacy elastic-gcs:cacheEvictionHours context
		coldDefault = time.Duration(d.CacheEvictionHours) * time.Hour
	}
	r.DrainTimeout = dur("drain_timeout", rc.DrainTimeout, 10*time.Minute)
	r.ColdTTL = dur("cold_ttl", rc.ColdTTL, coldDefault)
	r.UnhealthyAfter = dur("unhealthy_after", rc.UnhealthyAfter, 5*time.Minute)
	r.ScaleInIdle = dur("scale_in_idle", rc.ScaleInIdle, 15*time.Minute)
	r.MaxRestarts = 2
	if rc.MaxRestarts != nil {
		r.MaxRestarts = *rc.MaxRestarts
		if r.MaxRestarts < 0 {
			bad("max_restarts must be >= 0")
		}
	}
	r.InstanceTypes = []string{r.DTNInstance}
	for _, t := range rc.InstanceTypes {
		if t = strings.TrimSpace(t); t == "" {
			bad("instance_types has an empty entry")
		} else if t != r.DTNInstance {
			r.InstanceTypes = append(r.InstanceTypes, t)
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

// SlotConfig returns the slot reconciler's settings (internal/slots.Config
// fields), kept here as plain values so config doesn't import the controller.
func (r Resolved) SlotConfig() (maxDTNs, warmSlots int, drain, coldTTL, unhealthyAfter time.Duration, maxRestarts int) {
	return r.MaxDTNs, r.WarmSlots, r.DrainTimeout, r.ColdTTL, r.UnhealthyAfter, r.MaxRestarts
}
