package config

import "testing"

func TestResolve(t *testing.T) {
	cases := []struct {
		name    string
		rc      RegionConfig
		d       Defaults
		check   func(Resolved) bool
		wantErr bool
	}{
		{"defaults", RegionConfig{Name: "us-west-2"}, Defaults{}, func(r Resolved) bool {
			return r.WarmPoolSize == 2 && r.MaxDTNs == 3 && r.TotalSlots == 5 && r.CacheEvictionHours == 72 &&
				r.DTNInstance == "c8in.2xlarge" && r.ControlInstance == "t3a.large" && r.GCSVersion == "5.4.101" &&
				r.ThroughputThresholdBps == 1_093_750_000 && !r.ThroughputDerivedFallback
		}, false},
		{"regions.yaml max_dtns wins over context (#49)", RegionConfig{MaxDTNs: 8, WarmPoolSize: 8}, Defaults{MaxDTNs: 4}, func(r Resolved) bool {
			return r.MaxDTNs == 8 && r.TotalSlots == 16
		}, false},
		{"context max_dtns when unset", RegionConfig{}, Defaults{MaxDTNs: 4}, func(r Resolved) bool { return r.MaxDTNs == 4 && r.TotalSlots == 6 }, false},
		{"spot drops the warm pool", RegionConfig{UseSpot: true, MaxDTNs: 3}, Defaults{}, func(r Resolved) bool {
			return r.WarmPoolSize == 0 && r.TotalSlots == 3
		}, false},
		{"spot with nothing set still has one DTN", RegionConfig{UseSpot: true}, Defaults{}, func(r Resolved) bool { return r.MaxDTNs == 1 }, false},
		{"idle shutdown gets min uptime", RegionConfig{IdleShutdownMinutes: 20}, Defaults{}, func(r Resolved) bool { return r.MinUptimeMinutes == 30 }, false},
		{"context throughput wins", RegionConfig{ThroughputScaleOutMBps: 900}, Defaults{ThroughputMBps: 800}, func(r Resolved) bool {
			return r.ThroughputThresholdBps == 800_000_000
		}, false},
		{"unknown type falls back", RegionConfig{DTNInstance: "x9.huge"}, Defaults{}, func(r Resolved) bool {
			return r.ThroughputThresholdBps == 500_000_000 && r.ThroughputDerivedFallback
		}, false},
		{"bad gcs version", RegionConfig{}, Defaults{GCSVersion: "5.4"}, nil, true},
		{"bad cidr", RegionConfig{}, Defaults{AdminCIDR: "10.0.0.0/33"}, nil, true},
		{"world cidr", RegionConfig{}, Defaults{AdminCIDR: "0.0.0.0/0"}, nil, true},
		{"negative", RegionConfig{WarmPoolSize: -1}, Defaults{}, nil, true},
		{"bad spot price", RegionConfig{UseSpot: true, SpotMaxPrice: "cheap"}, Defaults{}, nil, true},
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
