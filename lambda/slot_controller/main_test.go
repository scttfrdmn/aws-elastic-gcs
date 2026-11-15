package main

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-durable-execution-sdk-go/durable/durabletest"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/slotctl"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/slots"
)

// fake simulates a deployment: Execute changes the observed tiers directly.
type fake struct {
	locked  bool
	desired int
	state   map[int]slots.Tier
	execs   []string
	failOn  slots.Kind
}

func (f *fake) Acquire(context.Context, string, time.Duration) error {
	if f.locked {
		return slotctl.ErrLocked
	}
	return nil
}
func (f *fake) Release(context.Context, string) {}
func (f *fake) Desired(context.Context) int     { return f.desired }
func (f *fake) Gather(context.Context) (slots.Observed, error) {
	obs := slots.Observed{ControlServing: true}
	for n := 1; n <= 3; n++ {
		t := f.state[n]
		if t == "" {
			t = slots.Cold
		}
		obs.Slots = append(obs.Slots, slots.Slot{N: n, Tier: t, Healthy: t == slots.Hot})
	}
	return obs, nil
}
func (f *fake) Execute(_ context.Context, a slots.Action) error {
	f.execs = append(f.execs, a.String())
	if a.Kind == f.failOn {
		return context.DeadlineExceeded
	}
	switch a.Kind {
	case slots.Start, slots.Revive:
		f.state[a.Slot] = slots.Hot
	case slots.BeginDrain, slots.Stop:
		f.state[a.Slot] = slots.Warm // drain completes instantly in the fake
	}
	return nil
}

var cfg = slots.Config{MaxDTNs: 3, WarmSlots: 2, DrainTimeout: time.Minute, ColdTTL: time.Hour, UnhealthyAfter: time.Minute, MaxRestarts: 2}

func run(t *testing.T, f *fake) Result {
	t.Helper()
	r, err := durabletest.NewLocalRunner(newHandler(f, cfg)).RunUntilComplete(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != durabletest.Succeeded {
		t.Fatalf("status %v", r.Status)
	}
	res, err := durabletest.ResultAs[Result](r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestConvergesScaleOut(t *testing.T) {
	f := &fake{desired: 2, state: map[int]slots.Tier{1: slots.Warm, 2: slots.Cold}}
	res := run(t, f)
	if f.state[1] != slots.Hot || f.state[2] != slots.Hot || len(res.Errors) != 0 {
		t.Fatalf("state %v res %+v", f.state, res)
	}
}

func TestSkipsWhenLocked(t *testing.T) {
	f := &fake{locked: true, desired: 1, state: map[int]slots.Tier{}}
	if res := run(t, f); !res.Skipped || len(f.execs) != 0 {
		t.Fatalf("res %+v execs %v", res, f.execs)
	}
}

func TestActionErrorsAreReportedNotFatal(t *testing.T) {
	f := &fake{desired: 1, state: map[int]slots.Tier{}, failOn: slots.Revive}
	res := run(t, f)
	if len(res.Errors) == 0 || res.Rounds != maxRounds {
		t.Fatalf("res %+v", res)
	}
}
