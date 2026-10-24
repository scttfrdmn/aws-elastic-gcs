// aws-elastic-gcs-ctl is an operator CLI for managing Elastic GCS deployments.
// It provides a one-stop view of system state and commands for common operations,
// designed for live GlobusWorld demos and day-to-day operations.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cloudformationtypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/fatih/color"
	gcs "github.com/scttfrdmn/globus-gcs-go"
	"github.com/spf13/cobra"

	"github.com/scttfrdmn/aws-elastic-gcs/config"
)

const toolVersion = "v0.6.0"

// Well-known collection and endpoint IDs for test transfers.
const (
	collection1000Genomes = "28daf55e-07a4-4a65-9f54-292b0b4b8739"
	tutorialEndpoint2     = "31ce9ba0-176d-45a5-add3-f37d233ba47d"
	testTransferFile      = "/1000genomes/release/20130502/20140625_related_individuals.txt"
	testTransferDest      = "/~/elastic-gcs-test.txt"
)

var (
	regionFlag  string
	profileFlag string
	rootCmd     = &cobra.Command{
		Use:   "aws-elastic-gcs-ctl",
		Short: "Elastic GCS operator CLI",
		Long:  "aws-elastic-gcs-ctl — manage Elastic Globus Connect Server deployments on AWS",
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
	rootCmd.AddCommand(wakeCmd())
	rootCmd.AddCommand(scaleCmd())
	rootCmd.AddCommand(nodesCmd())
	rootCmd.AddCommand(logsCmd())
	rootCmd.AddCommand(metricsCmd())
	rootCmd.AddCommand(refreshCmd())
	rootCmd.AddCommand(transferCmd())
	rootCmd.AddCommand(buildAMICmd())
	rootCmd.AddCommand(collectionsCmd())
	rootCmd.AddCommand(deprovisionCmd())
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("aws-elastic-gcs-ctl %s\n", toolVersion)
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

	// GCS node list — only available when control node is running.
	var gcsNodes []gcsNode
	if controlState == "running" {
		nodeJSON, ssmErr := runGCSCLIOnControlNode(ctx, ssmClient, controlNodeID,
			"globus-connect-server node list -F json 2>/dev/null")
		if ssmErr == nil {
			var results []gcsNodeResult
			if json.Unmarshal([]byte(nodeJSON), &results) == nil && len(results) > 0 {
				gcsNodes = results[0].Data
			}
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
// wake
// ─────────────────────────────────────────────────────────────────────────────

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

	fnName := fmt.Sprintf("elastic-gcs-waker-%s", regionFlag)
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

	// Resolve a prefix (e.g. "12ba8822") to a full UUID by listing all nodes.
	fullID := nodeID
	if len(nodeID) < 36 {
		nodeJSON, err := runGCSCLIOnControlNode(ctx, ssmClient, controlNodeID,
			"globus-connect-server node list -F json 2>/dev/null")
		if err != nil {
			return fmt.Errorf("node list for ID resolution: %w", err)
		}
		var results []gcsNodeResult
		if json.Unmarshal([]byte(nodeJSON), &results) == nil && len(results) > 0 {
			for _, n := range results[0].Data {
				if strings.HasPrefix(n.ID, nodeID) {
					fullID = n.ID
					break
				}
			}
		}
		if fullID == nodeID {
			return fmt.Errorf("no node found with ID prefix %q", nodeID)
		}
	}

	out, err := runGCSCLIOnControlNode(ctx, ssmClient, controlNodeID,
		fmt.Sprintf("globus-connect-server node delete %s 2>&1", fullID))
	if err != nil {
		return fmt.Errorf("node delete failed: %w", err)
	}
	fmt.Printf("%s\n", strings.TrimSpace(out))
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
		fmt.Println("(Log output not yet available — run `./aws-elastic-gcs-ctl logs version-check --since 5m` to see results)")
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

func runTransferTest(cmd *cobra.Command, args []string) error {
	label := fmt.Sprintf("aws-elastic-gcs-ctl test %s", time.Now().Format("20060102-150405"))
	fmt.Printf("Submitting test transfer via globus CLI:\n")
	fmt.Printf("  Source: %s:%s\n", collection1000Genomes, testTransferFile)
	fmt.Printf("  Dest:   %s:%s\n\n", tutorialEndpoint2, testTransferDest)
	c := exec.Command("globus", "transfer",
		collection1000Genomes+":"+testTransferFile,
		tutorialEndpoint2+":"+testTransferDest,
		"--label", label,
		"--sync-level", "exists")
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
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

// ─────────────────────────────────────────────────────────────────────────────
// collections
// ─────────────────────────────────────────────────────────────────────────────

func collectionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collections",
		Short: "Manage storage gateways and mapped collections on the endpoint",
	}
	cmd.AddCommand(collectionsSetupCmd(), collectionsListCmd())
	return cmd
}

func collectionsSetupCmd() *cobra.Command {
	var configFile string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Idempotently create storage gateways and mapped collections from a YAML config",
		Long: `Reads a YAML file describing S3 buckets and creates a storage gateway and
mapped collection on the deployed GCS endpoint for each bucket in the target
region. Existing gateways and collections are left untouched (idempotent).

The config/open_data_buckets.yaml file shipped with this project describes the
AWS Open Data Registry buckets. You can supply any YAML file in the same format
to configure your own S3 buckets.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCollectionsSetup(configFile, dryRun)
		},
	}
	cmd.Flags().StringVar(&configFile, "config", "config/open_data_buckets.yaml", "path to bucket config YAML")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be created without making changes")
	return cmd
}

func collectionsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List storage gateways and mapped collections on the endpoint",
		RunE:  runCollectionsList,
	}
}

func runCollectionsSetup(configFile string, dryRun bool) error {
	cfg, err := config.LoadOpenDataBuckets(configFile)
	if err != nil {
		return fmt.Errorf("load config %s: %w", configFile, err)
	}
	buckets := cfg.BucketsForRegion(regionFlag)
	if len(buckets) == 0 {
		fmt.Printf("No buckets configured for region %s in %s\n", regionFlag, configFile)
		return nil
	}

	fmt.Printf("Collections setup — region=%s  config=%s  buckets=%d\n\n",
		regionFlag, configFile, len(buckets))

	if dryRun {
		fmt.Printf("%-32s  %-36s  %s\n", "GATEWAY", "COLLECTION", "BUCKET")
		fmt.Println(strings.Repeat("─", 80))
		for _, b := range buckets {
			fmt.Printf("%-32s  %-36s  s3://%s\n", b.GatewayName(), b.ColName(), b.Bucket)
		}
		fmt.Println("\n[dry-run] no changes made")
		return nil
	}

	// Marshal bucket list as JSON for embedding in the SSM shell script.
	// Single-quoting is safe here — JSON contains no single quotes.
	bucketsJSON, err := json.Marshal(buckets)
	if err != nil {
		return fmt.Errorf("marshal buckets: %w", err)
	}

	ctx := context.Background()
	awsCfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}
	ssmClient := ssm.NewFromConfig(awsCfg)
	controlNodeID, err := getSSMParam(ctx, ssmClient, controlNodeIDParamPath())
	if err != nil {
		return fmt.Errorf("cannot get control node ID: %w", err)
	}

	// Build a script that processes all buckets in one SSM invocation.
	// It outputs one JSON line per bucket with the result.
	script := fmt.Sprintf(`
BUCKETS_JSON='%s'
REGION='%s'

EXISTING_GWS=$(globus-connect-server storage-gateway list -F json 2>/dev/null || echo '[]')
EXISTING_COLS=$(globus-connect-server collection list -F json 2>/dev/null || echo '[]')

echo "$BUCKETS_JSON" | jq -c '.[]' | while IFS= read -r entry; do
    NAME=$(echo "$entry" | jq -r '.name')
    BUCKET=$(echo "$entry" | jq -r '.bucket')
    REQ_PAYS=$(echo "$entry" | jq -r '.requester_pays')
    COL_NAME=$(echo "$entry" | jq -r 'if .collection_name != "" and .collection_name != null then .collection_name else ("Open Data: " + .name) end')
    GW_DOMAIN=$(echo "$entry" | jq -r 'if .domain != "" and .domain != null then .domain else "globusid.org" end')

    GW_ID=$(echo "$EXISTING_GWS" | jq -r --arg n "$NAME" '.[0].data[]? | select(.display_name == $n) | .id' 2>/dev/null | head -1)
    GW_CREATED=false

    if [ -z "$GW_ID" ]; then
        AUTH_FLAG="--s3-unauthenticated"
        [ "$REQ_PAYS" = "true" ] && AUTH_FLAG="--s3-user-credential"
        GW_OUT=$(globus-connect-server storage-gateway create s3 \
            "$NAME" \
            --domain "$GW_DOMAIN" \
            --s3-endpoint "https://s3.${REGION}.amazonaws.com" \
            --bucket "$BUCKET" \
            $AUTH_FLAG 2>&1)
        GW_ID=$(echo "$GW_OUT" | grep -oP 'Storage Gateway ID: \K[a-f0-9-]+' || true)
        GW_CREATED=true
    fi

    if [ -z "$GW_ID" ]; then
        echo "{\"name\":\"$NAME\",\"error\":\"failed to create storage gateway\"}"
        continue
    fi

    COL_ID=$(echo "$EXISTING_COLS" | jq -r --arg gwid "$GW_ID" '.[] | select(.storage_gateway_id == $gwid) | .id' 2>/dev/null | head -1)
    COL_CREATED=false

    if [ -z "$COL_ID" ]; then
        COL_OUT=$(globus-connect-server collection create \
            "$GW_ID" / \
            "$COL_NAME" \
            --allow-guest-collections 2>&1)
        COL_ID=$(echo "$COL_OUT" | grep -oP 'Collection ID: \K[a-f0-9-]+' || true)
        COL_CREATED=true
    fi

    echo "{\"name\":\"$NAME\",\"gateway_id\":\"$GW_ID\",\"collection_id\":\"$COL_ID\",\"gateway_created\":$GW_CREATED,\"collection_created\":$COL_CREATED}"
done
`, string(bucketsJSON), regionFlag)

	output, err := runGCSCLIOnControlNode(ctx, ssmClient, controlNodeID, script)
	if err != nil {
		return fmt.Errorf("collections setup: %w", err)
	}

	// Parse and display one result line per bucket.
	type result struct {
		Name              string `json:"name"`
		GatewayID         string `json:"gateway_id"`
		CollectionID      string `json:"collection_id"`
		GatewayCreated    bool   `json:"gateway_created"`
		CollectionCreated bool   `json:"collection_created"`
		Error             string `json:"error"`
	}

	fmt.Printf("%-28s  %-38s  %-38s\n", "BUCKET", "GATEWAY ID", "COLLECTION ID")
	fmt.Println(strings.Repeat("─", 108))

	ok := true
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var r result
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.Error != "" {
			color.Red("%-28s  ERROR: %s\n", r.Name, r.Error)
			ok = false
			continue
		}
		gwTag := ""
		colTag := ""
		if r.GatewayCreated {
			gwTag = " [created]"
		}
		if r.CollectionCreated {
			colTag = " [created]"
		}
		color.Green("%-28s  %-38s  %-38s\n", r.Name,
			r.GatewayID+gwTag,
			r.CollectionID+colTag)
	}

	if !ok {
		return fmt.Errorf("one or more buckets failed — see output above")
	}
	return nil
}

func runCollectionsList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	awsCfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}
	ssmClient := ssm.NewFromConfig(awsCfg)
	controlNodeID, err := getSSMParam(ctx, ssmClient, controlNodeIDParamPath())
	if err != nil {
		return fmt.Errorf("cannot get control node ID: %w", err)
	}

	gwJSON, err := runGCSCLIOnControlNode(ctx, ssmClient, controlNodeID,
		"globus-connect-server storage-gateway list -F json 2>/dev/null")
	if err != nil {
		return fmt.Errorf("storage-gateway list: %w", err)
	}
	colJSON, err := runGCSCLIOnControlNode(ctx, ssmClient, controlNodeID,
		"globus-connect-server collection list -F json 2>/dev/null")
	if err != nil {
		return fmt.Errorf("collection list: %w", err)
	}

	// storage-gateway list uses result#1.x.x wrapper: [{data: [...]}]
	// collection list returns a flat array: [{...}, {...}]
	type gateway struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	type collection struct {
		ID               string `json:"id"`
		DisplayName      string `json:"display_name"`
		StorageGatewayID string `json:"storage_gateway_id"`
	}
	type gwResultPage struct {
		Data []gateway `json:"data"`
	}

	var gwPages []gwResultPage
	var cols []collection
	json.Unmarshal([]byte(gwJSON), &gwPages)
	json.Unmarshal([]byte(colJSON), &cols)

	// Index collections by gateway ID.
	colsByGW := make(map[string][]collection)
	for _, c := range cols {
		colsByGW[c.StorageGatewayID] = append(colsByGW[c.StorageGatewayID], c)
	}

	if len(gwPages) == 0 || len(gwPages[0].Data) == 0 {
		fmt.Println("No storage gateways found.")
		return nil
	}

	for _, gw := range gwPages[0].Data {
		color.New(color.Bold).Printf("%-36s  %s\n", gw.ID, gw.DisplayName)
		gwCols := colsByGW[gw.ID]
		if len(gwCols) == 0 {
			fmt.Printf("  (no collections)\n")
		}
		for _, c := range gwCols {
			fmt.Printf("  %-36s  %s\n", c.ID, c.DisplayName)
		}
		fmt.Println()
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GCS Manager API helpers
// ─────────────────────────────────────────────────────────────────────────────

// getSecretValue fetches a plaintext secret from Secrets Manager.
func getSecretValue(ctx context.Context, smClient *secretsmanager.Client, secretID string) (string, error) {
	out, err := smClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretID),
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.SecretString), nil
}

// globusClientCredentialsToken obtains a Globus access token using the
// OAuth2 client credentials grant.
func globusClientCredentialsToken(clientID, clientSecret, scope string) (string, error) {
	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {scope},
	}
	req, err := http.NewRequest(http.MethodPost,
		"https://auth.globus.org/v2/oauth2/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(clientID, clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if tok.Error != "" {
		return "", fmt.Errorf("globus auth: %s: %s", tok.Error, tok.ErrorDesc)
	}
	return tok.AccessToken, nil
}

// gcsBaseURLFromTransfer looks up the GCS Manager API base URL for an endpoint
// using the Globus Transfer API's endpoint metadata.
func gcsBaseURLFromTransfer(ctx context.Context, endpointID, transferToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://transfer.api.globus.org/v0.10/endpoint/"+endpointID, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+transferToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("transfer API: %w", err)
	}
	defer resp.Body.Close()

	var ep struct {
		HTTPSServer string `json:"https_server"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ep); err != nil {
		return "", fmt.Errorf("parse endpoint response: %w", err)
	}
	if ep.HTTPSServer == "" {
		return "", fmt.Errorf("endpoint %s has no https_server field (GCS v5 required)", endpointID)
	}
	return ep.HTTPSServer, nil
}

// newGCSClient builds an authenticated GCS Manager API client for the current
// region's endpoint. It fetches Globus client credentials from Secrets Manager,
// discovers the endpoint domain via the Transfer API, and obtains a
// manage_collections-scoped access token.
func newGCSClient(ctx context.Context, smClient *secretsmanager.Client, ssmClient *ssm.Client) (*gcs.Client, error) {
	pfx := secretsPrefix()

	clientID, err := getSecretValue(ctx, smClient, pfx+"/globus-client-id")
	if err != nil {
		return nil, fmt.Errorf("get client ID: %w", err)
	}
	clientSecret, err := getSecretValue(ctx, smClient, pfx+"/globus-client-secret")
	if err != nil {
		return nil, fmt.Errorf("get client secret: %w", err)
	}
	endpointID, err := getSSMParam(ctx, ssmClient, endpointIDParamPath())
	if err != nil {
		return nil, fmt.Errorf("get endpoint ID: %w", err)
	}

	// Discover the GCS Manager domain via the Transfer API.
	transferToken, err := globusClientCredentialsToken(clientID, clientSecret,
		"urn:globus:auth:scope:transfer.api.globus.org:all")
	if err != nil {
		return nil, fmt.Errorf("get transfer token: %w", err)
	}
	baseURL, err := gcsBaseURLFromTransfer(ctx, endpointID, transferToken)
	if err != nil {
		return nil, fmt.Errorf("discover endpoint domain: %w", err)
	}

	// Get a manage_collections-scoped token for the GCS Manager API.
	gcsToken, err := globusClientCredentialsToken(clientID, clientSecret,
		gcs.ManageCollectionsScope(endpointID))
	if err != nil {
		return nil, fmt.Errorf("get GCS token: %w", err)
	}

	return gcs.NewClient(baseURL, &gcs.StaticAuthorizer{Token: gcsToken})
}

// ─────────────────────────────────────────────────────────────────────────────
// deprovision
// ─────────────────────────────────────────────────────────────────────────────

// deprovisionInventory holds the complete resource list gathered before teardown.
type deprovisionInventory struct {
	// Control node
	controlNodeID string
	controlState  string // "running", "stopped", "not-found"
	// Globus resources (discovered via GCS Manager API — control node need not be running)
	endpointID    string
	gcsNodeIDs    []string
	collectionIDs []string
	gatewayIDs    []string
	// gcsInitErr is non-empty if the GCS Manager API client could not be built.
	// GCS resources will be empty and the SSM fallback will be used if possible.
	gcsInitErr string
	// ASG
	asgExists       bool
	asgInstanceIDs  []string // InService
	warmInstanceIDs []string // Warmed:Stopped
	// CloudFormation
	cfnStackNames []string
	// Secrets Manager
	secretARNs []string
	// SSM parameters
	ssmParamNames []string
	// CloudWatch log groups
	logGroupNames []string
	// DTN AMI
	amiID       string
	snapshotIDs []string
}

func deprovisionCmd() *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "deprovision",
		Short: "Completely tear down the Elastic GCS deployment (--confirm to execute)",
		Long: `Performs an ordered, complete teardown of all Elastic GCS resources in the
specified region — Globus endpoint nodes/collections/gateways, CloudFormation
stacks, Secrets Manager secrets, SSM parameters, CloudWatch log groups, and
the DTN AMI with its EBS snapshots.

Without --confirm this performs a dry run: inventories all resources and prints
what would be deleted, then exits without making any changes.

With --confirm it executes the full teardown in order.

Note: The Globus endpoint registration itself is not deleted programmatically.
After teardown, a link is printed to remove it manually via the Globus web app.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeprovision(cmd.Context(), confirm)
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Execute teardown (without this flag, dry-run only)")
	return cmd
}

func runDeprovision(ctx context.Context, confirm bool) error {
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("Collecting inventory for %s...\n", regionFlag)
	inv, err := collectDeprovisionInventory(ctx, cfg)
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}

	if !confirm {
		printDeprovisionDryRun(inv)
		return nil
	}

	return executeDeprovision(ctx, cfg, inv)
}

func collectDeprovisionInventory(ctx context.Context, cfg aws.Config) (*deprovisionInventory, error) {
	inv := &deprovisionInventory{}

	ssmClient := ssm.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
	asgClient := autoscaling.NewFromConfig(cfg)
	cfnClient := cloudformation.NewFromConfig(cfg)
	smClient := secretsmanager.NewFromConfig(cfg)
	cwlogsClient := cloudwatchlogs.NewFromConfig(cfg)

	// ── Control node ──────────────────────────────────────────────────────────
	controlNodeID, err := getSSMParam(ctx, ssmClient, controlNodeIDParamPath())
	if err == nil {
		inv.controlNodeID = controlNodeID
		instOut, err2 := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{controlNodeID},
		})
		if err2 == nil && len(instOut.Reservations) > 0 && len(instOut.Reservations[0].Instances) > 0 {
			inv.controlState = string(instOut.Reservations[0].Instances[0].State.Name)
		} else {
			inv.controlState = "not-found"
		}
	} else {
		inv.controlNodeID = ""
		inv.controlState = "not-found"
	}

	inv.endpointID, _ = getSSMParam(ctx, ssmClient, endpointIDParamPath())

	// ── GCS resources (via GCS Manager API — no control node required) ────────
	gcsClient, gcsErr := newGCSClient(ctx, smClient, ssmClient)
	if gcsErr != nil {
		inv.gcsInitErr = gcsErr.Error()
		// Fallback: if control node is running, try SSM RunCommand.
		if inv.controlState == "running" {
			nodeJSON, _ := runGCSCLIOnControlNode(ctx, ssmClient, inv.controlNodeID,
				"globus-connect-server node list -F json 2>/dev/null")
			var pages []gcsNodeResult
			if json.Unmarshal([]byte(nodeJSON), &pages) == nil && len(pages) > 0 {
				for _, n := range pages[0].Data {
					inv.gcsNodeIDs = append(inv.gcsNodeIDs, n.ID)
				}
			}
			colJSON, _ := runGCSCLIOnControlNode(ctx, ssmClient, inv.controlNodeID,
				"globus-connect-server collection list -F json 2>/dev/null")
			type col struct {
				ID string `json:"id"`
			}
			var cols []col
			if json.Unmarshal([]byte(colJSON), &cols) == nil {
				for _, c := range cols {
					inv.collectionIDs = append(inv.collectionIDs, c.ID)
				}
			}
			gwJSON, _ := runGCSCLIOnControlNode(ctx, ssmClient, inv.controlNodeID,
				"globus-connect-server storage-gateway list -F json 2>/dev/null")
			type gw struct{ ID string `json:"id"` }
			type gwPage struct{ Data []gw `json:"data"` }
			var gwPages []gwPage
			if json.Unmarshal([]byte(gwJSON), &gwPages) == nil && len(gwPages) > 0 {
				for _, g := range gwPages[0].Data {
					inv.gatewayIDs = append(inv.gatewayIDs, g.ID)
				}
			}
		}
	} else {
		if nodes, err := gcsClient.ListNodes(ctx); err == nil {
			for _, n := range nodes {
				inv.gcsNodeIDs = append(inv.gcsNodeIDs, n.ID)
			}
		}
		if cols, err := gcsClient.ListCollections(ctx, nil); err == nil {
			for _, c := range cols {
				inv.collectionIDs = append(inv.collectionIDs, c.ID)
			}
		}
		if gws, err := gcsClient.ListStorageGateways(ctx, nil); err == nil {
			for _, g := range gws {
				inv.gatewayIDs = append(inv.gatewayIDs, g.ID)
			}
		}
	}

	// ── ASG + warm pool ───────────────────────────────────────────────────────
	groups, aerr := asgClient.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName()},
	})
	if aerr == nil && len(groups.AutoScalingGroups) > 0 {
		inv.asgExists = true
		for _, inst := range groups.AutoScalingGroups[0].Instances {
			if string(inst.LifecycleState) == "InService" {
				inv.asgInstanceIDs = append(inv.asgInstanceIDs, aws.ToString(inst.InstanceId))
			}
		}
	}
	wp, _ := asgClient.DescribeWarmPool(ctx, &autoscaling.DescribeWarmPoolInput{
		AutoScalingGroupName: aws.String(asgName()),
	})
	if wp != nil {
		for _, inst := range wp.Instances {
			inv.warmInstanceIDs = append(inv.warmInstanceIDs, aws.ToString(inst.InstanceId))
		}
	}

	// ── CloudFormation stacks ────────────────────────────────────────────────
	// DTN stack first (depends on control stack), so delete it first.
	for _, stackName := range []string{
		fmt.Sprintf("ElasticGCS-DTNPool-%s", regionFlag),
		fmt.Sprintf("ElasticGCS-Control-%s", regionFlag),
	} {
		out, err := cfnClient.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
			StackName: aws.String(stackName),
		})
		if err == nil && len(out.Stacks) > 0 {
			if out.Stacks[0].StackStatus != cloudformationtypes.StackStatusDeleteComplete {
				inv.cfnStackNames = append(inv.cfnStackNames, stackName)
			}
		}
	}

	// ── Secrets Manager ───────────────────────────────────────────────────────
	smPrefix := secretsPrefix() + "/"
	var smToken *string
	for {
		smOut, serr := smClient.ListSecrets(ctx, &secretsmanager.ListSecretsInput{
			NextToken: smToken,
		})
		if serr != nil {
			break
		}
		for _, s := range smOut.SecretList {
			name := aws.ToString(s.Name)
			if strings.HasPrefix(name, smPrefix) {
				inv.secretARNs = append(inv.secretARNs, aws.ToString(s.ARN))
			}
		}
		if smOut.NextToken == nil {
			break
		}
		smToken = smOut.NextToken
	}

	// ── SSM parameters ────────────────────────────────────────────────────────
	var ssmToken *string
	for {
		paramOut, perr := ssmClient.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
			Path:      aws.String(fmt.Sprintf("/elastic-gcs/%s", regionFlag)),
			Recursive: aws.Bool(true),
			NextToken: ssmToken,
		})
		if perr != nil {
			break
		}
		for _, p := range paramOut.Parameters {
			inv.ssmParamNames = append(inv.ssmParamNames, aws.ToString(p.Name))
		}
		if paramOut.NextToken == nil {
			break
		}
		ssmToken = paramOut.NextToken
	}

	// ── CloudWatch log groups ─────────────────────────────────────────────────
	logPrefix := "/aws/lambda/elastic-gcs-"
	var lgToken *string
	for {
		lgOut, lgerr := cwlogsClient.DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{
			LogGroupNamePrefix: aws.String(logPrefix),
			NextToken:          lgToken,
		})
		if lgerr != nil {
			break
		}
		for _, lg := range lgOut.LogGroups {
			name := aws.ToString(lg.LogGroupName)
			if strings.HasSuffix(name, "-"+regionFlag) {
				inv.logGroupNames = append(inv.logGroupNames, name)
			}
		}
		if lgOut.NextToken == nil {
			break
		}
		lgToken = lgOut.NextToken
	}

	// ── DTN AMI ───────────────────────────────────────────────────────────────
	inv.amiID, _ = getSSMParam(ctx, ssmClient, fmt.Sprintf("/elastic-gcs/%s/dtn-ami-id", regionFlag))
	if inv.amiID != "" {
		imgOut, imerr := ec2Client.DescribeImages(ctx, &ec2.DescribeImagesInput{
			ImageIds: []string{inv.amiID},
		})
		if imerr == nil && len(imgOut.Images) > 0 {
			for _, bdm := range imgOut.Images[0].BlockDeviceMappings {
				if bdm.Ebs != nil && aws.ToString(bdm.Ebs.SnapshotId) != "" {
					inv.snapshotIDs = append(inv.snapshotIDs, aws.ToString(bdm.Ebs.SnapshotId))
				}
			}
		}
	}

	return inv, nil
}

// truncID returns the first n chars of id, or all of id if shorter.
func truncID(id string, n int) string {
	if len(id) <= n {
		return id
	}
	return id[:n]
}

// truncIDs returns truncated versions of a slice of IDs for display.
func truncIDs(ids []string, n int) []string {
	result := make([]string, len(ids))
	for i, id := range ids {
		result[i] = truncID(id, n)
	}
	return result
}

func printDeprovisionDryRun(inv *deprovisionInventory) {
	bold := color.New(color.Bold)
	yellow := color.New(color.FgYellow)

	bold.Printf("\nElastic GCS — %s deprovision dry run\n", regionFlag)
	fmt.Println(strings.Repeat("━", 52))
	fmt.Println()
	yellow.Printf("⚠  WARNING: --confirm will permanently destroy all resources listed below.\n\n")

	// Globus resources
	fmt.Println("Globus resources:")
	if inv.endpointID != "" {
		fmt.Printf("  Endpoint       %s\n", inv.endpointID)
	} else {
		fmt.Printf("  Endpoint       (not found)\n")
	}
	if inv.gcsInitErr != "" {
		yellow.Printf("  (GCS API unavailable: %s)\n", inv.gcsInitErr)
		if inv.controlState != "running" {
			yellow.Printf("  (control node is %s — SSM fallback also unavailable)\n", inv.controlState)
		}
	}

	noData := ""
	if inv.gcsInitErr != "" && inv.controlState != "running" {
		noData = " — GCS API and SSM fallback unavailable"
	}

	if len(inv.collectionIDs) > 0 {
		fmt.Printf("  Collections    %d: %s\n", len(inv.collectionIDs),
			strings.Join(truncIDs(inv.collectionIDs, 8), "  "))
	} else {
		fmt.Printf("  Collections    (none found%s)\n", noData)
	}
	if len(inv.gatewayIDs) > 0 {
		fmt.Printf("  Gateways       %d: %s\n", len(inv.gatewayIDs),
			strings.Join(truncIDs(inv.gatewayIDs, 8), "  "))
	} else {
		fmt.Printf("  Gateways       (none found%s)\n", noData)
	}
	if len(inv.gcsNodeIDs) > 0 {
		fmt.Printf("  Nodes          %d: %s\n", len(inv.gcsNodeIDs),
			strings.Join(truncIDs(inv.gcsNodeIDs, 8), "  "))
	} else {
		fmt.Printf("  Nodes          (none found%s)\n", noData)
	}

	fmt.Println()
	fmt.Println("AWS resources:")

	// EC2 instances
	hasInstances := false
	if inv.controlNodeID != "" {
		fmt.Printf("  EC2 instances  %s (control, %s)\n", inv.controlNodeID, inv.controlState)
		hasInstances = true
	}
	for _, id := range inv.asgInstanceIDs {
		fmt.Printf("                 %s (DTN, InService)\n", id)
		hasInstances = true
	}
	for _, id := range inv.warmInstanceIDs {
		fmt.Printf("                 %s (DTN, Warmed:Stopped)\n", id)
		hasInstances = true
	}
	if !hasInstances {
		fmt.Printf("  EC2 instances  (none found)\n")
	}

	// CloudFormation stacks
	if len(inv.cfnStackNames) > 0 {
		first := true
		for _, name := range inv.cfnStackNames {
			if first {
				fmt.Printf("  CFN stacks     %s\n", name)
				first = false
			} else {
				fmt.Printf("                 %s\n", name)
			}
		}
	} else {
		fmt.Printf("  CFN stacks     (none found)\n")
	}

	// Secrets
	if len(inv.secretARNs) > 0 {
		fmt.Printf("  Secrets        %d: %s/*\n", len(inv.secretARNs), secretsPrefix())
	} else {
		fmt.Printf("  Secrets        (none found)\n")
	}

	// SSM params
	if len(inv.ssmParamNames) > 0 {
		fmt.Printf("  SSM params     %d: /elastic-gcs/%s/*\n", len(inv.ssmParamNames), regionFlag)
	} else {
		fmt.Printf("  SSM params     (none found)\n")
	}

	// Log groups
	if len(inv.logGroupNames) > 0 {
		fmt.Printf("  Log groups     %d: /aws/lambda/elastic-gcs-*-%s\n", len(inv.logGroupNames), regionFlag)
	} else {
		fmt.Printf("  Log groups     (none found)\n")
	}

	// AMI
	if inv.amiID != "" {
		fmt.Printf("  AMI            %s  (%d snapshot(s))\n", inv.amiID, len(inv.snapshotIDs))
	} else {
		fmt.Printf("  AMI            (none found)\n")
	}

	fmt.Println()
	fmt.Println("Re-run with --confirm to proceed.")
}

func executeDeprovision(ctx context.Context, cfg aws.Config, inv *deprovisionInventory) error {
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)
	red := color.New(color.FgRed)

	const totalSteps = 10
	stepNum := 0

	// printStep prints the step header without a trailing newline.
	printStep := func(desc string) {
		stepNum++
		bold.Printf("[%d/%d] %s", stepNum, totalSteps, desc)
	}
	printOK := func() { green.Printf("  ✓\n") }
	printWarn := func(msg string) { yellow.Printf("  ⚠  %s\n", msg) }
	printErr := func(err error) { red.Printf("  ✗  %s (continuing)\n", err) }

	asgClient := autoscaling.NewFromConfig(cfg)
	ssmClient := ssm.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
	cfnClient := cloudformation.NewFromConfig(cfg)
	smClient := secretsmanager.NewFromConfig(cfg)
	cwlogsClient := cloudwatchlogs.NewFromConfig(cfg)

	var (
		gcsNodesDeleted    int
		collectionsDeleted int
		gatewaysDeleted    int
		cfnStacksDeleted   int
		secretsDeleted     int
		ssmParamsDeleted   int
		logGroupsDeleted   int
		snapshotsDeleted   int
	)

	// ── Step 1: Scale to zero ─────────────────────────────────────────────────
	printStep("Scale to zero and drain ASG...")
	if !inv.asgExists {
		printWarn("ASG not found, skipping")
	} else {
		// Set min/max/desired to 0 and force-delete warm pool.
		_, err1 := asgClient.UpdateAutoScalingGroup(ctx, &autoscaling.UpdateAutoScalingGroupInput{
			AutoScalingGroupName: aws.String(asgName()),
			MinSize:              aws.Int32(0),
			MaxSize:              aws.Int32(0),
			DesiredCapacity:      aws.Int32(0),
		})
		_, err2 := asgClient.DeleteWarmPool(ctx, &autoscaling.DeleteWarmPoolInput{
			AutoScalingGroupName: aws.String(asgName()),
			ForceDelete:          aws.Bool(true),
		})
		if err1 != nil {
			printErr(fmt.Errorf("UpdateASG: %w", err1))
		} else if err2 != nil {
			printErr(fmt.Errorf("DeleteWarmPool: %w", err2))
		} else {
			// Poll until all instances drained (10 min timeout).
			fmt.Printf("\n")
			deadline := time.Now().Add(10 * time.Minute)
			for time.Now().Before(deadline) {
				time.Sleep(10 * time.Second)
				g, gerr := asgClient.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
					AutoScalingGroupNames: []string{asgName()},
				})
				asgCount := 0
				if gerr == nil && len(g.AutoScalingGroups) > 0 {
					asgCount = len(g.AutoScalingGroups[0].Instances)
				}
				wpOut, _ := asgClient.DescribeWarmPool(ctx, &autoscaling.DescribeWarmPoolInput{
					AutoScalingGroupName: aws.String(asgName()),
				})
				wpCount := 0
				if wpOut != nil {
					wpCount = len(wpOut.Instances)
				}
				fmt.Printf("\r  draining: %d ASG instance(s), %d warm pool instance(s)   ",
					asgCount, wpCount)
				if asgCount == 0 && wpCount == 0 {
					break
				}
			}
			fmt.Printf("\n")
			green.Printf("  ✓ ASG drained\n")
		}
	}

	// Build the GCS client once for steps 2–4 and 10.
	gcsClient, gcsErr := newGCSClient(ctx, smClient, ssmClient)
	if gcsErr != nil {
		yellow.Printf("  ⚠  GCS Manager API unavailable (%s)\n", gcsErr)
		yellow.Printf("     Steps 2–4 and 10 will use SSM fallback or be skipped.\n\n")
	}

	// gcsDelete calls the GCS API if available, otherwise falls back to SSM RunCommand.
	gcsDelete := func(apiCall func() error, ssmCmd string) error {
		if gcsErr == nil {
			return apiCall()
		}
		if inv.controlState == "running" {
			_, err := runGCSCLIOnControlNode(ctx, ssmClient, inv.controlNodeID, ssmCmd)
			return err
		}
		return fmt.Errorf("GCS API unavailable and control node is %s", inv.controlState)
	}

	// ── Step 2: GCS node cleanup ──────────────────────────────────────────────
	printStep(fmt.Sprintf("Delete %d GCS node(s)...", len(inv.gcsNodeIDs)))
	if len(inv.gcsNodeIDs) == 0 {
		printWarn("no nodes found")
	} else {
		anyErr := false
		for _, nodeID := range inv.gcsNodeIDs {
			err := gcsDelete(
				func() error { return gcsClient.DeleteNode(ctx, nodeID) },
				fmt.Sprintf("globus-connect-server node delete %s 2>&1 || true", nodeID),
			)
			if err != nil {
				printErr(fmt.Errorf("node %s: %w", truncID(nodeID, 8), err))
				anyErr = true
			} else {
				gcsNodesDeleted++
			}
		}
		if !anyErr {
			printOK()
		}
	}

	// ── Step 3: Delete collections ────────────────────────────────────────────
	printStep(fmt.Sprintf("Delete %d collection(s)...", len(inv.collectionIDs)))
	if len(inv.collectionIDs) == 0 {
		printWarn("no collections found")
	} else {
		anyErr := false
		for _, colID := range inv.collectionIDs {
			err := gcsDelete(
				func() error { return gcsClient.DeleteCollection(ctx, colID) },
				fmt.Sprintf("globus-connect-server collection delete %s 2>&1 || true", colID),
			)
			if err != nil {
				printErr(fmt.Errorf("collection %s: %w", truncID(colID, 8), err))
				anyErr = true
			} else {
				collectionsDeleted++
			}
		}
		if !anyErr {
			printOK()
		}
	}

	// ── Step 4: Delete storage gateways ──────────────────────────────────────
	printStep(fmt.Sprintf("Delete %d storage gateway(s)...", len(inv.gatewayIDs)))
	if len(inv.gatewayIDs) == 0 {
		printWarn("no gateways found")
	} else {
		anyErr := false
		for _, gwID := range inv.gatewayIDs {
			err := gcsDelete(
				func() error { return gcsClient.DeleteStorageGateway(ctx, gwID) },
				fmt.Sprintf("globus-connect-server storage-gateway delete %s 2>&1 || true", gwID),
			)
			if err != nil {
				printErr(fmt.Errorf("gateway %s: %w", truncID(gwID, 8), err))
				anyErr = true
			} else {
				gatewaysDeleted++
			}
		}
		if !anyErr {
			printOK()
		}
	}

	// ── Step 5: Delete CloudFormation stacks ──────────────────────────────────
	printStep(fmt.Sprintf("Delete %d CloudFormation stack(s)...", len(inv.cfnStackNames)))
	if len(inv.cfnStackNames) == 0 {
		printWarn("no stacks found")
	} else {
		fmt.Printf("\n")
		for _, stackName := range inv.cfnStackNames {
			fmt.Printf("  Deleting %s...", stackName)
			_, derr := cfnClient.DeleteStack(ctx, &cloudformation.DeleteStackInput{
				StackName: aws.String(stackName),
			})
			if derr != nil {
				printErr(fmt.Errorf("%s: %w", stackName, derr))
				continue
			}
			// Poll until DELETE_COMPLETE or stack not found (15 min timeout).
			deadline := time.Now().Add(15 * time.Minute)
			deleted := false
			timedOut := false
			for time.Now().Before(deadline) {
				time.Sleep(15 * time.Second)
				out, perr := cfnClient.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
					StackName: aws.String(stackName),
				})
				if perr != nil {
					// Stack no longer exists.
					deleted = true
					break
				}
				if len(out.Stacks) == 0 {
					deleted = true
					break
				}
				st := out.Stacks[0]
				fmt.Printf("\r  %s: %-22s  ", stackName, string(st.StackStatus))
				if st.StackStatus == cloudformationtypes.StackStatusDeleteComplete {
					deleted = true
					break
				}
				if st.StackStatus == cloudformationtypes.StackStatusDeleteFailed {
					printErr(fmt.Errorf("%s DELETE_FAILED: %s",
						stackName, aws.ToString(st.StackStatusReason)))
					break
				}
			}
			if !deleted && !timedOut && time.Now().After(deadline) {
				timedOut = true
			}
			if deleted {
				cfnStacksDeleted++
				fmt.Printf("\r")
				green.Printf("  ✓ %-50s deleted\n", stackName)
			} else if timedOut {
				printErr(fmt.Errorf("%s: timed out waiting for deletion", stackName))
			}
		}
	}

	// ── Step 6: Delete Secrets Manager secrets ────────────────────────────────
	printStep(fmt.Sprintf("Delete %d secret(s)...", len(inv.secretARNs)))
	if len(inv.secretARNs) == 0 {
		printWarn("no secrets found")
	} else {
		anyErr := false
		for _, arn := range inv.secretARNs {
			_, err := smClient.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{
				SecretId:                   aws.String(arn),
				ForceDeleteWithoutRecovery: aws.Bool(true),
			})
			if err != nil {
				printErr(fmt.Errorf("%s: %w", arn, err))
				anyErr = true
			} else {
				secretsDeleted++
			}
		}
		if !anyErr {
			printOK()
		}
	}

	// ── Step 7: Delete SSM parameters ─────────────────────────────────────────
	printStep(fmt.Sprintf("Delete %d SSM parameter(s)...", len(inv.ssmParamNames)))
	if len(inv.ssmParamNames) == 0 {
		printWarn("no SSM parameters found")
	} else {
		anyErr := false
		for i := 0; i < len(inv.ssmParamNames); i += 10 {
			end := i + 10
			if end > len(inv.ssmParamNames) {
				end = len(inv.ssmParamNames)
			}
			out, err := ssmClient.DeleteParameters(ctx, &ssm.DeleteParametersInput{
				Names: inv.ssmParamNames[i:end],
			})
			if err != nil {
				printErr(fmt.Errorf("batch delete: %w", err))
				anyErr = true
			} else {
				ssmParamsDeleted += len(out.DeletedParameters)
			}
		}
		if !anyErr {
			printOK()
		}
	}

	// ── Step 8: Delete CloudWatch log groups ──────────────────────────────────
	printStep(fmt.Sprintf("Delete %d log group(s)...", len(inv.logGroupNames)))
	if len(inv.logGroupNames) == 0 {
		printWarn("no log groups found")
	} else {
		anyErr := false
		for _, lg := range inv.logGroupNames {
			_, err := cwlogsClient.DeleteLogGroup(ctx, &cloudwatchlogs.DeleteLogGroupInput{
				LogGroupName: aws.String(lg),
			})
			if err != nil {
				printErr(fmt.Errorf("%s: %w", lg, err))
				anyErr = true
			} else {
				logGroupsDeleted++
			}
		}
		if !anyErr {
			printOK()
		}
	}

	// ── Step 9: Deregister AMI + delete snapshots ─────────────────────────────
	printStep(fmt.Sprintf("Deregister AMI + delete %d snapshot(s)...", len(inv.snapshotIDs)))
	if inv.amiID == "" {
		printWarn("no AMI found")
	} else {
		_, err := ec2Client.DeregisterImage(ctx, &ec2.DeregisterImageInput{
			ImageId: aws.String(inv.amiID),
		})
		if err != nil {
			printErr(fmt.Errorf("deregister %s: %w", inv.amiID, err))
		} else {
			anySnapErr := false
			for _, snapID := range inv.snapshotIDs {
				_, serr := ec2Client.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{
					SnapshotId: aws.String(snapID),
				})
				if serr != nil {
					printErr(fmt.Errorf("snapshot %s: %w", snapID, serr))
					anySnapErr = true
				} else {
					snapshotsDeleted++
				}
			}
			if !anySnapErr {
				printOK()
			}
		}
	}

	// ── Step 10: Delete Globus endpoint registration ──────────────────────────
	endpointDeleted := false
	printStep("Delete Globus endpoint registration...")
	if inv.endpointID == "" {
		printWarn("no endpoint ID found, skipping")
	} else if gcsErr != nil {
		printWarn(fmt.Sprintf("GCS API unavailable (%s); endpoint not deleted", gcsErr))
	} else {
		if err := gcsClient.DeleteEndpoint(ctx); err != nil {
			printErr(fmt.Errorf("endpoint %s: %w", inv.endpointID, err))
		} else {
			endpointDeleted = true
			printOK()
		}
	}

	// ── Final report ──────────────────────────────────────────────────────────
	fmt.Println()
	bold.Printf("Deprovisioned: %s\n", regionFlag)
	green.Printf("  ✓ %d GCS nodes deleted\n", gcsNodesDeleted)
	green.Printf("  ✓ %d collections deleted\n", collectionsDeleted)
	green.Printf("  ✓ %d storage gateways deleted\n", gatewaysDeleted)
	green.Printf("  ✓ %d CFN stacks deleted (EC2 instances, Lambdas, IAM roles)\n", cfnStacksDeleted)
	green.Printf("  ✓ %d secrets deleted\n", secretsDeleted)
	green.Printf("  ✓ %d SSM parameters deleted\n", ssmParamsDeleted)
	green.Printf("  ✓ %d log groups deleted\n", logGroupsDeleted)
	if inv.amiID != "" {
		green.Printf("  ✓ AMI %s deregistered, %d snapshot(s) deleted\n",
			truncID(inv.amiID, 21)+"...", snapshotsDeleted)
	}
	if endpointDeleted {
		green.Printf("  ✓ Globus endpoint %s deleted\n", inv.endpointID)
	} else if inv.endpointID != "" {
		fmt.Println()
		yellow.Printf("Note: The Globus endpoint %s was not deleted (see above).\n", inv.endpointID)
		yellow.Printf("To remove it manually, visit:\n")
		yellow.Printf("  https://app.globus.org/endpoints/%s/overview\n", inv.endpointID)
	}

	return nil
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
