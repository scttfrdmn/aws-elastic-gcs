//go:build integration

package integration

// TestTransferEndToEnd submits a 15KB file from the 1000 Genomes Open Data
// collection to Globus Tutorial Endpoint 2 and asserts SUCCEEDED within 5 minutes.
//
// Prerequisites:
//   - GLOBUS_CLIENT_ID and GLOBUS_CLIENT_SECRET set
//
// The test is self-contained: if no DTN slot is serving it sets desired-dtns
// to 1, runs the transfer, then sets it back to 0 via t.Cleanup.
//
// Run: make test-integration REGION=us-east-1 -run TestTransferEndToEnd
import (
	"context"
	"testing"
	"time"
)

func TestTransferEndToEnd(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)

	// Ensure at least one DTN slot is serving. Scale one out if needed.
	t.Log("checking for serving DTN slots...")
	existing := e.servingSlotIDs(ctx)
	if len(existing) == 0 {
		t.Log("no serving DTN slots — setting desired-dtns=1 for this test")
		e.setDesired(ctx, t, 1)
		t.Cleanup(func() {
			t.Log("cleanup: desired-dtns back to 0")
			e.setDesired(context.Background(), t, 0)
		})
		ids := e.waitForServingSlots(ctx, t, 1, 5*time.Minute)
		t.Logf("DTN slot serving: %v — waiting for GCS node to activate...", ids)
		waitForActiveGCSNode(ctx, t, e, 2*time.Minute)
	} else {
		t.Logf("using existing serving DTN slot(s): %v", existing)
	}

	// Submit the transfer and wait for completion.
	t.Logf("submitting transfer: collection=%s path=%s",
		getEnvOrDefault("ELASTIC_GCS_COLLECTION_ID", collection1000Genomes),
		testSourceFile)
	start := time.Now()
	taskID := e.submitTestTransfer(ctx, t)
	task := e.waitForTransferSucceeded(ctx, t, taskID, 5*time.Minute)
	elapsed := time.Since(start).Round(time.Millisecond)

	t.Logf("transfer SUCCEEDED")
	t.Logf("  task ID:           %s", taskID)
	t.Logf("  bytes transferred: %d", task.BytesTransferred)
	t.Logf("  elapsed:           %s", elapsed)

	if task.BytesTransferred <= 0 {
		t.Errorf("expected bytes_transferred > 0, got %d", task.BytesTransferred)
	}
	if elapsed > 5*time.Minute {
		t.Errorf("transfer took %s (expected < 5m)", elapsed)
	}
}

// waitForActiveGCSNode polls until at least one GCS node is active, up to timeout.
func waitForActiveGCSNode(ctx context.Context, t *testing.T, e *testEnv, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		if hasActiveGCSNode(e.listGCSNodes(ctx, t)) {
			return
		}
	}
	t.Fatalf("GCS node did not become active within %s", timeout)
}
