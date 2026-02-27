//go:build integration

package integration

// TestScaleOutLatency measures the wall-clock time for each phase of the warm-path
// scale-out cycle and asserts SLOs for the GlobusWorld presentation.
//
// Warm path (resume from Warmed:Stopped):
//
//	SLO: ≤ 90s to InService  (assert < 120s)
//
// Cold path (launch from AMI) is tested only when ELASTIC_GCS_TEST_COLD_PATH=true.
// It requires draining the warm pool, which terminates warm instances, so it is
// opt-in.
//
// Run N iterations via ELASTIC_GCS_ITERATIONS (default: 1; issue #22 recommends 3).
//
// Run: make test-integration REGION=us-east-1 -run TestScaleOutLatency
import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// scalePhase records timing for one phase of the scale-out cycle.
type scalePhase struct {
	label   string
	elapsed time.Duration
}

func TestScaleOutLatency(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)

	iterations := 1
	if v := os.Getenv("ELASTIC_GCS_ITERATIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			iterations = n
		}
	}

	// Ensure we start from desired=0 with at least one warm instance.
	t.Log("ensuring ASG desired=0 before measuring...")
	e.scaleASG(ctx, t, 0)
	warmIDs := e.waitForWarmPool(ctx, t, 1, 5*time.Minute)
	t.Logf("warm pool ready: %v", warmIDs)

	t.Cleanup(func() {
		// Always restore to desired=0 after test.
		e.scaleASG(context.Background(), t, 0)
	})

	var results [][]scalePhase // one entry per iteration

	for iter := 0; iter < iterations; iter++ {
		if iter > 0 {
			// Between iterations: scale back to 0 and wait for warm pool.
			t.Logf("iteration %d/%d: resetting to desired=0...", iter+1, iterations)
			e.scaleASG(ctx, t, 0)
			e.waitForWarmPool(ctx, t, 1, 5*time.Minute)
			time.Sleep(5 * time.Second) // brief settle
		}
		t.Logf("=== iteration %d/%d ===", iter+1, iterations)

		phases := measureWarmScaleOut(ctx, t, e)
		results = append(results, phases)

		// Print per-iteration breakdown.
		var total time.Duration
		for _, p := range phases {
			t.Logf("  %-35s %s", p.label+":", p.elapsed.Round(time.Millisecond))
			total += p.elapsed
		}
		t.Logf("  %-35s %s", "TOTAL:", total.Round(time.Millisecond))
	}

	// Aggregate stats across iterations.
	if iterations > 1 {
		printStats(t, results)
	}

	// Assert SLO on the last iteration's InService time.
	for _, p := range results[len(results)-1] {
		if p.label == "desired=1 → InService" && p.elapsed > 120*time.Second {
			t.Errorf("warm scale-out SLO exceeded: %s (expect < 120s)", p.elapsed)
		}
		if p.label == "InService → GCS node active" && p.elapsed > 90*time.Second {
			t.Errorf("GCS node activation SLO exceeded: %s (expect < 90s)", p.elapsed)
		}
	}
}

// measureWarmScaleOut runs one scale-out measurement cycle and returns timing phases.
func measureWarmScaleOut(ctx context.Context, t *testing.T, e *testEnv) []scalePhase {
	t.Helper()

	// Phase 1: desired=1 → InService
	t0 := time.Now()
	e.scaleASG(ctx, t, 1)
	e.waitForASGInService(ctx, t, 1, 3*time.Minute)
	t1 := time.Now()

	// Phase 2: InService → GCS node active (node enable + DNS propagation)
	t2 := waitForActiveGCSNodeTimed(ctx, t, e, 2*time.Minute)

	// Phase 3: GCS ready → transfer submitted
	t3 := time.Now()
	taskID := e.submitTestTransfer(ctx, t)
	t4 := time.Now()

	// Phase 4: transfer submitted → first bytes received
	t5 := waitForFirstBytes(ctx, t, e, taskID, 3*time.Minute)

	// Phase 5: first bytes → SUCCEEDED
	task := e.waitForTransferSucceeded(ctx, t, taskID, 5*time.Minute)
	t6 := time.Now()

	t.Logf("  transfer %s: %d bytes", taskID, task.BytesTransferred)

	return []scalePhase{
		{"desired=1 → InService", t1.Sub(t0)},
		{"InService → GCS node active", t2.Sub(t1)},
		{"GCS ready → transfer submitted", t4.Sub(t3)},
		{"transfer submitted → first bytes", t5.Sub(t4)},
		{"first bytes → SUCCEEDED", t6.Sub(t5)},
	}
}

// waitForActiveGCSNodeTimed polls until an active GCS node appears, returns the time it happened.
func waitForActiveGCSNodeTimed(ctx context.Context, t *testing.T, e *testEnv, timeout time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		if hasActiveGCSNode(e.listGCSNodes(ctx, t)) {
			return time.Now()
		}
	}
	t.Fatalf("GCS node did not become active within %s", timeout)
	return time.Now() // unreachable
}

// waitForFirstBytes polls until task.BytesTransferred > 0, returns the time it happened.
func waitForFirstBytes(ctx context.Context, t *testing.T, e *testEnv, taskID string, timeout time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		task, err := e.transferClient.GetTask(ctx, taskID)
		if err != nil {
			continue
		}
		if task.BytesTransferred > 0 {
			return time.Now()
		}
		// If task is already complete (small file), treat that as first bytes too.
		if task.Status == "SUCCEEDED" {
			return time.Now()
		}
		if task.Status == "FAILED" || task.Status == "CANCELLED" {
			t.Fatalf("transfer %s: status=%s", taskID, task.Status)
		}
	}
	t.Fatalf("no bytes transferred for task %s within %s", taskID, timeout)
	return time.Now() // unreachable
}

// printStats prints mean and max across all iterations for each phase.
func printStats(t *testing.T, results [][]scalePhase) {
	t.Helper()
	if len(results) == 0 {
		return
	}
	t.Logf("\n=== aggregate stats (%d iterations) ===", len(results))
	numPhases := len(results[0])
	for i := 0; i < numPhases; i++ {
		label := results[0][i].label
		var total, max time.Duration
		for _, phases := range results {
			d := phases[i].elapsed
			total += d
			if d > max {
				max = d
			}
		}
		mean := total / time.Duration(len(results))
		t.Logf("  %-35s mean=%s  max=%s", label+":", mean.Round(time.Millisecond), max.Round(time.Millisecond))
	}
}

// labeledDuration is a helper for formatting benchmark output lines.
func labeledDuration(label string, d time.Duration) string {
	return fmt.Sprintf("  %-35s %s", label+":", d.Round(time.Millisecond))
}
