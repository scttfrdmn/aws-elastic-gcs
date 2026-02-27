//go:build integration

package integration

// TestNodeLifecycle validates the full warm-pool DTN lifecycle:
//
//  1. Assert warm pool has at least one Warmed:Stopped instance
//  2. Record baseline active GCS node count (control node only = 1)
//  3. Scale out (desired=1), measure time to InService
//  4. Assert active GCS node count increased (DTN node enabled)
//  5. Submit a small transfer, assert SUCCEEDED
//  6. Scale in (desired=0), measure time for instance to return to Warmed:Stopped
//  7. Assert active GCS node count returned to baseline
//
// Prerequisites:
//   - Deployed us-east-1 stack with warm pool
//   - GLOBUS_CLIENT_ID and GLOBUS_CLIENT_SECRET set
//
// Run: make test-integration REGION=us-east-1 -run TestNodeLifecycle
import (
	"context"
	"testing"
	"time"
)

func TestNodeLifecycle(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)

	// ── 1. Verify warm pool has at least one Warmed:Stopped instance ──────────
	t.Log("step 1: checking warm pool...")
	warmIDs := e.waitForWarmPool(ctx, t, 1, 30*time.Second)
	t.Logf("  warm pool has %d Warmed:Stopped instance(s): %v", len(warmIDs), warmIDs)

	// Restore ASG to desired=0 after the test, regardless of outcome.
	t.Cleanup(func() {
		cleanCtx := context.Background()
		t.Log("cleanup: scaling ASG back to 0...")
		e.scaleASG(cleanCtx, t, 0)
		// Wait for the instance to return to the warm pool (up to 5 min for lifecycle hook).
		e.waitForWarmPool(cleanCtx, t, len(warmIDs), 5*time.Minute)
		t.Log("cleanup: warm pool restored")
	})

	// ── 2. Baseline: count active GCS nodes before scale-out ─────────────────
	// The control node is always active; baseline is typically 1.
	// We compare against this baseline rather than expecting 0 after scale-in.
	baselineNodes := e.listGCSNodes(ctx, t)
	baselineActive := activeNodeCount(baselineNodes)
	t.Logf("  baseline active GCS nodes: %d", baselineActive)

	// ── 3. Scale out → wait for InService ────────────────────────────────────
	t.Log("step 3: triggering scale-out (desired=1)...")
	t0 := time.Now()
	e.scaleASG(ctx, t, 1)
	dtnIDs := e.waitForASGInService(ctx, t, 1, 3*time.Minute)
	t1 := time.Now()
	t.Logf("  DTN InService: %v  elapsed: %s", dtnIDs, t1.Sub(t0).Round(time.Second))
	if elapsed := t1.Sub(t0); elapsed > 3*time.Minute {
		t.Errorf("scale-out too slow: %s (expected < 3m)", elapsed)
	}

	// ── 4. Assert DTN GCS node became active ──────────────────────────────────
	t.Log("step 4: checking GCS node list for new active node...")
	var gcsReady bool
	for i := 0; i < 12; i++ {
		time.Sleep(10 * time.Second)
		nodes := e.listGCSNodes(ctx, t)
		if activeNodeCount(nodes) > baselineActive {
			gcsReady = true
			t.Logf("  DTN GCS node active after %s (total active: %d)",
				time.Since(t1).Round(time.Second), activeNodeCount(nodes))
			break
		}
		t.Logf("  waiting for DTN node to activate... (%d/12)", i+1)
	}
	if !gcsReady {
		t.Error("DTN GCS node did not become active within 2 minutes of InService")
	}

	// ── 5. Submit test transfer ───────────────────────────────────────────────
	t.Log("step 5: submitting test transfer...")
	t2 := time.Now()
	taskID := e.submitTestTransfer(ctx, t)
	task := e.waitForTransferSucceeded(ctx, t, taskID, 10*time.Minute)
	t3 := time.Now()
	t.Logf("  transfer SUCCEEDED  bytes=%d  elapsed=%s",
		task.BytesTransferred, t3.Sub(t2).Round(time.Second))

	// ── 6. Scale in → wait for Warmed:Stopped ────────────────────────────────
	t.Log("step 6: triggering scale-in (desired=0)...")
	t4 := time.Now()
	e.scaleASG(ctx, t, 0)
	// Lifecycle hook runs `node disable` before stopping — allow up to 5 min.
	e.waitForWarmPool(ctx, t, len(warmIDs), 5*time.Minute)
	t5 := time.Now()
	t.Logf("  warm pool restored  elapsed=%s", t5.Sub(t4).Round(time.Second))

	// ── 7. Assert active node count returned to baseline ──────────────────────
	t.Log("step 7: verifying DTN GCS node is inactive...")
	// Allow up to 30s for node disable + DNS propagation to reflect in node list.
	var backToBaseline bool
	for i := 0; i < 6; i++ {
		time.Sleep(10 * time.Second)
		nodes := e.listGCSNodes(ctx, t)
		if activeNodeCount(nodes) <= baselineActive {
			backToBaseline = true
			t.Logf("  confirmed: active nodes back to baseline (%d)", baselineActive)
			break
		}
		t.Logf("  waiting for DTN node to go inactive... (%d/6, active=%d)", i+1, activeNodeCount(nodes))
	}
	if !backToBaseline {
		t.Errorf("expected active GCS node count to return to %d after scale-in", baselineActive)
	}

	// ── Summary ───────────────────────────────────────────────────────────────
	t.Logf("\n=== lifecycle timing ===")
	t.Logf("  scale-out → InService:   %s", t1.Sub(t0).Round(time.Second))
	t.Logf("  transfer duration:       %s", t3.Sub(t2).Round(time.Second))
	t.Logf("  scale-in → Warmed:Stop:  %s", t5.Sub(t4).Round(time.Second))
}

// activeNodeCount returns the number of nodes with status "active".
func activeNodeCount(nodes []gcsNodeStatus) int {
	n := 0
	for _, node := range nodes {
		if node.Status == "active" {
			n++
		}
	}
	return n
}
