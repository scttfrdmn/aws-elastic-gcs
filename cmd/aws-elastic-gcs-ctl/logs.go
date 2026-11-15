package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func logsCmd() *cobra.Command {
	var since string
	var tailFlag bool
	var jsonFlag bool

	cmd := &cobra.Command{
		Use:   "logs <lambda>",
		Short: "View Lambda CloudWatch logs",
		Long: fmt.Sprintf("Stream CloudWatch Logs for a Lambda.\nAvailable: %s",
			strings.Join(sortedLambdaNames(), ", ")),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(args[0], since, tailFlag, jsonFlag)
		},
	}
	cmd.Flags().StringVar(&since, "since", "1h", "Show logs since duration (e.g. 30m, 1h, 24h)")
	cmd.Flags().BoolVar(&tailFlag, "tail", false, "Continuously poll for new log events (every 2s)")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Emit raw JSON lines (pipe-friendly for jq)")
	return cmd
}

func runLogs(lambdaName, since string, tail, asJSON bool) error {
	base, ok := lambdaFunctionBases[lambdaName]
	if !ok {
		return fmt.Errorf("unknown lambda %q — available: %s",
			lambdaName, strings.Join(sortedLambdaNames(), ", "))
	}
	logGroup := "/aws/lambda/" + base + "-" + dep.ID()

	dur, err := parseDuration(since)
	if err != nil {
		return err
	}
	startTime := time.Now().Add(-dur)
	startMs := startTime.UnixMilli()

	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}
	cwlogsClient := cloudwatchlogs.NewFromConfig(cfg)

	fmt.Printf("Logs: %s  (since %s)\n\n", logGroup, startTime.Format("15:04:05"))

	var nextToken *string
	for {
		input := &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName: aws.String(logGroup),
			StartTime:    aws.Int64(startMs),
		}
		if nextToken != nil {
			input.NextToken = nextToken
		}

		out, err := cwlogsClient.FilterLogEvents(ctx, input)
		if err != nil {
			return fmt.Errorf("FilterLogEvents: %w", err)
		}

		for _, event := range out.Events {
			msg := strings.TrimRight(aws.ToString(event.Message), "\n")
			if asJSON {
				fmt.Println(msg)
			} else {
				prettyPrintLogLine(msg)
			}
		}

		if out.NextToken != nil {
			nextToken = out.NextToken
		} else if !tail {
			break
		} else {
			if len(out.Events) > 0 {
				startMs = aws.ToInt64(out.Events[len(out.Events)-1].Timestamp) + 1
			}
			nextToken = nil
			time.Sleep(2 * time.Second)
		}
	}
	return nil
}

func prettyPrintLogLine(line string) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		fmt.Println(line)
		return
	}
	var entry map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &entry); err != nil {
		fmt.Println(line)
		return
	}

	level := strings.ToUpper(fmt.Sprint(entry["level"]))
	ts := fmt.Sprint(entry["time"])
	msg := fmt.Sprint(entry["msg"])

	var pairs []string
	skipKeys := map[string]bool{"level": true, "time": true, "msg": true}
	for k, v := range entry {
		if !skipKeys[k] {
			pairs = append(pairs, fmt.Sprintf("%s=%v", k, v))
		}
	}
	sort.Strings(pairs)

	formatted := fmt.Sprintf("[%s] %s %s %s", level, ts, msg, strings.Join(pairs, " "))
	switch level {
	case "ERROR":
		color.Red("%s\n", formatted)
	case "WARN", "WARNING":
		color.Yellow("%s\n", formatted)
	default:
		fmt.Println(formatted)
	}
}
