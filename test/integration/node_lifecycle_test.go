//go:build integration

package integration

// TestNodeLifecycle validates the full warm-pool DTN lifecycle:
//
//  1. Assert warm pool has at least one Warmed:Stopped instance
//  2. Scale out (desired=1), measure time to InService
//  3. Assert GCS node is active via `node list -F json` on the control node
//  4. Submit a small transfer, assert SUCCEEDED
//  5. Scale in (desired=0), measure time for instance to return to Warmed:Stopped
//  6. Assert GCS node is no longer active
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

	// ── 2. Scale out → wait for InService ────────────────────────────────────
	t.Log("step 2: triggering scale-out (desired=1)...")
	t0 := time.Now()
	e.scaleASG(ctx, t, 1)
	dtnIDs := e.waitForASGInService(ctx, t, 1, 3*time.Minute)
	t1 := time.Now()
	t.Logf("  DTN InService: %v  elapsed: %s", dtnIDs, t1.Sub(t0).Round(time.Second))
	if elapsed := t1.Sub(t0); elapsed > 3*time.Minute {
		t.Errorf("scale-out too slow: %s (expected < 3m)", elapsed)
	}

	// ── 3. Assert GCS node is active ─────────────────────────────────────────
	t.Log("step 3: checking GCS node list...")
	// GCS node enable and DNS propagation can take ~30s after InService.
	var gcsReady bool
	for i := 0; i < 12; i++ {
		time.Sleep(10 * time.Second)
		nodes := e.listGCSNodes(ctx, t)
		if hasActiveGCSNode(nodes) {
			gcsReady = true
			t.Logf("  GCS node active after %s", time.Since(t1).Round(time.Second))
			break
		}
		t.Logf("  waiting for GCS node to become active... (%d/12)", i+1)
	}
	if !gcsReady {
		t.Error("GCS node did not become active within 2 minutes of InService")
	}

	// ── 4. Submit test transfer ───────────────────────────────────────────────
	t.Log("step 4: submitting test transfer...")
	t2 := time.Now()
	taskID := e.submitTestTransfer(ctx, t)
	task := e.waitForTransferSucceeded(ctx, t, taskID, 10*time.Minute)
	t3 := time.Now()
	t.Logf("  transfer SUCCEEDED  bytes=%d  elapsed=%s",
		task.BytesTransferred, t3.Sub(t2).Round(time.Second))

	// ── 5. Scale in → wait for Warmed:Stopped ────────────────────────────────
	t.Log("step 5: triggering scale-in (desired=0)...")
	t4 := time.Now()
	e.scaleASG(ctx, t, 0)
	// Lifecycle hook runs `node disable` before stopping — allow up to 5 min.
	e.waitForWarmPool(ctx, t, len(warmIDs), 5*time.Minute)
	t5 := time.Now()
	t.Logf("  warm pool restored  elapsed=%s", t5.Sub(t4).Round(time.Second))

	// Suppress cleanup re-scaling since we already scaled to 0.
	// The t.Cleanup above is still correct — it's a no-op if desired is already 0.

	// ── 6. Assert GCS node is no longer active ────────────────────────────────
	t.Log("step 6: verifying GCS node is inactive...")
	// Give lifecycle hook time to complete node disable + DNS propagation.
	time.Sleep(15 * time.Second)
	nodes := e.listGCSNodes(ctx, t)
	if hasActiveGCSNode(nodes) {
		t.Error("expected no active GCS nodes after scale-in, but found at least one")
	} else {
		t.Log("  confirmed: no active GCS nodes")
	}

	// ── Summary ───────────────────────────────────────────────────────────────
	t.Logf("\n=== lifecycle timing ===")
	t.Logf("  scale-out → InService:   %s", t1.Sub(t0).Round(time.Second))
	t.Logf("  transfer duration:       %s", t3.Sub(t2).Round(time.Second))
	t.Logf("  scale-in → Warmed:Stop:  %s", t5.Sub(t4).Round(time.Second))
}
