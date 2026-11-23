package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/ssmexec"
)

func wakeCmd() *cobra.Command {
	var scaleOut bool
	cmd := &cobra.Command{
		Use:   "wake",
		Short: "Bring the endpoint up: start the control node and wait until it serves",
		Long: `Starts the control node if it is stopped, waits for SSM, enables its GCS
node and waits until its address is in endpoint DNS. Transfer rejects
submissions to an endpoint with no serving node, so wake an endpoint whose
control node was stopped (control_node: on_demand / none, or
scripts/control_node_power.sh stop) before submitting transfers (#130).

With --scale-out, also asks the slot controller for one DTN (desired-dtns =
1, like 'scale up --to 1'), which comes up in parallel.

Runs entirely with your AWS credentials (EC2, SSM); it needs no Lambda.

  aws-elastic-gcs-ctl wake              # control node only
  aws-elastic-gcs-ctl wake --scale-out  # control node + one DTN`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWake(cmd.Context(), scaleOut)
		},
	}
	cmd.Flags().BoolVar(&scaleOut, "scale-out", false, "Also ask the slot controller for one DTN (desired-dtns = 1)")
	return cmd
}

func runWake(ctx context.Context, scaleOut bool) error {
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}
	ec2c, ssmc := ec2.NewFromConfig(cfg), ssm.NewFromConfig(cfg)
	start := time.Now()

	id, err := getSSMParam(ctx, ssmc, controlNodeIDParamPath())
	if err != nil {
		return fmt.Errorf("control node ID: %w", err)
	}
	if scaleOut {
		if desiredDTNs(ctx, ssmc) < 1 {
			if err := setDesiredDTNs(ctx, ssmc, 1); err != nil {
				return err
			}
			fmt.Println("Asked the slot controller for one DTN (desired-dtns = 1).")
		} else {
			fmt.Println("A DTN is already wanted (desired-dtns >= 1).")
		}
	}

	out, err := ec2c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil || len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return fmt.Errorf("describe control node %s: %v", id, err)
	}
	switch st := instanceState(out.Reservations[0].Instances[0]); st {
	case string(ec2types.InstanceStateNameRunning):
		fmt.Printf("Control node %s is running.\n", id)
	case string(ec2types.InstanceStateNameStopped):
		fmt.Printf("Starting control node %s...\n", id)
		if _, err := ec2c.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}}); err != nil {
			return fmt.Errorf("start control node: %w", err)
		}
		if err := ec2.NewInstanceRunningWaiter(ec2c).Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}}, 10*time.Minute); err != nil {
			return fmt.Errorf("wait for control node to run: %w", err)
		}
	default:
		return fmt.Errorf("control node %s is %s; wait for it to settle and retry", id, st)
	}

	fmt.Println("Waiting for SSM...")
	if err := waitSSMOnline(ctx, ssmc, id, 10*time.Minute); err != nil {
		return err
	}
	// The boot unit (elastic-gcs-control.service) runs this on boot; running
	// it again is idempotent and waits (bounded) for the address in DNS.
	fmt.Println("Enabling the GCS node and waiting for endpoint DNS...")
	res, err := ssmexec.RunOK(ctx, ssmc, id, "/opt/elastic-gcs/scripts/control_node_boot.sh start",
		ssmexec.Options{Wait: 5 * time.Minute, Comment: "elastic-gcs: wake"})
	if err != nil {
		return fmt.Errorf("enable control node: %w", err)
	}
	if res.Stdout != "" {
		fmt.Print(res.Stdout)
	}
	color.Green("Endpoint serving via the control node (%s).\n", time.Since(start).Round(time.Second))
	return nil
}

// waitSSMOnline polls until the instance's SSM agent reports Online.
func waitSSMOnline(ctx context.Context, c *ssm.Client, id string, max time.Duration) error {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		out, err := c.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
			Filters: []ssmtypes.InstanceInformationStringFilter{{Key: aws.String("InstanceIds"), Values: []string{id}}},
		})
		if err == nil && len(out.InstanceInformationList) > 0 && out.InstanceInformationList[0].PingStatus == ssmtypes.PingStatusOnline {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return errors.New("control node never came online in SSM")
}
