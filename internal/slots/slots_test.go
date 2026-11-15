package slots

import (
	"reflect"
	"testing"
	"time"
)

var cfg = Config{MaxDTNs: 4, WarmSlots: 2, DrainTimeout: 10 * time.Minute, ColdTTL: 72 * time.Hour,
	UnhealthyAfter: 5 * time.Minute, MaxRestarts: 2}

func hot(n, conns int) Slot { return Slot{N: n, Tier: Hot, Healthy: true, DataConns: conns} }
func warm(n int) Slot       { return Slot{N: n, Tier: Warm} }
func cold(n int) Slot       { return Slot{N: n, Tier: Cold} }
func kinds(acts []Action) []string {
	out := []string{}
	for _, a := range acts {
		out = append(out, string(a.Kind)+":"+itoa(a.Slot))
	}
	return out
}
func itoa(n int) string { return string(rune('0' + n)) }

func TestPlan(t *testing.T) {
	cases := []struct {
		name    string
		obs     Observed
		desired int
		want    []string
	}{
		{"steady state does nothing",
			Observed{Slots: []Slot{hot(1, 3), warm(2), warm(3), cold(4)}, ControlServing: true}, 1, []string{}},
		{"scale out starts warm slots lowest first, in parallel",
			Observed{Slots: []Slot{warm(1), warm(2), warm(3), cold(4)}}, 2, []string{"start:1", "start:2"}},
		{"then revives one cold slot at a time",
			Observed{Slots: []Slot{hot(1, 0), warm(2), cold(3), cold(4)}}, 4, []string{"start:2", "revive:3"}},
		{"no second revive while one is in flight",
			Observed{Slots: []Slot{hot(1, 0), hot(2, 0), {N: 3, Tier: Cold, Phase: Reviving}, cold(4)}}, 4, []string{}},
		{"desired is capped at max_dtns",
			Observed{Slots: []Slot{warm(1), warm(2), warm(3), warm(4)}}, 9, []string{"start:1", "start:2", "start:3", "start:4"}},
		{"scale in drains the slot with fewest data connections",
			Observed{Slots: []Slot{hot(1, 5), hot(2, 0), hot(3, 2), cold(4)}, ControlServing: true}, 1, []string{"begin-drain:2"}},
		{"ties drain the highest slot number",
			Observed{Slots: []Slot{hot(1, 0), hot(2, 0), warm(3), cold(4)}, ControlServing: true}, 1, []string{"begin-drain:2"}},
		{"one scale-in at a time",
			Observed{Slots: []Slot{hot(1, 0), {N: 2, Tier: Hot, Phase: Draining, DataConns: 4, DrainingFor: time.Minute}, hot(3, 0), cold(4)}, ControlServing: true}, 0, []string{}},
		{"a drained slot stops",
			Observed{Slots: []Slot{hot(1, 0), {N: 2, Tier: Hot, Phase: Draining}, warm(3), cold(4)}, ControlServing: true}, 1, []string{"stop:2"}},
		{"drain timeout stops even with connections",
			Observed{Slots: []Slot{{N: 1, Tier: Hot, Phase: Draining, DataConns: 3, DrainingFor: 11 * time.Minute}, warm(2), warm(3), cold(4)}, ControlServing: true}, 0, []string{"stop:1"}},
		{"never the last DTN unless the control node serves",
			Observed{Slots: []Slot{hot(1, 0), warm(2), warm(3), cold(4)}, ControlServing: false}, 0, []string{"ensure-control:0"}},
		{"the last DTN goes once the control node serves",
			Observed{Slots: []Slot{hot(1, 0), warm(2), warm(3), cold(4)}, ControlServing: true}, 0, []string{"begin-drain:1"}},
		{"unhealthy slot is restarted",
			Observed{Slots: []Slot{{N: 1, Tier: Hot, UnhealthyFor: 6 * time.Minute}, warm(2), warm(3), cold(4)}, ControlServing: true}, 1, []string{"restart:1"}},
		{"briefly unhealthy slot is left alone",
			Observed{Slots: []Slot{{N: 1, Tier: Hot, UnhealthyFor: time.Minute}, warm(2), warm(3), cold(4)}, ControlServing: true}, 1, []string{}},
		{"a start that never turns healthy is restarted",
			Observed{Slots: []Slot{{N: 1, Tier: Hot, Phase: Starting, UnhealthyFor: 6 * time.Minute}, warm(2), warm(3), cold(4)}, ControlServing: true}, 1, []string{"restart:1"}},
		{"an abandoned revive is rebuilt, not restarted",
			Observed{Slots: []Slot{{N: 1, Tier: Hot, Phase: Reviving, UnhealthyFor: 6 * time.Minute}, warm(2), warm(3), cold(4)}, ControlServing: true}, 1, []string{"rebuild:1"}},
		{"a revive still settling (DNS) is left alone",
			Observed{Slots: []Slot{{N: 1, Tier: Hot, Phase: Reviving, UnhealthyFor: time.Minute}, warm(2), warm(3), cold(4)}, ControlServing: true}, 1, []string{}},
		{"rebuild after too many restarts",
			Observed{Slots: []Slot{{N: 1, Tier: Hot, UnhealthyFor: 6 * time.Minute, RestartAttempts: 2}, warm(2), warm(3), cold(4)}, ControlServing: true}, 1, []string{"rebuild:1"}},
		{"extra warm slots past cold_ttl are evicted; the first warm_slots are kept",
			Observed{Slots: []Slot{{N: 1, Tier: Warm, StoppedFor: 100 * time.Hour}, {N: 2, Tier: Warm, StoppedFor: 100 * time.Hour}, {N: 3, Tier: Warm, StoppedFor: 100 * time.Hour}, {N: 4, Tier: Warm, StoppedFor: time.Hour}}}, 0, []string{"evict:3"}},
		{"warm_slots counts after this round's starts",
			Observed{Slots: []Slot{{N: 1, Tier: Warm, StoppedFor: 100 * time.Hour}, {N: 2, Tier: Warm, StoppedFor: 100 * time.Hour}, {N: 3, Tier: Warm, StoppedFor: 100 * time.Hour}, cold(4)}}, 1, []string{"start:1"}},
		{"missing slot records are created cold",
			Observed{Slots: []Slot{hot(1, 0)}, ControlServing: true}, 1, []string{"create-slot:2", "create-slot:3", "create-slot:4"}},
		{"scale from zero with no slots revives one",
			Observed{}, 2, []string{"create-slot:1", "create-slot:2", "create-slot:3", "create-slot:4", "revive:1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := kinds(Plan(c.obs, c.desired, cfg))
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestMinimalProfileEvictsAllAfterTTL(t *testing.T) {
	c := cfg
	c.WarmSlots = 0
	obs := Observed{Slots: []Slot{{N: 1, Tier: Warm, StoppedFor: 73 * time.Hour}, {N: 2, Tier: Warm, StoppedFor: 73 * time.Hour}, cold(3), cold(4)}}
	if got := kinds(Plan(obs, 0, c)); !reflect.DeepEqual(got, []string{"evict:1", "evict:2"}) {
		t.Fatalf("got %v", got)
	}
}
