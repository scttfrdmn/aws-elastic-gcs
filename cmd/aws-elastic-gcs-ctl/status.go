package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/gcscli"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/scaling"
)

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show system status (control node, DTN slots, GCS nodes, active transfers)",
		RunE:  runStatus,
	}
}

func runStatus(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}

	ssmClient := ssm.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
	cwClient := cloudwatch.NewFromConfig(cfg)

	// Control node + endpoint IDs from SSM
	controlNodeID, err := getSSMParam(ctx, ssmClient, controlNodeIDParamPath())
	if err != nil {
		return fmt.Errorf("cannot get control node ID: %w", err)
	}
	endpointID, _ := getSSMParam(ctx, ssmClient, endpointIDParamPath())

	// Describe control node
	var controlIP, controlState string
	controlInfo, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{controlNodeID},
	})
	if err == nil && len(controlInfo.Reservations) > 0 && len(controlInfo.Reservations[0].Instances) > 0 {
		inst := controlInfo.Reservations[0].Instances[0]
		controlIP = aws.ToString(inst.PublicIpAddress)
		controlState = instanceState(inst)
	}

	// Slot controller: desired-dtns and the live slot instances (#65).
	desired := desiredDTNs(ctx, ssmClient)
	slots, err := listSlotInstances(ctx, ec2Client)
	if err != nil {
		return err
	}
	var serving, warm int
	for _, s := range slots {
		switch {
		case s.State == "running" && s.Phase == "":
			serving++
		case s.State == "stopped":
			warm++
		}
	}

	// GCS node list — only available when control node is running.
	var gcsNodes []gcscli.Node
	if controlState == "running" {
		gcsNodes, _ = listGCSNodes(ctx, ssmClient, controlNodeID)
	}

	// Active transfers (last 5 min)
	var activeTransfers float64
	stats, err := cwClient.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String(names.MetricNamespace),
		MetricName: aws.String("ActiveTransfers"),
		StartTime:  aws.Time(time.Now().Add(-5 * time.Minute)),
		EndTime:    aws.Time(time.Now()),
		Period:     aws.Int32(300),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticMaximum},
		Dimensions: []cwtypes.Dimension{
			{Name: aws.String("Region"), Value: aws.String(dep.Region)},
			{Name: aws.String("Deployment"), Value: aws.String(dep.Name)},
		},
	})
	if err == nil {
		for _, v := range scaling.Maxima(stats.Datapoints) {
			activeTransfers = max(activeTransfers, v)
		}
	}

	// ── Render ────────────────────────────────────────────────────────────────
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	red := color.New(color.FgRed)
	dim := color.New(color.Faint)

	bold.Printf("\nElastic GCS — %s\n", dep.ID())
	fmt.Println(strings.Repeat("━", 60))

	// Control node
	sym, sc := "●", green
	if controlState != "running" {
		sym, sc = "○", red
	}
	fmt.Printf("Control Node   %-24s", controlNodeID)
	sc.Printf("%s %-12s", sym, controlState)
	fmt.Printf("  %s\n", controlIP)

	// Endpoint
	fmt.Printf("Endpoint       %s\n", endpointID)

	fmt.Println()

	// DTN slots
	bold.Printf("DTN Slots      ")
	fmt.Printf("desired=%-3d  serving=%-3d  warm=%d\n", desired, serving, warm)
	for _, s := range slots {
		phase := s.Phase
		if phase == "" {
			phase = "-"
		}
		fmt.Printf("  slot %-3d  %-20s  %-14s  phase=%-10s  %s\n", s.Slot, s.ID, s.State, phase, s.IP)
	}

	fmt.Println()

	// GCS nodes
	bold.Printf("GCS Nodes\n")
	if controlState != "running" {
		dim.Printf("  (control node stopped — run: aws-elastic-gcs-ctl wake)\n")
	}
	for _, n := range gcsNodes {
		sym, nc := "○", dim
		if n.Status == "active" {
			sym, nc = "●", green
		}
		shortID := n.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		label := ""
		if n.IP() == controlIP {
			label = "(control)"
		}
		nc.Printf("  %s  ", sym)
		fmt.Printf("%-10s  %-18s  %-8s  %s\n", shortID, n.IP(), n.Status, label)
	}

	fmt.Println()
	fmt.Printf("Active Transfers (5 min):  %.0f\n", activeTransfers)
	return nil
}
