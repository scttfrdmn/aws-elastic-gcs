// elastic-gcs-ctl is an operator CLI for managing Elastic GCS deployments.
// It provides a one-stop view of system state and commands for common operations,
// designed for live GlobusWorld demos and day-to-day operations.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/scttfrdmn/globus-go-sdk/v3/pkg/services/transfer"
)

const toolVersion = "v0.4.0"

// Well-known collection and endpoint IDs for test transfers.
const (
	collection1000Genomes = "bb3e8265-a9f6-4179-a367-f6cfaf4a29af"
	tutorialEndpoint2     = "31ce9ba0-176d-45a5-add3-f37d233ba47d"
	// nativeAppClientID is the public (no-secret) Globus native app registration
	// used for interactive CLI authentication (transfer test, etc.).
	nativeAppClientID = "fd6f02b5-c2aa-4f69-9654-fd27e26f09da"
	testTransferFile      = "/1000genomes/release/20130502/20140625_related_individuals.txt"
	testTransferDest      = "/~/elastic-gcs-test.txt"
)

var (
	regionFlag  string
	profileFlag string
	rootCmd     = &cobra.Command{
		Use:   "elastic-gcs-ctl",
		Short: "Elastic GCS operator CLI",
		Long:  "elastic-gcs-ctl — manage Elastic Globus Connect Server deployments on AWS",
	}
)

func init() {
	defaultRegion := os.Getenv("AWS_REGION")
	if defaultRegion == "" {
		defaultRegion = "us-east-1"
	}
	defaultProfile := os.Getenv("AWS_PROFILE")
	if defaultProfile == "" {
		defaultProfile = "aws"
	}

	rootCmd.PersistentFlags().StringVar(&regionFlag, "region", defaultRegion, "AWS region")
	rootCmd.PersistentFlags().StringVar(&profileFlag, "profile", defaultProfile, "AWS profile")

	rootCmd.AddCommand(statusCmd())
	rootCmd.AddCommand(scaleCmd())
	rootCmd.AddCommand(nodesCmd())
	rootCmd.AddCommand(logsCmd())
	rootCmd.AddCommand(metricsCmd())
	rootCmd.AddCommand(refreshCmd())
	rootCmd.AddCommand(transferCmd())
	rootCmd.AddCommand(buildAMICmd())
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("elastic-gcs-ctl %s\n", toolVersion)
		},
	})
}

// loadAWSConfig creates an AWS SDK config with the --region and --profile flags.
func loadAWSConfig(ctx context.Context) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(regionFlag),
	}
	if profileFlag != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profileFlag))
	}
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

// getSSMParam fetches a single SSM parameter value.
func getSSMParam(ctx context.Context, ssmClient *ssm.Client, name string) (string, error) {
	out, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Parameter.Value), nil
}

func secretsPrefix() string { return fmt.Sprintf("elastic-gcs/%s", regionFlag) }
func asgName() string        { return fmt.Sprintf("elastic-gcs-dtn-%s", regionFlag) }
func controlNodeIDParamPath() string {
	return fmt.Sprintf("/elastic-gcs/%s/control-node-id", regionFlag)
}
func endpointIDParamPath() string {
	return fmt.Sprintf("/elastic-gcs/%s/endpoint-id", regionFlag)
}

// runGCSCLIOnControlNode sends an SSM command to the control node that sets
// up GCS CLI auth from Secrets Manager and runs the given GCS command.
func runGCSCLIOnControlNode(ctx context.Context, ssmClient *ssm.Client, controlNodeID, gcsCmd string) (string, error) {
	pfx := secretsPrefix()
	region := regionFlag
	script := fmt.Sprintf(`
export GCS_CLI_CLIENT_ID=$(aws secretsmanager get-secret-value \
    --secret-id %s/globus-client-id \
    --query SecretString --output text --region %s 2>/dev/null)
export GCS_CLI_CLIENT_SECRET=$(aws secretsmanager get-secret-value \
    --secret-id %s/globus-client-secret \
    --query SecretString --output text --region %s 2>/dev/null)
export GCS_CLI_ENDPOINT_ID=$(aws ssm get-parameter \
    --name /elastic-gcs/%s/endpoint-id \
    --query Parameter.Value --output text --region %s 2>/dev/null)
%s`, pfx, region, pfx, region, region, region, gcsCmd)

	cmdOut, err := ssmClient.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:  []string{controlNodeID},
		DocumentName: aws.String("AWS-RunShellScript"),
		Parameters: map[string][]string{
			"commands":         {script},
			"executionTimeout": {"60"},
		},
	})
	if err != nil {
		return "", fmt.Errorf("SSM SendCommand: %w", err)
	}

	commandID := aws.ToString(cmdOut.Command.CommandId)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		inv, err := ssmClient.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(commandID),
			InstanceId: aws.String(controlNodeID),
		})
		if err != nil {
			continue
		}
		switch inv.Status {
		case ssmtypes.CommandInvocationStatusSuccess:
			return aws.ToString(inv.StandardOutputContent), nil
		case ssmtypes.CommandInvocationStatusFailed:
			return aws.ToString(inv.StandardOutputContent),
				fmt.Errorf("command failed: %s", aws.ToString(inv.StandardErrorContent))
		case ssmtypes.CommandInvocationStatusTimedOut,
			ssmtypes.CommandInvocationStatusCancelled,
			ssmtypes.CommandInvocationStatusCancelling:
			return "", fmt.Errorf("SSM command in terminal state: %s", inv.Status)
		}
	}
	return "", fmt.Errorf("timed out waiting for SSM command")
}

// ─────────────────────────────────────────────────────────────────────────────
// status
// ─────────────────────────────────────────────────────────────────────────────

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show system status (control node, DTN pool, GCS nodes, metrics, alarms)",
		RunE:  runStatus,
	}
}

type gcsNodeResult struct {
	Data []gcsNode `json:"data"`
}
type gcsNode struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	IPAddress string `json:"ip_address"`
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
		controlState = string(inst.State.Name)
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
		ec2Out, _ := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: warmIDs,
		})
		tagsByID := make(map[string][]ec2types.Tag)
		for _, r := range ec2Out.Reservations {
			for _, i := range r.Instances {
				tagsByID[aws.ToString(i.InstanceId)] = i.Tags
			}
		}
		for _, id := range warmIDs {
			info := dtnInfo{instanceID: id, state: stateByID[id]}
			for _, tag := range tagsByID[id] {
				switch aws.ToString(tag.Key) {
				case "elastic-gcs:slot":
					info.slot = aws.ToString(tag.Value)
				case "elastic-gcs:gcs-node-id":
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

	// GCS node list from control node via SSM
	var gcsNodes []gcsNode
	nodeJSON, ssmErr := runGCSCLIOnControlNode(ctx, ssmClient, controlNodeID,
		"globus-connect-server node list -F json 2>/dev/null")
	if ssmErr == nil {
		var results []gcsNodeResult
		if json.Unmarshal([]byte(nodeJSON), &results) == nil && len(results) > 0 {
			gcsNodes = results[0].Data
		}
	}

	// Active transfers (last 5 min)
	var activeTransfers float64
	stats, _ := cwClient.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("ElasticGCS"),
		MetricName: aws.String("ActiveTransfers"),
		StartTime:  aws.Time(time.Now().Add(-5 * time.Minute)),
		EndTime:    aws.Time(time.Now()),
		Period:     aws.Int32(300),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticMaximum},
		Dimensions: []cwtypes.Dimension{
			{Name: aws.String("Region"), Value: aws.String(regionFlag)},
		},
	})
	if len(stats.Datapoints) > 0 {
		activeTransfers = aws.ToFloat64(stats.Datapoints[0].Maximum)
	}

	// Alarm states
	alarmNames := []string{
		fmt.Sprintf("elastic-gcs-scaleout-%s", regionFlag),
		fmt.Sprintf("elastic-gcs-scalein-%s", regionFlag),
		fmt.Sprintf("elastic-gcs-version-drift-%s", regionFlag),
	}
	alarmStates := make(map[string]string)
	alarmsOut, _ := cwClient.DescribeAlarms(ctx, &cloudwatch.DescribeAlarmsInput{
		AlarmNames: alarmNames,
	})
	for _, a := range alarmsOut.MetricAlarms {
		alarmStates[aws.ToString(a.AlarmName)] = string(a.StateValue)
	}

	// ── Render ────────────────────────────────────────────────────────────────
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	red := color.New(color.FgRed)
	yellow := color.New(color.FgYellow)
	dim := color.New(color.Faint)

	bold.Printf("\nElastic GCS — %s\n", regionFlag)
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
		if n.IPAddress == controlIP {
			label = "(control)"
		}
		nc.Printf("  %s  ", sym)
		fmt.Printf("%-10s  %-18s  %-8s  %s\n", shortID, n.IPAddress, n.Status, label)
	}

	fmt.Println()
	fmt.Printf("Active Transfers (5 min):  %.0f\n", activeTransfers)

	// Alarms line
	fmt.Printf("Alarms:")
	for _, name := range alarmNames {
		parts := strings.Split(name, "-")
		short := parts[2] // scaleout / scalein / version
		if short == "version" && len(parts) > 3 {
			short = "version-drift"
		}
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

// ─────────────────────────────────────────────────────────────────────────────
// scale
// ─────────────────────────────────────────────────────────────────────────────

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
	if err != nil || len(groups.AutoScalingGroups) == 0 {
		return fmt.Errorf("cannot describe ASG: %w", err)
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

// ─────────────────────────────────────────────────────────────────────────────
// nodes
// ─────────────────────────────────────────────────────────────────────────────

func nodesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "nodes", Short: "GCS node operations"}
	cmd.AddCommand(nodesListCmd(), nodesDeleteCmd(), nodesVersionCheckCmd())
	return cmd
}

func nodesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all GCS nodes on the endpoint",
		RunE:  runNodesList,
	}
}

func runNodesList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}
	ssmClient := ssm.NewFromConfig(cfg)

	controlNodeID, err := getSSMParam(ctx, ssmClient, controlNodeIDParamPath())
	if err != nil {
		return fmt.Errorf("cannot get control node ID: %w", err)
	}

	nodeJSON, err := runGCSCLIOnControlNode(ctx, ssmClient, controlNodeID,
		"globus-connect-server node list -F json 2>/dev/null")
	if err != nil {
		return fmt.Errorf("node list failed: %w", err)
	}

	var results []gcsNodeResult
	if err := json.Unmarshal([]byte(nodeJSON), &results); err != nil || len(results) == 0 {
		return fmt.Errorf("failed to parse node list (output: %.300s)", nodeJSON)
	}

	nodes := results[0].Data
	fmt.Printf("%-36s  %-18s  %-10s\n", "NODE ID", "IP ADDRESS", "STATUS")
	fmt.Println(strings.Repeat("─", 68))
	for _, n := range nodes {
		if n.Status == "active" {
			color.Green("%-36s  %-18s  %s\n", n.ID, n.IPAddress, n.Status)
		} else {
			color.New(color.Faint).Printf("%-36s  %-18s  %s\n", n.ID, n.IPAddress, n.Status)
		}
	}
	fmt.Printf("\n%d nodes total\n", len(nodes))
	return nil
}

func nodesDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <node-id>",
		Short: "Delete a GCS node registration from the endpoint",
		Args:  cobra.ExactArgs(1),
		RunE:  runNodesDelete,
	}
}

func runNodesDelete(cmd *cobra.Command, args []string) error {
	nodeID := args[0]
	fmt.Printf("Are you sure you want to delete GCS node %s? [y/N] ", nodeID)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	if strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
		fmt.Println("Aborted.")
		return nil
	}

	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}
	ssmClient := ssm.NewFromConfig(cfg)
	controlNodeID, err := getSSMParam(ctx, ssmClient, controlNodeIDParamPath())
	if err != nil {
		return fmt.Errorf("cannot get control node ID: %w", err)
	}

	out, err := runGCSCLIOnControlNode(ctx, ssmClient, controlNodeID,
		fmt.Sprintf("globus-connect-server node delete %s 2>&1", nodeID))
	if err != nil {
		return fmt.Errorf("node delete failed: %w", err)
	}
	fmt.Printf("node delete %s:\n%s\n", nodeID, strings.TrimSpace(out))
	return nil
}

func nodesVersionCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version-check",
		Short: "Invoke the version drift check Lambda and display results",
		RunE:  runNodesVersionCheck,
	}
}

func runNodesVersionCheck(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}
	lambdaClient := lambda.NewFromConfig(cfg)
	logsClient := cloudwatchlogs.NewFromConfig(cfg)

	fnName := fmt.Sprintf("elastic-gcs-version-check-%s", regionFlag)
	logGroup := fmt.Sprintf("/aws/lambda/%s", fnName)
	fmt.Printf("Invoking %s...\n", fnName)

	invokeStart := time.Now()
	result, err := lambdaClient.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   aws.String(fnName),
		InvocationType: lambdatypes.InvocationTypeRequestResponse,
	})
	if err != nil {
		return fmt.Errorf("Lambda invoke failed: %w", err)
	}

	if result.FunctionError != nil {
		color.Red("Function error: %s\n", aws.ToString(result.FunctionError))
		if len(result.Payload) > 0 {
			fmt.Printf("Payload: %s\n", string(result.Payload))
		}
		return nil
	}

	// Lambda completed successfully. Pull the structured log lines from this
	// invocation so we can show actual version data rather than just "check the metric".
	// Retry for up to 15s to allow CloudWatch Logs to flush.
	type logLine struct {
		Msg      string              `json:"msg"`
		Versions map[string]string   `json:"versions"`
		Distinct int                 `json:"distinct_versions"`
		Drift    int                 `json:"drift_count"`
	}

	startMS := invokeStart.UnixMilli()
	var summary *logLine
	for attempt := 0; attempt < 5; attempt++ {
		time.Sleep(3 * time.Second)
		filterOut, ferr := logsClient.FilterLogEvents(ctx, &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName:  aws.String(logGroup),
			StartTime:     aws.Int64(startMS),
			FilterPattern: aws.String("version check complete"),
		})
		if ferr != nil {
			break
		}
		for _, ev := range filterOut.Events {
			var ll logLine
			if json.Unmarshal([]byte(aws.ToString(ev.Message)), &ll) == nil &&
				ll.Msg == "version check complete" {
				summary = &ll
			}
		}
		if summary != nil {
			break
		}
	}

	if summary == nil {
		color.Green("Version check completed.\n")
		fmt.Println("(Log output not yet available — run `./elastic-gcs-ctl logs version-check --since 5m` to see results)")
		return nil
	}

	fmt.Println()
	if summary.Drift == 0 {
		color.Green("GCSVersionDrift = 0  (all nodes agree)\n")
	} else {
		color.Red("GCSVersionDrift = %d  (VERSION DRIFT DETECTED)\n", summary.Drift)
	}
	fmt.Println()

	// Print per-instance version table.
	fmt.Printf("  %-22s  %s\n", "Instance", "GCS Version")
	fmt.Printf("  %-22s  %s\n", strings.Repeat("─", 22), strings.Repeat("─", 12))
	// Sort instances for stable output.
	instances := make([]string, 0, len(summary.Versions))
	for id := range summary.Versions {
		instances = append(instances, id)
	}
	sort.Strings(instances)
	for _, id := range instances {
		ver := summary.Versions[id]
		if ver == "unknown" {
			color.Yellow("  %-22s  %s\n", id, ver)
		} else {
			fmt.Printf("  %-22s  %s\n", id, ver)
		}
	}
	fmt.Println()
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// logs
// ─────────────────────────────────────────────────────────────────────────────

var lambdaFunctionBases = map[string]string{
	"lifecycle":      "elastic-gcs-lifecycle",
	"scaleout":       "elastic-gcs-scaleout",
	"scalein":        "elastic-gcs-scalein",
	"demand-monitor": "elastic-gcs-demand-monitor",
	"cache-eviction": "elastic-gcs-cache-eviction",
	"version-check":  "elastic-gcs-version-check",
}

func sortedLambdaNames() []string {
	names := make([]string, 0, len(lambdaFunctionBases))
	for k := range lambdaFunctionBases {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

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

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return time.Hour, nil
	}
	unit := s[len(s)-1]
	val, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	switch unit {
	case 'm':
		return time.Duration(val * float64(time.Minute)), nil
	case 'h':
		return time.Duration(val * float64(time.Hour)), nil
	case 'd':
		return time.Duration(val * float64(24 * time.Hour)), nil
	default:
		return 0, fmt.Errorf("unknown duration unit %q (use m, h, d)", string(unit))
	}
}

func runLogs(lambdaName, since string, tail, asJSON bool) error {
	base, ok := lambdaFunctionBases[lambdaName]
	if !ok {
		return fmt.Errorf("unknown lambda %q — available: %s",
			lambdaName, strings.Join(sortedLambdaNames(), ", "))
	}
	logGroup := fmt.Sprintf("/aws/lambda/%s-%s", base, regionFlag)

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

// ─────────────────────────────────────────────────────────────────────────────
// metrics
// ─────────────────────────────────────────────────────────────────────────────

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
		Namespace:  aws.String("ElasticGCS"),
		MetricName: aws.String("ActiveTransfers"),
		StartTime:  aws.Time(time.Now().Add(-dur)),
		EndTime:    aws.Time(time.Now()),
		Period:     aws.Int32(period),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticMaximum},
		Dimensions: []cwtypes.Dimension{
			{Name: aws.String("Region"), Value: aws.String(regionFlag)},
		},
	})
	if err != nil {
		return fmt.Errorf("GetMetricStatistics: %w", err)
	}

	points := out.Datapoints
	sort.Slice(points, func(i, j int) bool {
		return aws.ToTime(points[i].Timestamp).Before(aws.ToTime(points[j].Timestamp))
	})

	fmt.Printf("\nActive Transfers — last %s  [%s]\n", since, regionFlag)
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
		fnName := fmt.Sprintf("%s-%s", base, regionFlag)
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

// ─────────────────────────────────────────────────────────────────────────────
// refresh
// ─────────────────────────────────────────────────────────────────────────────

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

// ─────────────────────────────────────────────────────────────────────────────
// transfer
// ─────────────────────────────────────────────────────────────────────────────

func transferCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "transfer", Short: "Globus transfer operations"}
	cmd.AddCommand(transferTestCmd())
	return cmd
}

func transferTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "Submit a test transfer (1000genomes → Globus Tutorial Collection 2)",
		RunE:  runTransferTest,
	}
}

// globusNativeAppAuth implements the Globus "native app" authorization code flow.
// It prints an authorization URL for the user to open, Globus displays the auth
// code on screen, the user pastes it back, and we exchange it for an access token.
// This approach works with a confidential client and requires no redirect server.
func globusNativeAppAuth(ctx context.Context, clientID, scope string) (string, error) {
	const (
		redirectURI  = "https://auth.globus.org/v2/web/auth-code"
		authorizeURL = "https://auth.globus.org/v2/oauth2/authorize"
		tokenURL     = "https://auth.globus.org/v2/oauth2/token"
	)

	// Step 1: generate PKCE code_verifier and code_challenge (RFC 7636).
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return "", fmt.Errorf("generate code verifier: %w", err)
	}
	codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	challengeHash := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])

	// Step 2: build the authorization URL.
	authURL := authorizeURL + "?" + url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {scope},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	bold := color.New(color.Bold)
	fmt.Println()
	bold.Println("Authenticate with your Globus identity:")
	fmt.Println()
	fmt.Println("  1. Open this URL in your browser:")
	color.Cyan("     %s\n", authURL)
	fmt.Println("  2. Log in and click Allow.")
	fmt.Println("  3. Copy the authorization code shown and paste it here.")
	fmt.Println()
	fmt.Print("Authorization code: ")

	// Step 2: read the code from stdin.
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return "", fmt.Errorf("no authorization code provided")
	}
	authCode := strings.TrimSpace(scanner.Text())
	if authCode == "" {
		return "", fmt.Errorf("authorization code was empty")
	}

	// Step 3: exchange the code for tokens, including the PKCE code_verifier.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader(url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {authCode},
			"redirect_uri":  {redirectURI},
			"client_id":     {clientID},
			"code_verifier": {codeVerifier},
		}.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var tr struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("parse token response: %s", string(body))
	}
	if tr.Error != "" {
		return "", fmt.Errorf("token error: %s — %s", tr.Error, tr.ErrorDesc)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("no access token in response: %s", string(body))
	}
	color.Green("\nAuthenticated.\n\n")
	return tr.AccessToken, nil
}

// staticBearerToken satisfies the globus-go-sdk auth.Authorizer interface.
type staticBearerToken struct{ token string }

func (a *staticBearerToken) GetAuthorizationHeader(_ ...context.Context) (string, error) {
	return "Bearer " + a.token, nil
}

func runTransferTest(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// GCS v5 collections require data_access dependent scopes in addition to the
	// base transfer scope. Request both at once so consent covers the full transfer.
	transferScope := fmt.Sprintf(
		"urn:globus:auth:scope:transfer.api.globus.org:all[*https://auth.globus.org/scopes/%s/data_access *https://auth.globus.org/scopes/%s/data_access]",
		collection1000Genomes, tutorialEndpoint2)

	// Native app authorization code flow — authenticates as the operator's personal
	// Globus identity using the public native app client (no secret required).
	accessToken, err := globusNativeAppAuth(ctx, nativeAppClientID, transferScope)
	if err != nil {
		return fmt.Errorf("Globus authentication: %w", err)
	}

	transferClient, err := transfer.NewClient(
		transfer.WithAuthorizer(&staticBearerToken{token: accessToken}))
	if err != nil {
		return fmt.Errorf("transfer.NewClient: %w", err)
	}

	fmt.Printf("Submitting test transfer:\n")
	fmt.Printf("  Source:  collection %s\n          %s\n", collection1000Genomes, testTransferFile)
	fmt.Printf("  Dest:    collection %s\n          %s\n\n", tutorialEndpoint2, testTransferDest)

	startTime := time.Now()
	label := fmt.Sprintf("elastic-gcs-ctl test %s", startTime.Format("20060102-150405"))

	// The Transfer API requires a unique submission_id obtained from the service.
	subIDResp, err := transferClient.GetSubmissionID(ctx)
	if err != nil {
		return fmt.Errorf("GetSubmissionID: %w", err)
	}

	task, err := transferClient.CreateTransferTask(ctx, &transfer.TransferTaskRequest{
		SubmissionID:          subIDResp,
		SourceEndpointID:      collection1000Genomes,
		DestinationEndpointID: tutorialEndpoint2,
		Label:                 label,
		Items: []transfer.TransferItem{
			{
				SourcePath:      testTransferFile,
				DestinationPath: testTransferDest,
				Recursive:       false,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("CreateTransferTask: %w", err)
	}

	taskID := task.TaskID
	fmt.Printf("Task ID: %s\n\nPolling...\n", taskID)

	for {
		time.Sleep(3 * time.Second)
		status, err := transferClient.GetTask(ctx, taskID)
		if err != nil {
			fmt.Printf("Warning: GetTask: %v\n", err)
			continue
		}
		elapsed := time.Since(startTime).Round(time.Second)
		fmt.Printf("\r  Status=%-12s  elapsed=%s  bytes=%d   ",
			status.Status, elapsed, status.BytesTransferred)
		switch status.Status {
		case "SUCCEEDED":
			fmt.Println()
			color.Green("Transfer SUCCEEDED in %s (%d bytes)\n", elapsed, status.BytesTransferred)
			return nil
		case "FAILED":
			fmt.Println()
			return fmt.Errorf("transfer FAILED: %v", status.FatalErrorDetails)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// build-ami
// ─────────────────────────────────────────────────────────────────────────────

func buildAMICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "build-ami",
		Short: "Build a new DTN AMI (runs scripts/build_ami.sh)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("Building DTN AMI for region %s...\n", regionFlag)
			c := exec.Command("bash", "scripts/build_ami.sh", regionFlag)
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			c.Env = append(os.Environ(),
				"AWS_REGION="+regionFlag,
				"AWS_PROFILE="+profileFlag,
			)
			return c.Run()
		},
	}
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
