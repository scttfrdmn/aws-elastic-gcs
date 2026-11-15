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
