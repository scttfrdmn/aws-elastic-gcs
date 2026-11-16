package config

import (
	"reflect"
	"testing"
	"time"
)

func ip(n int) *int   { return &n }
func bp(b bool) *bool { return &b }

func TestResolve(t *testing.T) {
	cases := []struct {
		name    string
		rc      RegionConfig
		d       Defaults
		check   func(Resolved) bool
		wantErr bool
	}{
		{"defaults", RegionConfig{Name: "us-west-2"}, Defaults{}, func(r Resolved) bool {
			return r.MaxDTNs == 4 && r.TotalSlots == 4 &&
				r.DTNInstance == "c8in.2xlarge" && r.ControlInstance == "t3a.large" && r.GCSVersion == "5.4.101" &&
				r.ThroughputThresholdBps == 1_093_750_000 && !r.ThroughputDerivedFallback
		}, false},
		{"regions.yaml max_dtns wins over context (#49)", RegionConfig{MaxDTNs: 8}, Defaults{MaxDTNs: 4}, func(r Resolved) bool {
			return r.MaxDTNs == 8 && r.TotalSlots == 8
		}, false},
		{"context max_dtns when unset", RegionConfig{}, Defaults{MaxDTNs: 6}, func(r Resolved) bool { return r.MaxDTNs == 6 && r.TotalSlots == 6 }, false},
		{"pool: slots is accepted", RegionConfig{Pool: "slots"}, Defaults{}, nil, false},
		{"context throughput wins", RegionConfig{ThroughputScaleOutMBps: 900}, Defaults{ThroughputMBps: 800}, func(r Resolved) bool {
			return r.ThroughputThresholdBps == 800_000_000
		}, false},
		{"unknown type falls back", RegionConfig{DTNInstance: "x9.huge"}, Defaults{}, func(r Resolved) bool {
			return r.ThroughputThresholdBps == 500_000_000 && r.ThroughputDerivedFallback
		}, false},
		{"network use for 1 DTN", RegionConfig{MaxDTNs: 1}, Defaults{}, func(r Resolved) bool {
			return r.NetworkUse == NetworkUse{32, 64, 8, 16}
		}, false},
		{"network use for 2 DTNs", RegionConfig{MaxDTNs: 2}, Defaults{}, func(r Resolved) bool {
			return r.NetworkUse == NetworkUse{64, 128, 8, 16}
		}, false},
		{"network use capped at 128", RegionConfig{MaxDTNs: 8}, Defaults{}, func(r Resolved) bool {
			return r.NetworkUse == NetworkUse{128, 128, 8, 16} &&
				r.NetworkUse.Args() == "--network-use custom --preferred-concurrency 128 --max-concurrency 128 --preferred-parallelism 8 --max-parallelism 16"
		}, false},
		{"explicit network use kept", RegionConfig{MaxDTNs: 8, NetworkUse: NetworkUse{16, 32, 4, 8}}, Defaults{}, func(r Resolved) bool {
			return r.NetworkUse == NetworkUse{16, 32, 4, 8}
		}, false},
		{"network use over 128", RegionConfig{NetworkUse: NetworkUse{MaxConcurrency: 256}}, Defaults{}, nil, true},
		{"preferred over max", RegionConfig{NetworkUse: NetworkUse{PreferredConcurrency: 64, MaxConcurrency: 32}}, Defaults{}, nil, true},
		{"slot defaults (balanced)", RegionConfig{MaxDTNs: 8}, Defaults{}, func(r Resolved) bool {
			return r.CostProfile == "balanced" && r.WarmSlots == 2 && r.ControlNode == "always" && r.Dashboard &&
				r.DrainTimeout == 10*time.Minute && r.ColdTTL == 72*time.Hour && r.UnhealthyAfter == 5*time.Minute &&
				r.ScaleInIdle == 15*time.Minute && r.MaxRestarts == 2 && len(r.InstanceTypes) == 1 && r.InstanceTypes[0] == "c8in.2xlarge" &&
				r.ControlDemoteAfter == 2*time.Minute
		}, false},
		{"control demotion off", RegionConfig{MaxDTNs: 4, ControlDemoteAfter: "off"}, Defaults{}, func(r Resolved) bool {
			return r.ControlDemoteAfter == 0
		}, false},
		{"control demotion after 5m", RegionConfig{MaxDTNs: 4, ControlDemoteAfter: "5m"}, Defaults{}, func(r Resolved) bool {
			return r.ControlDemoteAfter == 5*time.Minute
		}, false},
		{"control demotion bad value", RegionConfig{MaxDTNs: 4, ControlDemoteAfter: "soon"}, Defaults{}, nil, true},
		{"minimal profile", RegionConfig{MaxDTNs: 4, CostProfile: "minimal"}, Defaults{}, func(r Resolved) bool {
			return r.WarmSlots == 0 && r.ControlNode == "none" && !r.Dashboard && r.ColdTTL == 24*time.Hour
		}, false},
		{"explicit settings beat the profile", RegionConfig{MaxDTNs: 4, CostProfile: "minimal", WarmSlots: ip(1), ControlNode: "on_demand", Dashboard: bp(true), ColdTTL: "6h"}, Defaults{}, func(r Resolved) bool {
			return r.WarmSlots == 1 && r.ControlNode == "on_demand" && r.Dashboard && r.ColdTTL == 6*time.Hour
		}, false},
		{"explicit zero warm slots", RegionConfig{MaxDTNs: 4, WarmSlots: ip(0)}, Defaults{}, func(r Resolved) bool { return r.WarmSlots == 0 }, false},
		{"profile default clamps to max_dtns", RegionConfig{MaxDTNs: 1}, Defaults{}, func(r Resolved) bool { return r.WarmSlots == 1 }, false},
		{"legacy cache eviction hours feed cold_ttl", RegionConfig{}, Defaults{CacheEvictionHours: 48}, func(r Resolved) bool { return r.ColdTTL == 48*time.Hour }, false},
		{"instance type fallback list", RegionConfig{DTNInstance: "c8in.2xlarge", InstanceTypes: []string{"c8in.2xlarge", "m8in.2xlarge", "c8in.4xlarge"}}, Defaults{}, func(r Resolved) bool {
			return reflect.DeepEqual(r.InstanceTypes, []string{"c8in.2xlarge", "m8in.2xlarge", "c8in.4xlarge"})
		}, false},
		{"bad pool", RegionConfig{Pool: "k8s"}, Defaults{}, nil, true},
		{"the asg pool was removed (#133)", RegionConfig{Pool: "asg"}, Defaults{}, nil, true},
		{"warm_pool_size was removed", RegionConfig{WarmPoolSize: 8}, Defaults{}, nil, true},
		{"spot was removed", RegionConfig{UseSpot: true}, Defaults{}, nil, true},
		{"spot price was removed", RegionConfig{SpotMaxPrice: "0.2"}, Defaults{}, nil, true},
		{"idle shutdown was removed", RegionConfig{IdleShutdownMinutes: 20}, Defaults{}, nil, true},
		{"bad cost profile", RegionConfig{CostProfile: "cheap"}, Defaults{}, nil, true},
		{"bad control node", RegionConfig{ControlNode: "sometimes"}, Defaults{}, nil, true},
		{"warm slots over max", RegionConfig{MaxDTNs: 2, WarmSlots: ip(3)}, Defaults{}, nil, true},
		{"bad duration", RegionConfig{DrainTimeout: "soon"}, Defaults{}, nil, true},
		{"bad gcs version", RegionConfig{}, Defaults{GCSVersion: "5.4"}, nil, true},
		{"bad cidr", RegionConfig{}, Defaults{AdminCIDR: "10.0.0.0/33"}, nil, true},
		{"world cidr", RegionConfig{}, Defaults{AdminCIDR: "0.0.0.0/0"}, nil, true},
		{"negative", RegionConfig{MaxDTNs: -1}, Defaults{}, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Resolve(c.rc, c.d)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, c.wantErr)
			}
			if c.check != nil && !c.check(r) {
				t.Fatalf("unexpected %+v", r)
			}
		})
	}
}

func TestCtxInt(t *testing.T) {
	if n, err := CtxInt("k", ""); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := CtxInt("k", " 12 "); n != 12 || err != nil {
		t.Fatal(n, err)
	}
	for _, v := range []string{"x", "-3", "1.5"} {
		if _, err := CtxInt("k", v); err == nil {
			t.Errorf("%q: want error", v)
		}
	}
}

func TestExampleRegionsResolve(t *testing.T) {
	r, err := LoadRegions("regions.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, rc := range r.Regions {
		if _, err := Resolve(rc, Defaults{}); err != nil {
			t.Errorf("regions.example.yaml: %v", err)
		}
	}
}
