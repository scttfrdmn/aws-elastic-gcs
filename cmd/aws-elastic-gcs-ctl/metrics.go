package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/spf13/cobra"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

func metricsCmd() *cobra.Command {
	var since string
	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Show recent CloudWatch metrics with ASCII sparklines",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMetrics(since)
		},
	}
	cmd.Flags().StringVar(&since, "since", "1h", "Time window (e.g. 30m, 1h, 24h)")
	return cmd
}

func runMetrics(since string) error {
	dur, err := parseDuration(since)
	if err != nil {
		return err
	}
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}
	cwClient := cloudwatch.NewFromConfig(cfg)

	period := int32(60)
	if dur > 3*time.Hour {
		period = 300
	}

	out, err := cwClient.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String(names.MetricNamespace),
		MetricName: aws.String("ActiveTransfers"),
		StartTime:  aws.Time(time.Now().Add(-dur)),
		EndTime:    aws.Time(time.Now()),
		Period:     aws.Int32(period),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticMaximum},
		Dimensions: []cwtypes.Dimension{
			{Name: aws.String("Region"), Value: aws.String(dep.Region)},
			{Name: aws.String("Deployment"), Value: aws.String(dep.Name)},
		},
	})
	if err != nil {
		return fmt.Errorf("GetMetricStatistics: %w", err)
	}

	points := out.Datapoints
	sort.Slice(points, func(i, j int) bool {
		return aws.ToTime(points[i].Timestamp).Before(aws.ToTime(points[j].Timestamp))
	})

	fmt.Printf("\nActive Transfers — last %s  [%s]\n", since, dep.ID())
	if len(points) == 0 {
		fmt.Println("  (no data)")
	} else {
		values := make([]float64, len(points))
		maxVal := 0.0
		for i, p := range points {
			v := aws.ToFloat64(p.Maximum)
			values[i] = v
			if v > maxVal {
				maxVal = v
			}
		}
		fmt.Printf("  %s  (latest: %.0f)\n", sparkline(values, maxVal), values[len(values)-1])
	}

	fmt.Printf("\nLambda invocations — last %s\n", since)
	for _, name := range sortedLambdaNames() {
		base := lambdaFunctionBases[name]
		fnName := base + "-" + dep.ID()
		invOut, err := cwClient.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
			Namespace:  aws.String("AWS/Lambda"),
			MetricName: aws.String("Invocations"),
			StartTime:  aws.Time(time.Now().Add(-dur)),
			EndTime:    aws.Time(time.Now()),
			Period:     aws.Int32(int32(dur.Seconds())),
			Statistics: []cwtypes.Statistic{cwtypes.StatisticSum},
			Dimensions: []cwtypes.Dimension{
				{Name: aws.String("FunctionName"), Value: aws.String(fnName)},
			},
		})
		total := 0.0
		if err == nil && len(invOut.Datapoints) > 0 {
			total = aws.ToFloat64(invOut.Datapoints[0].Sum)
		}
		fmt.Printf("  %-22s  %.0f invocations\n", name, total)
	}
	return nil
}

func sparkline(values []float64, maxVal float64) string {
	blocks := []rune("▁▂▃▄▅▆▇█")
	if maxVal == 0 {
		maxVal = 1
	}
	result := make([]rune, len(values))
	for i, v := range values {
		idx := int(math.Round(v / maxVal * float64(len(blocks)-1)))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(blocks) {
			idx = len(blocks) - 1
		}
		result[i] = blocks[idx]
	}
	return string(result)
}
