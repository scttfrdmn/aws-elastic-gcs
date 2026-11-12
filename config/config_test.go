package config

import (
	"testing"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

func TestExampleConfigsParse(t *testing.T) {
	r, err := LoadRegions("regions.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, rc := range r.Regions {
		d, err := names.New(rc.Name, rc.Deployment)
		if err != nil {
			t.Errorf("regions.example.yaml: %v", err)
		}
		if seen[d.ID()] {
			t.Errorf("regions.example.yaml: duplicate deployment %s", d.ID())
		}
		seen[d.ID()] = true
	}
	if _, err := LoadOpenDataBuckets("open_data_buckets.example.yaml"); err != nil {
		t.Fatal(err)
	}
}

func TestBucketsFor(t *testing.T) {
	c := OpenDataBucketsConfig{Buckets: []OpenDataBucket{
		{Name: "all", Region: "us-west-2"},
		{Name: "dst-only", Region: "us-west-2", Deployments: []string{"dst"}},
		{Name: "east", Region: "us-east-1"},
	}}
	got := func(region, dep string) []string {
		var out []string
		for _, b := range c.BucketsFor(region, dep) {
			out = append(out, b.Name)
		}
		return out
	}
	if g := got("us-west-2", "src"); len(g) != 1 || g[0] != "all" {
		t.Errorf("src: got %v, want [all]", g)
	}
	if g := got("us-west-2", "dst"); len(g) != 2 {
		t.Errorf("dst: got %v, want [all dst-only]", g)
	}
	if g := got("us-east-1", "default"); len(g) != 1 || g[0] != "east" {
		t.Errorf("us-east-1: got %v, want [east]", g)
	}
}
