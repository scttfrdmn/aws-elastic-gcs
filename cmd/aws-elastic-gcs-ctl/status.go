package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
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
		Short: "Show system status (control node, DTN pool, GCS nodes, metrics, alarms)",
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
	asgClient := autoscaling.NewFromConfig(cfg)
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

	// ASG desired + in-service count
	var desired, inService int
	groups, err := asgClient.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName()},
	})
	if err == nil && len(groups.AutoScalingGroups) > 0 {
		desired = int(aws.ToInt32(groups.AutoScalingGroups[0].DesiredCapacity))
		for _, inst := range groups.AutoScalingGroups[0].Instances {
			if string(inst.LifecycleState) == "InService" {
				inService++
			}
		}
	}

	// Warm pool instances with slot + node ID tags
	type dtnInfo struct {
		instanceID, slot, state, nodeID string
	}
	var dtnNodes []dtnInfo
	wp, err := asgClient.DescribeWarmPool(ctx, &autoscaling.DescribeWarmPoolInput{
		AutoScalingGroupName: aws.String(asgName()),
	})
	if err == nil && len(wp.Instances) > 0 {
		var warmIDs []string
		stateByID := make(map[string]string)
		for _, inst := range wp.Instances {
			id := aws.ToString(inst.InstanceId)
			warmIDs = append(warmIDs, id)
			stateByID[id] = string(inst.LifecycleState)
		}
		// Tags are best-effort: on error the slot and node columns are blank.
		tagsByID := make(map[string][]ec2types.Tag)
		ec2Out, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: warmIDs,
		})
		if err == nil {
			for _, r := range ec2Out.Reservations {
				for _, i := range r.Instances {
					tagsByID[aws.ToString(i.InstanceId)] = i.Tags
				}
			}
		}
		for _, id := range warmIDs {
			info := dtnInfo{instanceID: id, state: stateByID[id]}
			for _, tag := range tagsByID[id] {
				switch aws.ToString(tag.Key) {
				case names.TagSlot:
					info.slot = aws.ToString(tag.Value)
				case names.TagGCSNodeID:
					v := aws.ToString(tag.Value)
					if len(v) > 8 {
						v = v[:8]
					}
					info.nodeID = v
				}
			}
			dtnNodes = append(dtnNodes, info)
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

	// Alarm states
	alarmNames := []string{
		dep.Resource("scaleout"),
		dep.Resource("scaleout-throughput"),
		dep.Resource("scalein"),
		dep.Resource("version-drift"),
	}
	alarmStates := make(map[string]string)
	alarmsOut, err := cwClient.DescribeAlarms(ctx, &cloudwatch.DescribeAlarmsInput{
		AlarmNames: alarmNames,
	})
	if err == nil {
		for _, a := range alarmsOut.MetricAlarms {
			alarmStates[aws.ToString(a.AlarmName)] = string(a.StateValue)
		}
	}

	// ── Render ────────────────────────────────────────────────────────────────
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	red := color.New(color.FgRed)
	yellow := color.New(color.FgYellow)
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

	// DTN pool
	bold.Printf("DTN Pool       ")
	fmt.Printf("desired=%-3d  in-service=%-3d  warm=%d\n", desired, inService, len(dtnNodes))
	for _, d := range dtnNodes {
		fmt.Printf("  %-24s  slot=%-3s  %-22s  node=%s\n",
			d.instanceID, d.slot, d.state, d.nodeID)
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

	// Alarms line
	fmt.Printf("Alarms:")
	for _, name := range alarmNames {
		short := alarmShortName(name)
		state := alarmStates[name]
		switch state {
		case "OK":
			fmt.Printf("  %s ", short)
			green.Printf("○ OK")
		case "ALARM":
			fmt.Printf("  %s ", short)
			red.Printf("● ALARM")
		default:
			fmt.Printf("  %s ", short)
			yellow.Printf("? %s", state)
		}
	}
	fmt.Println()
	return nil
}

// alarmShortName returns the label printed for an alarm on the status line:
// the kind after the "elastic-gcs-" prefix (scaleout, scalein, version-drift).
// Names with an unexpected shape are printed whole rather than indexed past
// their end.
func alarmShortName(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 3 {
		return name
	}
	short := parts[2] // scaleout / scalein / version
	if short == "version" && len(parts) > 3 {
		short = "version-drift"
	}
	return short
}
