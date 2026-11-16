package slotctl

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/slots"
)

var now = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func inst(id, slot, state string, tags ...string) ec2types.Instance {
	ts := []ec2types.Tag{{Key: aws.String(names.TagSlot), Value: aws.String(slot)}}
	for i := 0; i+1 < len(tags); i += 2 {
		ts = append(ts, ec2types.Tag{Key: aws.String(tags[i]), Value: aws.String(tags[i+1])})
	}
	return ec2types.Instance{InstanceId: aws.String(id), Tags: ts,
		State: &ec2types.InstanceState{Name: ec2types.InstanceStateName(state)}}
}

func ago(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

func TestSlotFromInstance(t *testing.T) {
	healthy := Probe{SSMOnline: true, InDNS: true, GCSActive: true, DataConns: 4}
	cases := []struct {
		name string
		in   ec2types.Instance
		p    Probe
		want slots.Slot
		ok   bool
	}{
		{"healthy hot", inst("i-1", "1", "running"), healthy, slots.Slot{N: 1, Tier: slots.Hot, Healthy: true, DataConns: 4}, true},
		{"healthy carries how long", inst("i-1", "1", "running", TagHealthySince, ago(3*time.Minute)), healthy,
			slots.Slot{N: 1, Tier: slots.Hot, Healthy: true, HealthyFor: 3 * time.Minute, DataConns: 4}, true},
		{"starting becomes stable once healthy", inst("i-1", "1", "running", TagPhase, "starting"), healthy, slots.Slot{N: 1, Tier: slots.Hot, Healthy: true, DataConns: 4}, true},
		{"unhealthy carries its duration and restarts", inst("i-1", "1", "running", TagUnhealthySince, ago(6*time.Minute), TagRestarts, "1"), Probe{SSMOnline: true},
			slots.Slot{N: 1, Tier: slots.Hot, UnhealthyFor: 6 * time.Minute, RestartAttempts: 1}, true},
		{"draining reports its drain time", inst("i-1", "2", "running", TagPhase, "draining", TagPhaseSince, ago(3*time.Minute)), healthy,
			slots.Slot{N: 2, Tier: slots.Hot, Phase: slots.Draining, Healthy: true, DataConns: 4, DrainingFor: 3 * time.Minute}, true},
		{"stopped is warm with stopped-for", inst("i-1", "3", "stopped", TagStoppedAt, ago(80*time.Hour)), Probe{},
			slots.Slot{N: 3, Tier: slots.Warm, StoppedFor: 80 * time.Hour}, true},
		{"a finished stop clears the phase", inst("i-1", "3", "stopped", TagPhase, "stopping"), Probe{}, slots.Slot{N: 3, Tier: slots.Warm}, true},
		{"stopping is warm, in flight", inst("i-1", "3", "stopping"), Probe{}, slots.Slot{N: 3, Tier: slots.Warm, Phase: slots.Stopping}, true},
		{"in DNS but GCS not running is unhealthy", inst("i-1", "1", "running"), Probe{SSMOnline: true, InDNS: true},
			slots.Slot{N: 1, Tier: slots.Hot}, true},
		{"terminated is ignored", inst("i-1", "4", "terminated"), Probe{}, slots.Slot{}, false},
		{"no slot tag is ignored", ec2types.Instance{InstanceId: aws.String("i-9"), State: &ec2types.InstanceState{Name: "running"}}, Probe{}, slots.Slot{}, false},
		{"future timestamps count as zero", inst("i-1", "1", "running", TagUnhealthySince, now.Add(time.Hour).Format(time.RFC3339)), Probe{},
			slots.Slot{N: 1, Tier: slots.Hot}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := SlotFromInstance(c.in, c.p, now)
			if ok != c.ok || got != c.want {
				t.Fatalf("got %+v ok=%v, want %+v ok=%v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestObservedAddsColdSlots(t *testing.T) {
	obs := Observed([]ec2types.Instance{inst("i-2", "2", "stopped"), inst("i-dup", "2", "running")}, nil, ControlState{Running: true, Serving: true}, 3, now)
	if len(obs.Slots) != 3 || !obs.ControlServing || !obs.ControlRunning {
		t.Fatalf("%+v", obs)
	}
	tiers := map[int]slots.Tier{}
	for _, s := range obs.Slots {
		tiers[s.N] = s.Tier
	}
	if tiers[1] != slots.Cold || tiers[2] != slots.Warm || tiers[3] != slots.Cold {
		t.Fatalf("tiers %v", tiers)
	}
}

func TestParseNodeProbe(t *testing.T) {
	for in, want := range map[string]Probe{
		"12\nup\n":  {DataConns: 12, GCSActive: true},
		"0\ndown\n": {},
		"3\n":       {DataConns: 3},
		"":          {},
	} {
		if got := parseNodeProbe(in); got != want {
			t.Errorf("parseNodeProbe(%q) = %+v, want %+v", in, got, want)
		}
	}
}
