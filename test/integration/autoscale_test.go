//go:build integration

package integration

// TestAutoScaleOut and TestAutoScaleFull exercise the automatic scale-out path:
//
//	CloudWatch scale-out alarm (triggered by triggerScaleOut) → SNS → scaleout
//	Lambda → ASG DesiredCapacity increases without any manual call
//
// TestAutoScaleOut  — single round, ~4 min
// TestAutoScaleFull — two rounds proving repeatability, ~10 min; opt-in via
//
//	ELASTIC_GCS_TEST_FULL_CYCLE=true
import (
	"context"
	"os"
	"testing"
	"time"
)

// TestAutoScaleOut exercises the full automatic scale-out path driven by Globus
// transfer demand. Scale-out is triggered by the CloudWatch alarm firing (via
// SetAlarmState), not by any manual ASG call.
//
// Expected runtime: ~4 minutes.
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

	// Step 1: submit one demand transfer and immediately trigger the scale-out
	// alarm. The alarm fires SNS → scaleout Lambda → ASG DesiredCapacity++.
	t.Log("step 1: triggering scale-out via demand transfer + alarm...")
	e.triggerScaleOut(ctx, t)
	t.Log("step 1 complete: demand transfer submitted, alarm fired")

	// Step 2: wait for scaleout Lambda to increase ASG desired (no manual call).
	t.Log("step 2: waiting for ASG desired to change from 0 (automatic scale-out)...")
	newDesired := e.waitForASGDesiredChange(ctx, t, 0, 8*time.Minute)
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

// TestAutoScaleIn validates the scale-in path end-to-end:
//
//	scale-in alarm fires (forced via SetAlarmState) → scalein Lambda reduces
//	desired → lifecycle hook disables node → instance returns to Warmed:Stopped.
//
// Scale-out is triggered the same way as TestAutoScaleOut (SetAlarmState on
// the scale-out alarm). Scale-in is also force-triggered because the organic
// 15-minute path (demand_monitor → CloudWatch 3×5min window → alarm) is not
// suitable for automated testing: tiny test transfers (13s, 1168 bytes) finish
// before demand_monitor even polls once, and endpoint_manager/task_list has a
// ~1-2 min lag before removing SUCCEEDED tasks from ACTIVE.
//
// Skipped unless ELASTIC_GCS_TEST_SCALE_IN=true.
// Expected runtime: ~12 minutes (includes ~7min CloudWatch metric cleanup window).
func TestAutoScaleIn(t *testing.T) {
	if os.Getenv("ELASTIC_GCS_TEST_SCALE_IN") != "true" {
		t.Skip("skipping: set ELASTIC_GCS_TEST_SCALE_IN=true to run (~12min)")
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

	// Step 1: submit one demand transfer and immediately trigger the scale-out alarm.
	t.Log("step 1: triggering scale-out via demand transfer + alarm...")
	keepAliveIDs := e.triggerScaleOut(ctx, t)
	t.Log("step 1 complete: demand transfer submitted, alarm fired")

	// Step 2: wait for scaleout Lambda to increase desired (no manual call).
	t.Log("step 2: waiting for automatic scale-out...")
	newDesired := e.waitForASGDesiredChange(ctx, t, 0, 8*time.Minute)
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

	// Step 5a: wait for all ASG scaling activities to complete before triggering
	// scale-in. The ASG rejects SetDesiredCapacity with ScalingActivityInProgress
	// while the scale-out lifecycle hook is finishing (health-check grace period,
	// typically ~5 min after InService). The scale-in Lambda now retries on this
	// error internally, but waiting here avoids burning Lambda execution time.
	t.Log("step 5a: waiting for ASG scaling activities to complete...")
	e.waitForScalingActivityComplete(ctx, t, 10*time.Minute)
	t.Log("step 5a complete: no in-progress scaling activities")

	// Step 5b: wait for CloudWatch ActiveTransfers metric to show 0 for the
	// last 5 minutes. The scale-in Lambda checks this before acting — if
	// non-zero data exists in that window it silently skips the scale-in.
	// endpoint_manager/task_list lag means demand_monitor may publish non-zero
	// for ~2 min post-completion; combined with the 5-min lookback that's up
	// to ~7 minutes before the Lambda guard clears.
	t.Log("step 5b: waiting for CloudWatch ActiveTransfers to clear (Lambda guard)...")
	e.waitForCleanScaleInMetric(ctx, t, 10*time.Minute)
	t.Log("step 5b complete: metric history clean")

	// Step 6: trigger scale-in by forcing the scale-in alarm to ALARM state,
	// then wait for the scalein Lambda to reduce ASG desired.
	//
	// We force the alarm rather than waiting for organic scale-in (~15 min)
	// because endpoint_manager/task_list has a ~1-2 min lag after a task
	// SUCCEEDS before removing it from ACTIVE, which resets CloudWatch's
	// 3×5-min evaluation window. With 13-second test transfers this makes
	// organic scale-in unreliable in any reasonable test timeout.
	//
	// What this tests: scale-in Lambda fires → ASG desired drops → lifecycle
	// hook (node disable → stop) completes → instance returns to Warmed:Stopped.
	t.Log("step 6: triggering scale-in alarm + waiting for automatic scale-in...")
	scaleInStart := time.Now()
	e.triggerScaleIn(ctx, t)
	prevDesired := e.waitForASGDesiredChange(ctx, t, 1, 5*time.Minute)
	t.Logf("step 6 complete: ASG desired 1 → %d after %s (scale-in Lambda fired)",
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
// Expected runtime: ~20 minutes.
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

		// Submit one demand transfer and fire the scale-out alarm.
		t.Logf("round %d: triggering scale-out via demand transfer + alarm...", round)
		e.triggerScaleOut(ctx, t)
		t.Logf("round %d: demand transfer submitted, alarm fired", round)

		// Wait for automatic scale-out (no manual ASG call).
		t.Logf("round %d: waiting for ASG desired to change from 0 (automatic scale-out)...", round)
		newDesired := e.waitForASGDesiredChange(ctx, t, 0, 8*time.Minute)
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
