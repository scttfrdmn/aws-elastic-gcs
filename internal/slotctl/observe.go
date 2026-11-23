// Package slotctl is the AWS-facing half of the slot controller (#128): it
// observes slots (instances, Elastic IPs, controller-owned tags, SSM health,
// endpoint DNS, GridFTP data connections) into an internal/slots.Observed, and
// executes the actions internal/slots.Plan returns.
//
// Controller state lives in instance tags only the controller can write (the
// DTN role may tag just elastic-gcs:slot and elastic-gcs:gcs-node-id):
//
//	elastic-gcs:phase           starting | reviving | draining | stopping | evicting
//	elastic-gcs:phase-since     RFC 3339
//	elastic-gcs:unhealthy-since RFC 3339 (HOT slots that failed a health check)
//	elastic-gcs:healthy-since   RFC 3339 (HOT slots passing it; for control demotion)
//	elastic-gcs:restarts        restarts since last healthy
//	elastic-gcs:stopped-at      RFC 3339 (WARM slots, for cold_ttl)
package slotctl

import (
	"strconv"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/slots"
)

// Controller-owned tag keys.
const (
	TagPhase          = "elastic-gcs:phase"
	TagPhaseSince     = "elastic-gcs:phase-since"
	TagUnhealthySince = "elastic-gcs:unhealthy-since"
	TagHealthySince   = "elastic-gcs:healthy-since"
	TagRestarts       = "elastic-gcs:restarts"
	TagStoppedAt      = "elastic-gcs:stopped-at"
	TagPool           = "elastic-gcs:pool" // "slots" on controller-owned resources
)

// Probe is what the controller learned about a running slot this round.
type Probe struct {
	SSMOnline bool // SSM agent ping Online
	InDNS     bool // the slot's EIP is in endpoint DNS
	GCSActive bool // gcs_manager and globus-gridftp-server are active on the node
	DataConns int  // established GridFTP data connections (ports 50000-51000)
}

// ControlState is the control node as the controller sees it.
type ControlState struct {
	Running bool // instance running
	Serving bool // running and its EIP in endpoint DNS
}

// SlotFromInstance turns an instance (and, if running, its probe) into the
// reconciler's view. A slot with no live instance is COLD; callers add those.
func SlotFromInstance(inst ec2types.Instance, p Probe, now time.Time) (slots.Slot, bool) {
	t := tagMap(inst.Tags)
	n, err := strconv.Atoi(t[names.TagSlot])
	if err != nil || n <= 0 {
		return slots.Slot{}, false
	}
	s := slots.Slot{N: n, Phase: slots.Phase(t[TagPhase])}
	state := ""
	if inst.State != nil {
		state = string(inst.State.Name)
	}
	switch state {
	case "pending", "running":
		s.Tier = slots.Hot
		s.DataConns = p.DataConns
		// DNS alone isn't enough: a slot's EIP stays in endpoint DNS after its
		// instance dies without a disable, so the node must also be serving.
		s.Healthy = state == "running" && p.SSMOnline && p.InDNS && p.GCSActive
		if s.Healthy {
			s.HealthyFor = since(t[TagHealthySince], now)
		} else {
			s.UnhealthyFor = since(t[TagUnhealthySince], now)
		}
		s.RestartAttempts, _ = strconv.Atoi(t[TagRestarts])
		if s.Phase == slots.Draining {
			s.DrainingFor = since(t[TagPhaseSince], now)
		}
		// A started slot is no longer "starting" once healthy.
		if s.Phase == slots.Starting && s.Healthy {
			s.Phase = slots.Stable
		}
		if s.Phase == slots.Reviving && s.Healthy {
			s.Phase = slots.Stable
		}
	case "stopping", "stopped":
		s.Tier = slots.Warm
		if state == "stopping" {
			s.Phase = slots.Stopping
		} else if s.Phase == slots.Stopping || s.Phase == slots.Draining {
			s.Phase = slots.Stable // stop finished
		}
		s.StoppedFor = since(t[TagStoppedAt], now)
	case "shutting-down":
		// Still terminating (Rebuild, Evict): COLD but in flight, so Plan
		// won't revive the slot until the old instance is gone. Two instances
		// must never run the same node identity at once (Globus, #151).
		return slots.Slot{N: n, Tier: slots.Cold, Phase: slots.Evicting}, true
	default: // terminated
		return slots.Slot{}, false
	}
	return s, true
}

// Observed builds the full view: every live slot instance, plus COLD records
// for slots 1..maxDTNs without one. A slot with two live instances (shouldn't
// happen) is reported by its lowest instance ID only.
func Observed(insts []ec2types.Instance, probes map[string]Probe, control ControlState, maxDTNs int, now time.Time) slots.Observed {
	seen := map[int]bool{}
	obs := slots.Observed{ControlServing: control.Serving, ControlRunning: control.Running}
	for _, inst := range insts {
		id := ""
		if inst.InstanceId != nil {
			id = *inst.InstanceId
		}
		s, ok := SlotFromInstance(inst, probes[id], now)
		if !ok || seen[s.N] {
			continue
		}
		seen[s.N] = true
		obs.Slots = append(obs.Slots, s)
	}
	for n := 1; n <= maxDTNs; n++ {
		if !seen[n] {
			obs.Slots = append(obs.Slots, slots.Slot{N: n, Tier: slots.Cold})
		}
	}
	return obs
}

func tagMap(tags []ec2types.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range tags {
		if t.Key != nil && t.Value != nil {
			m[*t.Key] = *t.Value
		}
	}
	return m
}

func since(rfc3339 string, now time.Time) time.Duration {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil || t.After(now) {
		return 0
	}
	return now.Sub(t)
}
