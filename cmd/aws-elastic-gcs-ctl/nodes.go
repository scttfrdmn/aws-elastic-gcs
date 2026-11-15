package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/gcsid"
)

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

	nodes, err := listGCSNodes(ctx, ssmClient, controlNodeID)
	if err != nil {
		return fmt.Errorf("node list failed: %w", err)
	}

	fmt.Printf("%-36s  %-18s  %-10s\n", "NODE ID", "IP ADDRESS", "STATUS")
	fmt.Println(strings.Repeat("─", 68))
	for _, n := range nodes {
		if n.Status == "active" {
			color.Green("%-36s  %-18s  %s\n", n.ID, n.IP(), n.Status)
		} else {
			color.New(color.Faint).Printf("%-36s  %-18s  %s\n", n.ID, n.IP(), n.Status)
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
		nodes, err := listGCSNodes(ctx, ssmClient, controlNodeID)
		if err != nil {
			return fmt.Errorf("node list for ID resolution: %w", err)
		}
		for _, n := range nodes {
			if strings.HasPrefix(n.ID, nodeID) {
				fullID = n.ID
				break
			}
		}
		if fullID == nodeID {
			return fmt.Errorf("no node found with ID prefix %q", nodeID)
		}
	}

	if err := gcsid.Validate(fullID); err != nil {
		return err
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

	fnName := dep.Resource("version-check")
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
		Msg      string            `json:"msg"`
		Versions map[string]string `json:"versions"`
		Distinct int               `json:"distinct_versions"`
		Drift    int               `json:"drift_count"`
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
