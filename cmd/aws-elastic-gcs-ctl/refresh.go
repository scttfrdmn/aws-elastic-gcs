package main

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func refreshCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Start a rolling instance refresh (AMI update)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRefresh(force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Replace all instances regardless of launch template match (SkipMatching=false)")
	return cmd
}

func runRefresh(force bool) error {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}
	asgClient := autoscaling.NewFromConfig(cfg)

	skipMatching := !force
	fmt.Printf("Starting instance refresh on %s (skip-matching=%v)...\n", asgName(), skipMatching)

	refreshOut, err := asgClient.StartInstanceRefresh(ctx, &autoscaling.StartInstanceRefreshInput{
		AutoScalingGroupName: aws.String(asgName()),
		Preferences: &autoscalingtypes.RefreshPreferences{
			SkipMatching: aws.Bool(skipMatching),
		},
	})
	if err != nil {
		return fmt.Errorf("StartInstanceRefresh: %w", err)
	}

	refreshID := aws.ToString(refreshOut.InstanceRefreshId)
	fmt.Printf("Refresh ID: %s\n\n", refreshID)
	fmt.Println("Polling (Ctrl-C to stop — refresh continues in background)...")

	for {
		time.Sleep(10 * time.Second)
		out, err := asgClient.DescribeInstanceRefreshes(ctx, &autoscaling.DescribeInstanceRefreshesInput{
			AutoScalingGroupName: aws.String(asgName()),
			InstanceRefreshIds:   []string{refreshID},
		})
		if err != nil {
			fmt.Printf("Warning: %v\n", err)
			continue
		}
		if len(out.InstanceRefreshes) == 0 {
			break
		}
		r := out.InstanceRefreshes[0]
		pct := aws.ToInt32(r.PercentageComplete)
		status := string(r.Status)
		fmt.Printf("\r  Status=%-20s  Progress=%d%%   ", status, pct)
		switch status {
		case "Successful":
			fmt.Println()
			color.Green("Refresh complete.\n")
			return nil
		case "Failed", "Cancelled":
			fmt.Println()
			return fmt.Errorf("refresh %s: %s", status, aws.ToString(r.StatusReason))
		}
	}
	return nil
}
