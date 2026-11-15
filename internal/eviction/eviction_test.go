package eviction

import (
	"reflect"
	"testing"
	"time"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/gcscli"
)

func TestOrphanNodes(t *testing.T) {
	nodes := []gcscli.Node{
		{ID: "active", Status: "active", IPAddresses: []string{"1.1.1.1"}},
		{ID: "warm", Status: "inactive", IPAddresses: []string{"2.2.2.2"}},
		{ID: "control-disabled", Status: "inactive", IPAddresses: []string{"100.20.14.104"}},
		{ID: "control-old-format", Status: "inactive", IPAddress: "100.20.14.104"},
		{ID: "ghost", Status: "inactive", IPAddresses: []string{"3.3.3.3"}},
	}
	got := OrphanNodes(nodes, map[string]bool{"warm": true}, map[string]bool{"100.20.14.104": true})
	if !reflect.DeepEqual(got, []string{"ghost"}) {
		t.Fatalf("got %v", got)
	}
}

func TestStale(t *testing.T) {
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	if !Stale(now.Add(-73*time.Hour), now, 72*time.Hour) || Stale(now.Add(-1*time.Hour), now, 72*time.Hour) || Stale(time.Time{}, now, time.Hour) {
		t.Fatal("Stale")
	}
}

func TestEvictPool(t *testing.T) {
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	old, fresh := now.Add(-80*time.Hour), now.Add(-2*time.Hour)
	ttl := 72 * time.Hour
	cases := []struct {
		name    string
		entered []time.Time
		desired int
		want    bool
	}{
		{"all stale, idle", []time.Time{old, old}, 0, true},
		{"one fresh", []time.Time{old, fresh}, 0, false},
		{"in use", []time.Time{old, old}, 1, false},
		{"empty pool", nil, 0, false},
	}
	for _, c := range cases {
		if got := EvictPool(c.entered, c.desired, now, ttl); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
