package main

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func scaleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scale",
		Short: "Set the number of DTNs the slot controller keeps serving",
		Long: `Sets desired-dtns, which the slot controller converges to. It also refreshes
last-active, so demand_monitor keeps the value for scale_in_idle; after that,
with no active transfers, demand sets it back to 0. While transfers are
active, demand only ever raises desired-dtns.`,
	}

	var toN int
	var watch bool

	upCmd := &cobra.Command{
		Use:   "up",
		Short: "Increase desired DTNs",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScale(context.Background(), toN, true, watch)
		},
	}
	upCmd.Flags().IntVar(&toN, "to", 0, "Target desired DTNs (default: current+1)")
	upCmd.Flags().BoolVar(&watch, "watch", false, "Poll until that many slots are running")

	downCmd := &cobra.Command{
		Use:   "down",
		Short: "Decrease desired DTNs",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScale(context.Background(), toN, false, watch)
		},
	}
	downCmd.Flags().IntVar(&toN, "to", -1, "Target desired DTNs (default: current-1)")
	downCmd.Flags().BoolVar(&watch, "watch", false, "Poll until that many slots are running")

	cmd.AddCommand(upCmd, downCmd)
	return cmd
}

func runScale(ctx context.Context, toN int, up bool, watch bool) error {
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}
	ssmClient := ssm.NewFromConfig(cfg)
	current := desiredDTNs(ctx, ssmClient)
	maxCap := maxDTNs(ctx, lambda.NewFromConfig(cfg))

	newDesired := current
	switch {
	case up && toN > 0, !up && toN >= 0:
		newDesired = toN
	case up:
		newDesired = current + 1
	default:
		newDesired = current - 1
	}
	newDesired = max(newDesired, 0)
	if maxCap > 0 {
		newDesired = min(newDesired, maxCap)
	}
	if newDesired == current {
		fmt.Printf("Already at desired=%d, no change.\n", current)
		return nil
	}

	dir := "↑"
	if newDesired < current {
		dir = "↓"
	}
	fmt.Printf("Scaling %s  desired-dtns: %d → %d\n", dir, current, newDesired)
	if err := setDesiredDTNs(ctx, ssmClient, newDesired); err != nil {
		return err
	}
	if !watch {
		fmt.Println("Set. The slot controller converges within about a minute; run `status` to follow it.")
		return nil
	}

	ec2Client := ec2.NewFromConfig(cfg)
	fmt.Printf("Watching...")
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		slots, err := listSlotInstances(ctx, ec2Client)
		if err != nil {
			continue
		}
		serving := 0
		for _, s := range slots {
			if s.State == "running" && s.Phase == "" {
				serving++
			}
		}
		fmt.Printf("\r  serving=%d  desired=%d   ", serving, newDesired)
		if serving == newDesired {
			fmt.Println()
			color.Green("  Done.\n")
			return nil
		}
	}
	fmt.Println()
	return fmt.Errorf("timed out waiting for the slot controller")
}
