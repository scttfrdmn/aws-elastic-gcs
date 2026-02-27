//go:build benchmark

// Package benchmark measures scale-out latency and validates cost estimates for
// the GlobusWorld 2026 presentation. It runs each scale-out path (warm/hot/cold)
// multiple times, records per-phase timestamps, and writes structured JSON results.
//
// Run:
//
//	make benchmark REGION=us-east-1 TIER=warm
//	make benchmark REGION=us-east-1 TIER=hot
//	make benchmark REGION=us-east-1 TIER=cold   # also set ELASTIC_GCS_TEST_COLD_PATH=true
package benchmark

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// phaseTiming records the label and elapsed duration for one phase of a scale-out cycle.
type phaseTiming struct {
	Label   string
	Elapsed time.Duration
}

// phaseResult is the JSON-serialisable form of phaseTiming.
type phaseResult struct {
	Label     string `json:"label"`
	ElapsedMs int64  `json:"elapsed_ms"`
}

// iterResult holds all phases for one benchmark iteration.
type iterResult struct {
	Iteration int           `json:"iteration"`
	Phases    []phaseResult `json:"phases"`
	TotalMs   int64         `json:"total_ms"`
}

// phaseStats holds min/mean/p95/max across iterations for one phase.
type phaseStats struct {
	Label  string `json:"label"`
	MinMs  int64  `json:"min_ms"`
	MeanMs int64  `json:"mean_ms"`
	P95Ms  int64  `json:"p95_ms"`
	MaxMs  int64  `json:"max_ms"`
}

// benchmarkReport is the top-level JSON written to test/benchmark/results/.
type benchmarkReport struct {
	Timestamp  string       `json:"timestamp"`
	Region     string       `json:"region"`
	Tier       string       `json:"tier"`
	Iterations int          `json:"iterations"`
	Stats      []phaseStats `json:"stats"`
	Raw        []iterResult `json:"raw_iterations"`
}

// ── Warm path ─────────────────────────────────────────────────────────────────

// TestBenchmarkWarmPath runs N iterations of the warm-path scale-out cycle with
// per-phase timestamps. Default: 5 iterations (override with ELASTIC_GCS_ITERATIONS).
//
// Phases:
//
//	T0 → T1  EC2 resume    (SetDesiredCapacity → EC2 state=running)
//	T1 → T2  SSM online    (EC2 running → SSM agent PingStatus=Online)
//	T2 → T3  node enable + DNS  (SSM online → GCS node active)
//	T3 → T4  first transfer     (GCS ready → transfer SUCCEEDED)
func TestBenchmarkWarmPath(t *testing.T) {
	tier := os.Getenv("ELASTIC_GCS_TIER")
	if tier != "" && tier != "warm" {
		t.Skipf("skipping warm path: ELASTIC_GCS_TIER=%q", tier)
	}

	ctx := context.Background()
	e := newBenchEnv(t)
	iterations := iterCount(5)

	t.Log("ensuring ASG desired=0 with warm pool ready...")
	e.scaleASG(ctx, t, 0)
	warmIDs := e.waitForWarmPool(ctx, t, 1, 5*time.Minute)
	t.Logf("warm pool ready: %v", warmIDs)

	t.Cleanup(func() { e.scaleASG(context.Background(), t, 0) })

	baselineActive := e.countActiveGCSNodes(ctx, t)
	t.Logf("baseline active GCS nodes: %d", baselineActive)

	var results []iterResult
	for iter := 0; iter < iterations; iter++ {
		if iter > 0 {
			t.Logf("--- iteration %d/%d: resetting to desired=0...", iter+1, iterations)
			e.scaleASG(ctx, t, 0)
			e.waitForWarmPool(ctx, t, 1, 5*time.Minute)
			time.Sleep(5 * time.Second)
		}
		t.Logf("=== warm iteration %d/%d ===", iter+1, iterations)

		phases := e.measureWarmIteration(ctx, t, baselineActive)
		results = append(results, buildIterResult(iter+1, phases))
		printPhases(t, phases)
	}

	stats := computeStats(results)
	printStatsTable(t, "warm", iterations, stats)
	assertWarmSLOs(t, stats)
	writeResults(t, e.region, "warm", iterations, stats, results)
}

// measureWarmIteration runs one warm-path iteration and records phase timestamps.
func (e *benchEnv) measureWarmIteration(ctx context.Context, t *testing.T, baselineActive int) []phaseTiming {
	t.Helper()

	// T0: SetDesiredCapacity(1)
	t0 := time.Now()
	e.scaleASG(ctx, t, 1)

	// T1: EC2 instance state = running
	instanceID := e.firstASGInstanceID(ctx, t, 2*time.Minute)
	t1 := e.waitForEC2Running(ctx, t, instanceID, 2*time.Minute)

	// T2: SSM agent PingStatus = Online
	t2 := e.waitForSSMOnline(ctx, t, instanceID, 2*time.Minute)

	// T3: GCS node active in node list
	t3 := e.waitForGCSNodeActive(ctx, t, baselineActive, 3*time.Minute)

	// T4: transfer SUCCEEDED
	taskID := e.submitTestTransfer(ctx, t)
	t4start := time.Now()
	info := e.waitForTransferSucceeded(ctx, t, taskID, 5*time.Minute)
	t4 := time.Now()

	t.Logf("  transfer %s: %d bytes", taskID, info.BytesTransferred)
	_ = t4start

	return []phaseTiming{
		{"EC2 resume (T0→T1)", t1.Sub(t0)},
		{"SSM online (T1→T2)", t2.Sub(t1)},
		{"node enable + DNS (T2→T3)", t3.Sub(t2)},
		{"first transfer (T3→T4)", t4.Sub(t3)},
	}
}

// ── Hot path ──────────────────────────────────────────────────────────────────

// TestBenchmarkHotPath measures transfer latency when a DTN is already InService
// — i.e. the "hot" path where there is no scale-out overhead.
//
// Run: make benchmark REGION=us-east-1 TIER=hot
func TestBenchmarkHotPath(t *testing.T) {
	tier := os.Getenv("ELASTIC_GCS_TIER")
	if tier != "hot" {
		t.Skipf("skipping hot path: set ELASTIC_GCS_TIER=hot")
	}

	ctx := context.Background()
	e := newBenchEnv(t)
	iterations := iterCount(3)

	// Ensure a DTN is InService.
	t.Log("ensuring DTN is InService...")
	e.scaleASG(ctx, t, 1)
	inSvc := e.waitForASGInService(ctx, t, 1, 3*time.Minute)
	t.Logf("DTN InService: %v", inSvc)

	// Wait for GCS node to activate.
	baselineActive := 1 // control node is always active
	for i := 0; i < 12; i++ {
		time.Sleep(10 * time.Second)
		if e.countActiveGCSNodes(ctx, t) > baselineActive {
			t.Log("DTN GCS node active")
			break
		}
		t.Logf("  waiting for DTN GCS node... (%d/12)", i+1)
	}

	t.Cleanup(func() { e.scaleASG(context.Background(), t, 0) })

	var results []iterResult
	for iter := 0; iter < iterations; iter++ {
		t.Logf("=== hot iteration %d/%d ===", iter+1, iterations)

		t0 := time.Now()
		taskID := e.submitTestTransfer(ctx, t)
		t1 := e.waitForFirstBytes(ctx, t, taskID, 3*time.Minute)
		info := e.waitForTransferSucceeded(ctx, t, taskID, 5*time.Minute)
		t2 := time.Now()

		t.Logf("  transfer %s: %d bytes", taskID, info.BytesTransferred)

		phases := []phaseTiming{
			{"submitted → first bytes", t1.Sub(t0)},
			{"first bytes → SUCCEEDED", t2.Sub(t1)},
		}
		results = append(results, buildIterResult(iter+1, phases))
		printPhases(t, phases)

		if iter < iterations-1 {
			time.Sleep(5 * time.Second)
		}
	}

	stats := computeStats(results)
	printStatsTable(t, "hot", iterations, stats)
	writeResults(t, e.region, "hot", iterations, stats, results)
}

// ── Cold path ─────────────────────────────────────────────────────────────────

// TestBenchmarkColdPath measures scale-out from a full cold launch (from AMI).
// This terminates warm pool instances and is destructive — opt-in only.
//
// Prerequisites: ELASTIC_GCS_TEST_COLD_PATH=true
// Run: make benchmark REGION=us-east-1 TIER=cold ELASTIC_GCS_TEST_COLD_PATH=true
func TestBenchmarkColdPath(t *testing.T) {
	if os.Getenv("ELASTIC_GCS_TEST_COLD_PATH") != "true" {
		t.Skip("skipping cold path: set ELASTIC_GCS_TEST_COLD_PATH=true to opt in (DESTRUCTIVE)")
	}
	tier := os.Getenv("ELASTIC_GCS_TIER")
	if tier != "" && tier != "cold" {
		t.Skipf("skipping cold path: ELASTIC_GCS_TIER=%q", tier)
	}

	ctx := context.Background()
	e := newBenchEnv(t)
	iterations := iterCount(3)

	// Drain the warm pool to force cold launches from AMI.
	t.Log("draining warm pool to force cold launch path (DESTRUCTIVE)...")
	e.scaleASG(ctx, t, 0)
	_, err := e.asgClient.DeleteWarmPool(ctx, &autoscaling.DeleteWarmPoolInput{
		AutoScalingGroupName: aws.String(e.asgName),
		ForceDelete:          aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("DeleteWarmPool: %v", err)
	}

	// Wait for warm pool to drain.
	for i := 0; i < 24; i++ {
		time.Sleep(15 * time.Second)
		wp, err := e.asgClient.DescribeWarmPool(ctx,
			&autoscaling.DescribeWarmPoolInput{AutoScalingGroupName: aws.String(e.asgName)})
		if err != nil || len(wp.Instances) == 0 {
			t.Log("warm pool drained")
			break
		}
		t.Logf("  warm pool draining: %d instances remain", len(wp.Instances))
	}

	t.Cleanup(func() {
		t.Log("NOTE: warm pool was deleted; redeploy CDK stack to restore it")
	})

	baselineActive := e.countActiveGCSNodes(ctx, t)

	var results []iterResult
	for iter := 0; iter < iterations; iter++ {
		if iter > 0 {
			t.Logf("--- iteration %d/%d: scaling back to 0...", iter+1, iterations)
			e.scaleASG(ctx, t, 0)
			// Wait for the instance to terminate (no warm pool to catch it).
			for i := 0; i < 20; i++ {
				time.Sleep(15 * time.Second)
				out, err := e.asgClient.DescribeAutoScalingGroups(ctx,
					&autoscaling.DescribeAutoScalingGroupsInput{
						AutoScalingGroupNames: []string{e.asgName},
					})
				if err == nil && len(out.AutoScalingGroups) > 0 &&
					len(out.AutoScalingGroups[0].Instances) == 0 {
					break
				}
			}
		}
		t.Logf("=== cold iteration %d/%d ===", iter+1, iterations)

		phases := e.measureColdIteration(ctx, t, baselineActive)
		results = append(results, buildIterResult(iter+1, phases))
		printPhases(t, phases)
	}

	stats := computeStats(results)
	printStatsTable(t, "cold", iterations, stats)
	writeResults(t, e.region, "cold", iterations, stats, results)
}

// measureColdIteration runs one cold-path iteration and records phase timestamps.
func (e *benchEnv) measureColdIteration(ctx context.Context, t *testing.T, baselineActive int) []phaseTiming {
	t.Helper()

	// T0: SetDesiredCapacity(1) — instance will launch from AMI
	t0 := time.Now()
	e.scaleASG(ctx, t, 1)

	// T1: EC2 instance state = running
	instanceID := e.firstASGInstanceID(ctx, t, 5*time.Minute)
	t1 := e.waitForEC2Running(ctx, t, instanceID, 5*time.Minute)

	// T2: SSM agent online
	t2 := e.waitForSSMOnline(ctx, t, instanceID, 5*time.Minute)

	// T3: GCS node active (node setup --import-node takes ~23s)
	t3 := e.waitForGCSNodeActive(ctx, t, baselineActive, 5*time.Minute)

	// T4: transfer SUCCEEDED
	taskID := e.submitTestTransfer(ctx, t)
	info := e.waitForTransferSucceeded(ctx, t, taskID, 5*time.Minute)
	t4 := time.Now()

	t.Logf("  transfer %s: %d bytes", taskID, info.BytesTransferred)

	return []phaseTiming{
		{"EC2 launch (T0→T1)", t1.Sub(t0)},
		{"SSM online (T1→T2)", t2.Sub(t1)},
		{"node setup + DNS (T2→T3)", t3.Sub(t2)},
		{"first transfer (T3→T4)", t4.Sub(t3)},
	}
}

// ── Cost model ────────────────────────────────────────────────────────────────

// TestCostModel samples 7 days of CloudWatch metrics and validates monthly cost
// estimates against the elastic vs static deployment models.
//
// Run: go test ./test/benchmark/ -tags=benchmark -run TestCostModel
func TestCostModel(t *testing.T) {
	tier := os.Getenv("ELASTIC_GCS_TIER")
	if tier != "" {
		t.Skipf("TestCostModel skipped when ELASTIC_GCS_TIER=%q is set; run standalone", tier)
	}

	ctx := context.Background()
	e := newBenchEnv(t)
	cwClient := cloudwatch.NewFromConfig(e.cfg)

	endTime := time.Now().UTC()
	startTime := endTime.Add(-7 * 24 * time.Hour)

	t.Logf("\n=== Cost Model Validation (%s, past 7 days) ===", e.region)

	// Count 1-hour buckets with active transfers (proxy for DTN running time).
	out, err := cwClient.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("elastic-gcs"),
		MetricName: aws.String("ActiveTransfers"),
		StartTime:  aws.Time(startTime),
		EndTime:    aws.Time(endTime),
		Period:     aws.Int32(3600),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticMaximum},
		Dimensions: []cwtypes.Dimension{
			{Name: aws.String("Region"), Value: aws.String(e.region)},
		},
	})
	if err != nil || len(out.Datapoints) == 0 {
		t.Skip("no ActiveTransfers data in CloudWatch; skipping cost model (run after some transfers)")
	}

	var dtnHours int
	for _, dp := range out.Datapoints {
		if dp.Maximum != nil && *dp.Maximum > 0 {
			dtnHours++
		}
	}

	// Also check QueuedTransfers.
	outQ, errQ := cwClient.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("elastic-gcs"),
		MetricName: aws.String("QueuedTransfers"),
		StartTime:  aws.Time(startTime),
		EndTime:    aws.Time(endTime),
		Period:     aws.Int32(3600),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticSum},
		Dimensions: []cwtypes.Dimension{
			{Name: aws.String("Region"), Value: aws.String(e.region)},
		},
	})
	var queuedEvents int
	if errQ == nil {
		for _, dp := range outQ.Datapoints {
			if dp.Sum != nil && *dp.Sum > 0 {
				queuedEvents++
			}
		}
	}

	// Cost estimates.
	const (
		dtnHourlyCostUSD     = 0.192  // m6i.xlarge on-demand
		controlHourlyCostUSD = 0.0752 // t3a.large on-demand
	)

	dtnWeeklyCost := float64(dtnHours) * dtnHourlyCostUSD
	dtnMonthlyCost := dtnWeeklyCost * (30.0 / 7.0)
	controlMonthlyCost := controlHourlyCostUSD * 24 * 30
	totalElasticMonthly := dtnMonthlyCost + controlMonthlyCost

	// Static: 1 DTN always running + control node.
	staticMonthly := (dtnHourlyCostUSD + controlHourlyCostUSD) * 24 * 30

	t.Logf("\nActivity (7 days):")
	t.Logf("  DTN active hours:        %d", dtnHours)
	t.Logf("  Hours with queued tasks: %d", queuedEvents)

	t.Logf("\nMonthly cost estimates (extrapolated from 7-day sample):")
	t.Logf("  DTN compute (elastic):   $%.2f  (%d hrs/month × $%.3f)",
		dtnMonthlyCost, int(float64(dtnHours)*(30.0/7.0)), dtnHourlyCostUSD)
	t.Logf("  Control node:            $%.2f  (always on)", controlMonthlyCost)
	t.Logf("  TOTAL elastic:           $%.2f/month", totalElasticMonthly)
	t.Logf("  Static (1 DTN always):   $%.2f/month", staticMonthly)

	if totalElasticMonthly < staticMonthly {
		savings := staticMonthly - totalElasticMonthly
		pct := 100 * savings / staticMonthly
		t.Logf("  Savings vs static:       $%.2f/month (%.0f%%)", savings, pct)
	}

	// Assert: elastic cost should be < static cost (otherwise why bother).
	if dtnHours > 0 && totalElasticMonthly >= staticMonthly {
		t.Errorf("elastic monthly cost ($%.2f) >= static ($%.2f) — check usage pattern",
			totalElasticMonthly, staticMonthly)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// iterCount returns the benchmark iteration count from ELASTIC_GCS_ITERATIONS or def.
func iterCount(def int) int {
	if v := os.Getenv("ELASTIC_GCS_ITERATIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// buildIterResult converts a slice of phaseTiming into an iterResult for JSON output.
func buildIterResult(iteration int, phases []phaseTiming) iterResult {
	var phaseResults []phaseResult
	var total time.Duration
	for _, p := range phases {
		phaseResults = append(phaseResults, phaseResult{
			Label:     p.Label,
			ElapsedMs: p.Elapsed.Milliseconds(),
		})
		total += p.Elapsed
	}
	return iterResult{
		Iteration: iteration,
		Phases:    phaseResults,
		TotalMs:   total.Milliseconds(),
	}
}

// printPhases logs per-phase timing to the test log.
func printPhases(t *testing.T, phases []phaseTiming) {
	t.Helper()
	var total time.Duration
	for _, p := range phases {
		t.Logf("  %-35s %s", p.Label+":", p.Elapsed.Round(time.Millisecond))
		total += p.Elapsed
	}
	t.Logf("  %-35s %s", "TOTAL:", total.Round(time.Millisecond))
}

// computeStats computes min/mean/p95/max for each phase across all iterations.
func computeStats(results []iterResult) []phaseStats {
	if len(results) == 0 {
		return nil
	}
	numPhases := len(results[0].Phases)
	stats := make([]phaseStats, numPhases)
	for i := 0; i < numPhases; i++ {
		stats[i].Label = results[0].Phases[i].Label
		var vals []int64
		for _, r := range results {
			if i < len(r.Phases) {
				vals = append(vals, r.Phases[i].ElapsedMs)
			}
		}
		if len(vals) == 0 {
			continue
		}
		sort.Slice(vals, func(a, b int) bool { return vals[a] < vals[b] })
		var sum int64
		for _, v := range vals {
			sum += v
		}
		stats[i].MinMs = vals[0]
		stats[i].MeanMs = sum / int64(len(vals))
		stats[i].MaxMs = vals[len(vals)-1]
		p95idx := int(math.Ceil(0.95*float64(len(vals)))) - 1
		if p95idx < 0 {
			p95idx = 0
		}
		if p95idx >= len(vals) {
			p95idx = len(vals) - 1
		}
		stats[i].P95Ms = vals[p95idx]
	}
	return stats
}

// printStatsTable logs the aggregate stats table to the test log.
func printStatsTable(t *testing.T, tier string, iterations int, stats []phaseStats) {
	t.Helper()
	t.Logf("\n=== %s path aggregate stats (%d iterations) ===", tier, iterations)
	for _, s := range stats {
		t.Logf("  %-35s min=%s  mean=%s  p95=%s  max=%s",
			s.Label+":",
			msToStr(s.MinMs), msToStr(s.MeanMs), msToStr(s.P95Ms), msToStr(s.MaxMs))
	}
}

// msToStr formats milliseconds as a rounded duration string.
func msToStr(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}

// assertWarmSLOs asserts the warm-path SLOs from the issue (#22).
//
//	EC2 resume (T0→T1):      P95 < 90s
//	node enable + DNS (T2→T3): P95 < 60s
func assertWarmSLOs(t *testing.T, stats []phaseStats) {
	t.Helper()
	for _, s := range stats {
		switch s.Label {
		case "EC2 resume (T0→T1)":
			if s.P95Ms > 90_000 {
				t.Errorf("EC2 resume P95 exceeded SLO: %s (expect < 90s)", msToStr(s.P95Ms))
			}
		case "node enable + DNS (T2→T3)":
			if s.P95Ms > 60_000 {
				t.Errorf("node enable P95 exceeded SLO: %s (expect < 60s)", msToStr(s.P95Ms))
			}
		}
	}
}

// writeResults marshals the benchmark report to test/benchmark/results/YYYY-MM-DD-HHmm-REGION-TIER.json.
func writeResults(t *testing.T, region, tier string, iterations int, stats []phaseStats, raw []iterResult) {
	t.Helper()
	report := benchmarkReport{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Region:     region,
		Tier:       tier,
		Iterations: iterations,
		Stats:      stats,
		Raw:        raw,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Logf("warn: could not marshal benchmark report: %v", err)
		return
	}

	resultsDir := filepath.Join("results")
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		t.Logf("warn: could not create results dir: %v", err)
		return
	}

	filename := fmt.Sprintf("%s-%s-%s.json",
		time.Now().UTC().Format("2006-01-02-1504"),
		region,
		tier,
	)
	path := filepath.Join(resultsDir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Logf("warn: could not write results file %s: %v", path, err)
		return
	}
	t.Logf("results written to test/benchmark/%s", path)
}
