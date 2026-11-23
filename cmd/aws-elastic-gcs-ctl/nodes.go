package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/gcsid"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/ssmexec"
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
	var expect string
	cmd := &cobra.Command{
		Use:   "version-check",
		Short: "Check that the control node and every running DTN run the same GCS version",
		// Drift is a result, not a usage error.
		SilenceUsage: true,
		Long: `All nodes in an endpoint must run the same GCS version. Reads the installed
globus-connect-server54 package version over SSM on the control node and on
every running slot DTN, and reports any drift. The expected version comes
from --expect, else from cdk.json (globus:gcsVersion) when run in the repo.
Exits non-zero on drift, so it can gate a release or an upgrade.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNodesVersionCheck(cmd.Context(), expect)
		},
	}
	cmd.Flags().StringVar(&expect, "expect", "", "Expected GCS version, e.g. 5.4.101 (default: cdk.json globus:gcsVersion if present)")
	return cmd
}

// versionReport compares per-node versions against an expected version (if
// any) and against each other. It returns the nodes that drift and whether
// all nodes agree.
func versionReport(versions map[string]string, expect string) (drift []string, agree bool) {
	seen := map[string]bool{}
	for id, v := range versions {
		seen[v] = true
		if v == "" || v == "unknown" || (expect != "" && v != expect) {
			drift = append(drift, id)
		}
	}
	sort.Strings(drift)
	return drift, len(seen) <= 1 && len(drift) == 0
}

// expectedGCSVersion reads globus:gcsVersion from cdk.json in the current
// directory or a parent, if there is one.
func expectedGCSVersion() string {
	dir, _ := os.Getwd()
	for i := 0; i < 6 && dir != ""; i++ {
		b, err := os.ReadFile(filepath.Join(dir, "cdk.json"))
		if err == nil {
			var c struct {
				Context map[string]any `json:"context"`
			}
			if json.Unmarshal(b, &c) == nil {
				if v, ok := c.Context["globus:gcsVersion"].(string); ok {
					return v
				}
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func runNodesVersionCheck(ctx context.Context, expect string) error {
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}
	ssmc, ec2c := ssm.NewFromConfig(cfg), ec2.NewFromConfig(cfg)
	if expect == "" {
		expect = expectedGCSVersion()
	}
	nodes := map[string]string{} // instance ID -> role label
	if id, err := getSSMParam(ctx, ssmc, controlNodeIDParamPath()); err == nil {
		nodes[id] = "control"
	}
	slots, err := listSlotInstances(ctx, ec2c)
	if err != nil {
		return err
	}
	for _, s := range slots {
		if s.State == "running" {
			nodes[s.ID] = fmt.Sprintf("slot %d", s.Slot)
		}
	}
	versions := map[string]string{}
	for id, role := range nodes {
		res, err := ssmexec.RunOK(ctx, ssmc, id, "rpm -q --qf '%{VERSION}' globus-connect-server54",
			ssmexec.Options{Wait: 60 * time.Second, Comment: "elastic-gcs: version check"})
		v := strings.TrimSpace(res.Stdout)
		if err != nil || v == "" {
			v = "unknown"
			if err != nil {
				color.Yellow("  %s (%s): %v\n", id, role, err)
			}
		}
		versions[id] = v
	}
	if len(versions) == 0 {
		return fmt.Errorf("no running nodes to check (control node and DTNs are stopped; try 'wake')")
	}

	fmt.Printf("  %-22s  %-9s  %s\n", "Instance", "Role", "GCS version")
	ids := make([]string, 0, len(versions))
	for id := range versions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Printf("  %-22s  %-9s  %s\n", id, nodes[id], versions[id])
	}
	fmt.Println()
	drift, agree := versionReport(versions, expect)
	switch {
	case agree:
		if expect != "" {
			color.Green("All %d running nodes run GCS %s, as expected.\n", len(versions), expect)
		} else {
			color.Green("All %d running nodes run the same GCS version (no expected version given).\n", len(versions))
		}
		return nil
	default:
		color.Red("GCS version drift on %d node(s): %s (expected %q)\n", len(drift), strings.Join(drift, ", "), expect)
		return fmt.Errorf("GCS version drift")
	}
}
