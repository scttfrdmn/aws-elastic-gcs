//go:build integration

package integration

// TestAutoScaleOut and TestAutoScaleFull exercise the automatic scale-out path:
//
//  demand_monitor Lambda (60s EventBridge) → publishes ElasticGCS/ActiveTransfers
//  → CloudWatch scale-out alarm (Period=60s, EvalPeriods=1) → scaleout Lambda
//  → ASG DesiredCapacity increases without any manual call
//
// TestAutoScaleOut  — single round, ~5 min
// TestAutoScaleFull — two rounds proving repeatability, ~12 min; opt-in via
//
//	ELASTIC_GCS_TEST_FULL_CYCLE=true
import (
	"context"
	"os"
	"testing"
	"time"
)

// TestAutoScaleOut exercises the full automatic scale-out path driven by Globus
// transfer demand. Scale-out is triggered by the demand_monitor Lambda publishing
// ElasticGCS/ActiveTransfers > 0, not by any manual ASG call.
//
// Timing assumptions:
//   - demand_monitor fires every 60 s (EventBridge schedule)
//   - Scale-out alarm: Period=60s, EvalPeriods=1 → fires within 1 cycle
//   - keepAliveSubmit(75s, 10s) ensures an ACTIVE task exists in any 60s window
//   - Warm-path resume: ~60s to InService
//
// Expected runtime: ~5 minutes.
func TestAutoScaleOut(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)

	// Safety net: always restore to desired=0 on exit, even on failure.
	t.Cleanup(func() {
		e.scaleASG(context.Background(), t, 0)
	})

	// Precondition: start from desired=0 with at least one warm instance.
	t.Log("precondition: scaling to desired=0...")
	e.scaleASG(ctx, t, 0)
	t.Log("waiting for warm pool (at least 1 Warmed:Stopped)...")
	warmIDs := e.waitForWarmPool(ctx, t, 1, 3*time.Minute)
	t.Logf("warm pool ready: %v", warmIDs)

	// Step 1: keep active transfers in-flight for 75s so that demand_monitor
	// sees ActiveTransfers > 0 in at least one 60s window.
	t.Log("step 1: submitting keepalive transfers for 75s (interval 10s)...")
	taskIDs := e.keepAliveSubmit(ctx, t, 75*time.Second, 10*time.Second)
	t.Logf("step 1 complete: submitted %d tasks: %v", len(taskIDs), taskIDs)

	// Step 2: wait for scaleout Lambda to increase ASG desired (no manual call).
	t.Log("step 2: waiting for ASG desired to change from 0 (automatic scale-out)...")
	newDesired := e.waitForASGDesiredChange(ctx, t, 0, 4*time.Minute)
	t.Logf("step 2 complete: ASG desired 0 → %d (scaleout Lambda fired)", newDesired)

	// Step 3: wait for DTN to transition from Warmed:Stopped to InService (~60s).
	t.Log("step 3: waiting for DTN InService...")
	ids := e.waitForASGInService(ctx, t, 1, 5*time.Minute)
	t.Logf("step 3 complete: DTN InService: %v", ids)

	// Step 4: submit a final transfer as confirmation that the DTN is serving.
	t.Log("step 4: submitting final confirmation transfer...")
	taskID := e.submitTestTransfer(ctx, t)
	info := e.waitForTransferSucceeded(ctx, t, taskID, 2*time.Minute)
	t.Logf("step 4 complete: transfer %s SUCCEEDED (%d bytes)", taskID, info.BytesTransferred)

	// Step 5: restore to desired=0 and wait for warm pool to refill.
	t.Log("step 5: restoring to desired=0 + waiting for warm pool...")
	e.scaleASG(ctx, t, 0)
	e.waitForWarmPool(ctx, t, 1, 5*time.Minute)
	t.Log("step 5 complete: warm pool restored")
}

// TestAutoScaleIn validates the full automatic scale-in path after load ends:
//
//	demand_monitor publishes ActiveTransfers=0 → scale-in alarm fires after
//	15 min → scalein Lambda reduces desired → lifecycle hook disables node →
//	instance returns to Warmed:Stopped in warm pool.
//
// Skipped unless ELASTIC_GCS_TEST_SCALE_IN=true.
// Expected runtime: ~20 minutes.
func TestAutoScaleIn(t *testing.T) {
	if os.Getenv("ELASTIC_GCS_TEST_SCALE_IN") != "true" {
		t.Skip("skipping: set ELASTIC_GCS_TEST_SCALE_IN=true to run (~20min)")
	}

	ctx := context.Background()
	e := newTestEnv(t)

	// Safety net: always restore to desired=0 on exit, even on failure.
	t.Cleanup(func() {
		e.scaleASG(context.Background(), t, 0)
	})

	// Precondition: start from desired=0 with at least one warm instance.
	t.Log("precondition: scaling to desired=0...")
	e.scaleASG(ctx, t, 0)
	t.Log("waiting for warm pool (at least 1 Warmed:Stopped)...")
	warmIDs := e.waitForWarmPool(ctx, t, 1, 3*time.Minute)
	t.Logf("warm pool ready: %v", warmIDs)

	// Step 1: submit keepalive transfers to trigger automatic scale-out.
	t.Log("step 1: submitting keepalive transfers for 75s (interval 10s)...")
	keepAliveIDs := e.keepAliveSubmit(ctx, t, 75*time.Second, 10*time.Second)
	t.Logf("step 1 complete: submitted %d tasks", len(keepAliveIDs))

	// Step 2: wait for scaleout Lambda to increase desired (no manual call).
	t.Log("step 2: waiting for automatic scale-out...")
	newDesired := e.waitForASGDesiredChange(ctx, t, 0, 4*time.Minute)
	t.Logf("step 2 complete: ASG desired 0 → %d", newDesired)

	// Step 3: wait for DTN InService.
	t.Log("step 3: waiting for DTN InService...")
	ids := e.waitForASGInService(ctx, t, 1, 5*time.Minute)
	t.Logf("step 3 complete: DTN InService: %v", ids)

	// Step 4: confirmation transfer to verify the DTN is serving.
	t.Log("step 4: submitting confirmation transfer...")
	confirmID := e.submitTestTransfer(ctx, t)
	info := e.waitForTransferSucceeded(ctx, t, confirmID, 2*time.Minute)
	t.Logf("step 4 complete: transfer %s SUCCEEDED (%d bytes)", confirmID, info.BytesTransferred)

	// Step 5: wait for all tasks (keepalive + confirmation) to reach a terminal
	// state. Once they do, demand_monitor will start publishing ActiveTransfers=0
	// and the 15-minute scale-in alarm clock begins.
	allIDs := append(keepAliveIDs, confirmID)
	t.Log("step 5: waiting for all transfers to complete (demand drops to zero)...")
	e.waitForAllTasksTerminal(ctx, t, allIDs, 5*time.Minute)
	t.Log("step 5 complete: all transfers done — scale-in alarm clock starts now")

	// Step 6: wait for the scale-in alarm to fire and the scalein Lambda to
	// reduce ASG desired. The alarm fires after Period=300s × EvalPeriods=3 =
	// 15 min of consecutive zero-demand datapoints.
	t.Log("step 6: waiting for automatic scale-in (~15min for alarm to fire)...")
	scaleInStart := time.Now()
	prevDesired := e.waitForASGDesiredChange(ctx, t, 1, 20*time.Minute)
	t.Logf("step 6 complete: ASG desired 1 → %d after %s (automatic scale-in)",
		prevDesired, time.Since(scaleInStart).Round(time.Second))

	// Step 7: wait for the instance to complete its lifecycle hook (node disable)
	// and return to Warmed:Stopped in the warm pool.
	t.Log("step 7: waiting for instance to return to Warmed:Stopped...")
	e.waitForWarmPool(ctx, t, 1, 5*time.Minute)
	t.Log("step 7 complete: instance back in warm pool — test complete")
}

// TestAutoScaleFull extends TestAutoScaleOut with a second round to prove
// repeatability of the automatic scale-out path.
//
// Skipped unless ELASTIC_GCS_TEST_FULL_CYCLE=true.
// Expected runtime: ~12 minutes.
func TestAutoScaleFull(t *testing.T) {
	if os.Getenv("ELASTIC_GCS_TEST_FULL_CYCLE") != "true" {
		t.Skip("skipping: set ELASTIC_GCS_TEST_FULL_CYCLE=true to run 2-round auto-scale test")
	}

	ctx := context.Background()
	e := newTestEnv(t)

	// Safety net: always restore to desired=0 on exit, even on failure.
	t.Cleanup(func() {
		e.scaleASG(context.Background(), t, 0)
	})

	for round := 1; round <= 2; round++ {
		t.Logf("=== Round %d/2 ===", round)

		// Precondition for each round: desired=0 with warm pool ready.
		t.Logf("round %d: scaling to desired=0...", round)
		e.scaleASG(ctx, t, 0)
		t.Logf("round %d: waiting for warm pool (at least 1 Warmed:Stopped)...", round)
		warmIDs := e.waitForWarmPool(ctx, t, 1, 5*time.Minute)
		t.Logf("round %d: warm pool ready: %v", round, warmIDs)

		// Keep active transfers in-flight for 75s.
		t.Logf("round %d: submitting keepalive transfers for 75s (interval 10s)...", round)
		taskIDs := e.keepAliveSubmit(ctx, t, 75*time.Second, 10*time.Second)
		t.Logf("round %d: submitted %d tasks: %v", round, len(taskIDs), taskIDs)

		// Wait for automatic scale-out (no manual ASG call).
		t.Logf("round %d: waiting for ASG desired to change from 0 (automatic scale-out)...", round)
		newDesired := e.waitForASGDesiredChange(ctx, t, 0, 4*time.Minute)
		t.Logf("round %d: ASG desired 0 → %d", round, newDesired)

		// Wait for DTN InService.
		t.Logf("round %d: waiting for DTN InService...", round)
		ids := e.waitForASGInService(ctx, t, 1, 5*time.Minute)
		t.Logf("round %d: DTN InService: %v", round, ids)

		// Final confirmation transfer.
		t.Logf("round %d: submitting final confirmation transfer...", round)
		taskID := e.submitTestTransfer(ctx, t)
		info := e.waitForTransferSucceeded(ctx, t, taskID, 2*time.Minute)
		t.Logf("round %d: transfer %s SUCCEEDED (%d bytes)", round, taskID, info.BytesTransferred)
	}

	// Final cleanup: restore warm pool.
	t.Log("final cleanup: scaling to desired=0...")
	e.scaleASG(ctx, t, 0)
	e.waitForWarmPool(ctx, t, 1, 5*time.Minute)
	t.Log("warm pool restored — test complete")
}
