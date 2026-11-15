package main

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func scaleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scale",
		Short: "Scale DTN pool up or down",
	}

	var toN int
	var watch bool

	upCmd := &cobra.Command{
		Use:   "up",
		Short: "Increase DTN desired capacity",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScale(context.Background(), toN, true, watch)
		},
	}
	upCmd.Flags().IntVar(&toN, "to", 0, "Target desired capacity (default: current+1)")
	upCmd.Flags().BoolVar(&watch, "watch", false, "Poll until stable")

	downCmd := &cobra.Command{
		Use:   "down",
		Short: "Decrease DTN desired capacity",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScale(context.Background(), toN, false, watch)
		},
	}
	downCmd.Flags().IntVar(&toN, "to", 0, "Target desired capacity (default: current-1)")
	downCmd.Flags().BoolVar(&watch, "watch", false, "Poll until stable")

	cmd.AddCommand(upCmd, downCmd)
	return cmd
}

func runScale(ctx context.Context, toN int, up bool, watch bool) error {
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}
	asgClient := autoscaling.NewFromConfig(cfg)

	groups, err := asgClient.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName()},
	})
	if err != nil {
		return fmt.Errorf("cannot describe ASG: %w", err)
	}
	if len(groups.AutoScalingGroups) == 0 {
		return fmt.Errorf("ASG %s not found", asgName())
	}
	asg := groups.AutoScalingGroups[0]
	current := int(aws.ToInt32(asg.DesiredCapacity))
	maxCap := int(aws.ToInt32(asg.MaxSize))

	newDesired := current
	switch {
	case toN > 0:
		newDesired = toN
	case up:
		newDesired = current + 1
	default:
		newDesired = current - 1
	}
	if newDesired < 0 {
		newDesired = 0
	}
	if newDesired > maxCap {
		newDesired = maxCap
	}

	if newDesired == current {
		fmt.Printf("Already at desired=%d, no change.\n", current)
		return nil
	}

	dir := "↑"
	if newDesired < current {
		dir = "↓"
	}
	fmt.Printf("Scaling %s  desired: %d → %d\n", dir, current, newDesired)

	_, err = asgClient.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: aws.String(asgName()),
		DesiredCapacity:      aws.Int32(int32(newDesired)),
		HonorCooldown:        aws.Bool(false),
	})
	if err != nil {
		return fmt.Errorf("SetDesiredCapacity: %w", err)
	}

	if !watch {
		fmt.Println("Scale request submitted. Run `status` to monitor progress.")
		return nil
	}

	fmt.Printf("Watching...")
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		g, err := asgClient.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
			AutoScalingGroupNames: []string{asgName()},
		})
		if err != nil || len(g.AutoScalingGroups) == 0 {
			continue
		}
		var inSvc int
		for _, inst := range g.AutoScalingGroups[0].Instances {
			if string(inst.LifecycleState) == "InService" {
				inSvc++
			}
		}
		fmt.Printf("\r  InService=%d  desired=%d   ", inSvc, newDesired)
		if inSvc == newDesired {
			fmt.Println()
			color.Green("  Done.\n")
			return nil
		}
	}
	fmt.Println()
	return fmt.Errorf("timed out waiting for scale to complete")
}
