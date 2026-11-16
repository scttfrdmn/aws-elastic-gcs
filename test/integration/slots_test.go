//go:build integration

package integration

// TestSlotScaleOutIn drives the slot controller (#65) through one scale-out and
// one scale-in by setting desired-dtns directly:
//
//  1. desired-dtns=0 and no slot running (baseline active GCS node count)
//  2. desired-dtns=1 → a slot starts (WARM) or revives (COLD), turns healthy,
//     and the endpoint gains an active GCS node
//  3. a transfer succeeds
//  4. desired-dtns=0 → the controller drains the slot (node disable) and stops
//     it; the endpoint is back to its baseline active node count
//
// The control node must be serving: the controller never stops the last DTN
// otherwise (it starts the control node first).
//
// Prerequisites: GLOBUS_CLIENT_ID and GLOBUS_CLIENT_SECRET set.
//
// Run: make test-integration REGION=us-west-2 -run TestSlotScaleOutIn
import (
	"context"
	"testing"
	"time"
)

func TestSlotScaleOutIn(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	t.Cleanup(func() { e.setDesired(context.Background(), t, 0) })

	t.Log("step 1: baseline (desired-dtns=0, no running slots)")
	e.setDesired(ctx, t, 0)
	e.waitForNoRunningSlots(ctx, t, 15*time.Minute)
	baseline := activeNodeCount(e.listGCSNodes(ctx, t))
	t.Logf("baseline active GCS nodes: %d", baseline)

	t.Log("step 2: desired-dtns=1")
	start := time.Now()
	e.setDesired(ctx, t, 1)
	ids := e.waitForServingSlots(ctx, t, 1, 10*time.Minute)
	t.Logf("slot serving after %s: %v", time.Since(start).Round(time.Second), ids)
	deadline := time.Now().Add(3 * time.Minute)
	for activeNodeCount(e.listGCSNodes(ctx, t)) <= baseline {
		if time.Now().After(deadline) {
			t.Fatalf("no new active GCS node within 3m of the slot serving")
		}
		time.Sleep(10 * time.Second)
	}
	t.Logf("endpoint gained an active node after %s", time.Since(start).Round(time.Second))

	t.Log("step 3: transfer")
	taskID := e.submitTestTransfer(ctx, t)
	info := e.waitForTransferSucceeded(ctx, t, taskID, 5*time.Minute)
	if info.BytesTransferred <= 0 {
		t.Errorf("expected bytes_transferred > 0, got %d", info.BytesTransferred)
	}

	t.Log("step 4: desired-dtns=0 (drain, then stop)")
	start = time.Now()
	e.setDesired(ctx, t, 0)
	e.waitForNoRunningSlots(ctx, t, 15*time.Minute)
	t.Logf("slot stopped after %s", time.Since(start).Round(time.Second))
	deadline = time.Now().Add(3 * time.Minute)
	for activeNodeCount(e.listGCSNodes(ctx, t)) > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("active GCS nodes still above baseline %d 3m after the slot stopped", baseline)
		}
		time.Sleep(10 * time.Second)
	}
}
