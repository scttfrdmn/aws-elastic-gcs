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

// EvictPool reports whether the warm pool as a whole has gone unused for ttl
// and should be removed (#33): nothing is desired and every warm instance
// entered the pool more than ttl ago. Evicting only some instances doesn't
// save anything: the ASG refills the pool to MaxGroupPreparedCapacity.
func EvictPool(entered []time.Time, desired int, now time.Time, ttl time.Duration) bool {
	if desired > 0 || len(entered) == 0 {
		return false
	}
	for _, e := range entered {
		if !Stale(e, now, ttl) {
			return false
		}
	}
	return true
}
