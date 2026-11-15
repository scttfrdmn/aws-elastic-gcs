package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func wakeCmd() *cobra.Command {
	var scaleOut bool
	cmd := &cobra.Command{
		Use:   "wake",
		Short: "Start the control node (and optionally scale out a DTN)",
		Long: `Starts the control node EC2 instance if stopped and waits for the GCS
Manager to become healthy. With --scale-out, also sets ASG desired=1 so a
warm-pool DTN begins resuming in parallel with the control node boot.

The waker Lambda is invoked directly via the Lambda Invoke API using your
AWS credentials — no bearer token needed from the CLI.

Typical use:
  aws-elastic-gcs-ctl wake              # start control node only
  aws-elastic-gcs-ctl wake --scale-out  # start control node + bring up one DTN`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWake(cmd.Context(), scaleOut)
		},
	}
	cmd.Flags().BoolVar(&scaleOut, "scale-out", false, "Also set ASG desired=1 to warm up a DTN")
	return cmd
}

// wakerRequest mirrors the WakeRequest struct in lambda/waker/main.go.
type wakerRequest struct {
	ScaleOut bool `json:"scale_out"`
}

// wakerResponse mirrors the WakeResponse struct in lambda/waker/main.go.
type wakerResponse struct {
	Status            string `json:"status"`
	ElapsedSeconds    int    `json:"elapsed_seconds"`
	WarmPoolPopulated bool   `json:"warm_pool_populated"`
	ScaleOutTriggered bool   `json:"scale_out_triggered"`
	Message           string `json:"message,omitempty"`
}

func runWake(ctx context.Context, scaleOut bool) error {
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}

	fnName := dep.Resource("waker")
	payload, _ := json.Marshal(wakerRequest{ScaleOut: scaleOut})

	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)

	fmt.Printf("Invoking waker Lambda (%s)...\n", fnName)
	start := time.Now()

	lambdaClient := lambda.NewFromConfig(cfg)
	out, err := lambdaClient.Invoke(ctx, &lambda.InvokeInput{
		FunctionName: aws.String(fnName),
		Payload:      payload,
	})
	if err != nil {
		return fmt.Errorf("Lambda invoke: %w", err)
	}
	if out.FunctionError != nil {
		return fmt.Errorf("waker Lambda error: %s — %s", aws.ToString(out.FunctionError), string(out.Payload))
	}

	var resp wakerResponse
	if err := json.Unmarshal(out.Payload, &resp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	elapsed := time.Since(start).Round(time.Second)
	if resp.Status == "ready" {
		bold.Printf("\nControl node ready")
		fmt.Printf(" (%ds, %s)\n", resp.ElapsedSeconds, elapsed)
		if resp.Message != "" {
			fmt.Printf("  %s\n", resp.Message)
		}
		if resp.WarmPoolPopulated {
			green.Printf("  Warm pool populated — DTN scale-out will take ~60s\n")
		} else {
			yellow.Printf("  Warm pool empty — DTN scale-out will take 3-5min (cold boot)\n")
		}
		if resp.ScaleOutTriggered {
			fmt.Println("  Scale-out triggered (desired=1)")
		}
	} else {
		return fmt.Errorf("unexpected waker response: %+v", resp)
	}
	return nil
}
