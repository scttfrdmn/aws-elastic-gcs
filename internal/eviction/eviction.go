// Package eviction holds cache eviction's pure decisions (#52).
package eviction

import (
	"time"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/gcscli"
)

// OrphanNodes returns the IDs of endpoint nodes that are safe to delete:
// inactive, not mapped to a live DTN instance (liveNodeIDs), and not at a
// protected IP. The control node's IP must be protected: it isn't a DTN, and
// when it's out of the data path its node is inactive.
func OrphanNodes(nodes []gcscli.Node, liveNodeIDs, protectedIPs map[string]bool) []string {
	var del []string
	for _, n := range nodes {
		if n.Active() || liveNodeIDs[n.ID] {
			continue
		}
		protected := false
		for _, ip := range append(append([]string{}, n.IPAddresses...), n.IPAddress) {
			if ip != "" && protectedIPs[ip] {
				protected = true
			}
		}
		if !protected {
			del = append(del, n.ID)
		}
	}
	return del
}

// Stale reports whether a warm-pool instance that entered the pool at entered
// has been there longer than ttl.
func Stale(entered, now time.Time, ttl time.Duration) bool {
	return !entered.IsZero() && now.Sub(entered) > ttl
}
