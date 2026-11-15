// Package slots is the slot controller's decision core (#65, #125): given the
// observed slots and a desired DTN count, Plan returns the next actions. It is
// pure (no AWS calls) so every rule is table-tested; the controller Lambda
// executes the actions and calls Plan again until nothing is left to do.
//
// A slot is a persistent pairing of a GCS node identity (node-slot-N), one EC2
// instance and one Elastic IP. Its tier is where it rests:
//
//	HOT   running, node enabled, serving
//	WARM  stopped, EIP kept: resume is a local `node enable`
//	COLD  no instance and no EIP: reviving runs `node setup` again
//
// Rules, in priority order:
//  1. Finish what's in flight: a draining slot stops once it has no data
//     connections or its drain timeout has passed.
//  2. Health: a HOT slot unhealthy for too long is restarted, and rebuilt
//     (COLD, then revived) after too many restarts.
//  3. Scale out: start WARM slots (lowest number first, in parallel), then
//     revive COLD slots one at a time (node setup races on the endpoint
//     keychain when concurrent).
//  4. Scale in: one slot at a time, the one with the fewest data connections
//     (ties: highest number), never the last DTN unless the control node is
//     serving.
//  5. Eviction: beyond the first WarmSlots warm slots, a WARM slot idle for
//     ColdTTL goes COLD.
package slots

import (
	"fmt"
	"sort"
	"time"
)

// Tier is where a slot rests.
type Tier string

const (
	Hot  Tier = "hot"
	Warm Tier = "warm"
	Cold Tier = "cold"
)

// Phase is a slot's transition in progress, if any.
type Phase string

const (
	Stable   Phase = ""
	Starting Phase = "starting" // WARM -> HOT
	Reviving Phase = "reviving" // COLD -> HOT (node setup)
	Draining Phase = "draining" // node disabled, waiting for data connections
	Stopping Phase = "stopping" // HOT -> WARM
	Evicting Phase = "evicting" // WARM -> COLD
)

// Slot is the observed state of one slot.
type Slot struct {
	N     int
	Tier  Tier
	Phase Phase

	// HOT slots only.
	Healthy         bool          // running, active in GCS, SSM online
	UnhealthyFor    time.Duration // how long it has been unhealthy
	RestartAttempts int           // restarts since it was last healthy
	DataConns       int           // active GridFTP data connections

	DrainingFor time.Duration // Draining only
	StoppedFor  time.Duration // WARM only: time since it stopped
}

// Observed is everything Plan needs to know.
type Observed struct {
	Slots []Slot
	// ControlServing reports whether the control node is running with its GCS
	// node enabled, i.e. the endpoint stays reachable without any DTN.
	ControlServing bool
}

// Config holds the reconciler's settings (from config.Resolve).
type Config struct {
	MaxDTNs        int
	WarmSlots      int           // WARM slots kept when idle; the rest go COLD
	DrainTimeout   time.Duration // max wait for data connections before stopping
	ColdTTL        time.Duration // idle time before an extra WARM slot goes COLD
	UnhealthyAfter time.Duration // unhealthy this long -> restart
	MaxRestarts    int           // restarts before a rebuild
}

// Kind of action.
type Kind string

const (
	Start         Kind = "start"          // WARM -> HOT
	Revive        Kind = "revive"         // COLD -> HOT
	BeginDrain    Kind = "begin-drain"    // disable node, start draining
	Stop          Kind = "stop"           // drained -> WARM
	Restart       Kind = "restart"        // stop + start an unhealthy slot
	Rebuild       Kind = "rebuild"        // terminate (COLD) + revive
	Evict         Kind = "evict"          // WARM -> COLD
	EnsureControl Kind = "ensure-control" // make the control node serve first
	CreateSlot    Kind = "create-slot"    // slot N doesn't exist yet: COLD record
)

// Action is one step for the controller to execute.
type Action struct {
	Kind   Kind
	Slot   int // 0 for EnsureControl
	Reason string
}

func (a Action) String() string { return fmt.Sprintf("%s slot %d: %s", a.Kind, a.Slot, a.Reason) }

// Plan returns the actions that move observed toward desired HOT slots.
func Plan(obs Observed, desired int, cfg Config) []Action {
	if desired < 0 {
		desired = 0
	}
	if desired > cfg.MaxDTNs {
		desired = cfg.MaxDTNs
	}
	slots := byNumber(obs.Slots)
	var acts []Action

	// Slots that should exist but don't: every slot 1..MaxDTNs has a record.
	have := map[int]bool{}
	for _, s := range slots {
		have[s.N] = true
	}
	for n := 1; n <= cfg.MaxDTNs; n++ {
		if !have[n] {
			acts = append(acts, Action{CreateSlot, n, "missing slot record"})
			slots = append(slots, Slot{N: n, Tier: Cold})
		}
	}
	slots = byNumber(slots)

	var serving, reviving, scalingIn int
	for _, s := range slots {
		switch {
		case s.Tier == Hot && (s.Phase == Stable || s.Phase == Starting):
			serving++
		case s.Phase == Reviving:
			serving++ // counts toward desired: it's on its way
			reviving++
		case s.Phase == Draining || s.Phase == Stopping:
			scalingIn++
		}
	}

	// 1. In flight.
	for _, s := range slots {
		if s.Phase == Draining && (s.DataConns == 0 || s.DrainingFor >= cfg.DrainTimeout) {
			why := "drained"
			if s.DataConns > 0 {
				why = fmt.Sprintf("drain timeout with %d data connections", s.DataConns)
			}
			acts = append(acts, Action{Stop, s.N, why})
		}
	}

	// 2. Health.
	for _, s := range slots {
		if s.Tier != Hot || s.Phase != Stable || s.Healthy || s.UnhealthyFor < cfg.UnhealthyAfter {
			continue
		}
		if s.RestartAttempts < cfg.MaxRestarts {
			acts = append(acts, Action{Restart, s.N, fmt.Sprintf("unhealthy for %s", s.UnhealthyFor.Round(time.Second))})
		} else {
			acts = append(acts, Action{Rebuild, s.N, fmt.Sprintf("still unhealthy after %d restarts", s.RestartAttempts)})
		}
	}

	// 3. Scale out.
	if need := desired - serving; need > 0 {
		for _, s := range slots {
			if need == 0 {
				break
			}
			if s.Tier == Warm && s.Phase == Stable {
				acts = append(acts, Action{Start, s.N, "scale out"})
				need--
			}
		}
		if need > 0 && reviving == 0 {
			for _, s := range slots {
				if s.Tier == Cold && s.Phase == Stable {
					acts = append(acts, Action{Revive, s.N, "scale out (no warm slot left)"})
					break // one node setup at a time
				}
			}
		}
	}

	// 4. Scale in: one at a time.
	if excess := serving - desired; excess > 0 && scalingIn == 0 {
		if serving == 1 && !obs.ControlServing {
			acts = append(acts, Action{EnsureControl, 0, "last DTN: the control node must serve first"})
		} else if c, ok := scaleInCandidate(slots); ok {
			acts = append(acts, Action{BeginDrain, c.N, fmt.Sprintf("scale in (%d data connections)", c.DataConns)})
		}
	}

	// 5. Eviction: keep the first WarmSlots warm slots.
	keptWarm := 0
	for _, s := range slots {
		if s.Tier != Warm || s.Phase != Stable || startedNow(acts, s.N) {
			continue
		}
		if keptWarm < cfg.WarmSlots {
			keptWarm++
			continue
		}
		if s.StoppedFor >= cfg.ColdTTL {
			acts = append(acts, Action{Evict, s.N, fmt.Sprintf("warm for %s beyond warm_slots=%d", s.StoppedFor.Round(time.Hour), cfg.WarmSlots)})
		}
	}
	return acts
}

// scaleInCandidate picks the HOT, stable, healthy slot with the fewest data
// connections; ties go to the highest slot number.
func scaleInCandidate(slots []Slot) (Slot, bool) {
	var best Slot
	found := false
	for _, s := range slots {
		if s.Tier != Hot || s.Phase != Stable {
			continue
		}
		if !found || s.DataConns < best.DataConns || (s.DataConns == best.DataConns && s.N > best.N) {
			best, found = s, true
		}
	}
	return best, found
}

func startedNow(acts []Action, n int) bool {
	for _, a := range acts {
		if a.Slot == n && (a.Kind == Start || a.Kind == Revive) {
			return true
		}
	}
	return false
}

func byNumber(in []Slot) []Slot {
	out := append([]Slot(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out
}
