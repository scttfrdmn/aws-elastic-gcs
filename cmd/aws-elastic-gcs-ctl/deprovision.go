package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cloudformationtypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/gcsid"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

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
	// DTN slots: instances and Elastic IPs owned by the slot controller, not by
	// CloudFormation, so stack deletion doesn't remove them.
	slots       []slotInstance
	slotEIPs    []string // allocation IDs
	slotEIPAddr []string // public IPs, for display
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
specified region — DTN slot instances and their Elastic IPs (owned by the slot
controller, not CloudFormation), Globus endpoint nodes/collections/gateways,
CloudFormation stacks, Secrets Manager secrets, SSM parameters, CloudWatch log
groups, and the DTN AMI with its EBS snapshots.

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

	fmt.Printf("Collecting inventory for %s...\n", dep.ID())
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
			inv.controlState = instanceState(instOut.Reservations[0].Instances[0])
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
			nodes, _ := listGCSNodes(ctx, ssmClient, inv.controlNodeID)
			for _, n := range nodes {
				inv.gcsNodeIDs = append(inv.gcsNodeIDs, n.ID)
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
			type gw struct {
				ID string `json:"id"`
			}
			type gwPage struct {
				Data []gw `json:"data"`
			}
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

	// ── DTN slots (controller-owned instances and Elastic IPs) ────────────────
	slots, serr := listSlotInstances(ctx, ec2Client)
	if serr != nil {
		return nil, serr
	}
	inv.slots = slots
	addrs, aerr := ec2Client.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{Filters: slotFilters()})
	if aerr != nil {
		return nil, fmt.Errorf("describe slot Elastic IPs: %w", aerr)
	}
	for _, a := range addrs.Addresses {
		inv.slotEIPs = append(inv.slotEIPs, aws.ToString(a.AllocationId))
		inv.slotEIPAddr = append(inv.slotEIPAddr, aws.ToString(a.PublicIp))
	}

	// ── CloudFormation stacks ────────────────────────────────────────────────
	// DTN stack first (depends on control stack), so delete it first.
	for _, stackName := range []string{
		dep.Stack("DTNPool"),
		dep.Stack("Control"),
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
			Path:      aws.String(dep.ParamPrefix()),
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
			if strings.HasSuffix(name, "-"+dep.ID()) {
				inv.logGroupNames = append(inv.logGroupNames, name)
			}
		}
		if lgOut.NextToken == nil {
			break
		}
		lgToken = lgOut.NextToken
	}

	// ── DTN AMI ───────────────────────────────────────────────────────────────
	// The DTN AMI is shared by every deployment in the region on the same GCS
	// version (names.AMIParam), so deprovisioning one deployment leaves it.
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

	bold.Printf("\nElastic GCS — %s deprovision dry run\n", dep.ID())
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
	for _, s := range inv.slots {
		fmt.Printf("                 %s (DTN slot %d, %s)\n", s.ID, s.Slot, s.State)
		hasInstances = true
	}
	if !hasInstances {
		fmt.Printf("  EC2 instances  (none found)\n")
	}
	if len(inv.slotEIPs) > 0 {
		fmt.Printf("  Elastic IPs    %d: %s (DTN slots)\n", len(inv.slotEIPs), strings.Join(inv.slotEIPAddr, "  "))
	} else {
		fmt.Printf("  Elastic IPs    (no DTN slot addresses)\n")
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
		fmt.Printf("  SSM params     %d: %s/*\n", len(inv.ssmParamNames), dep.ParamPrefix())
	} else {
		fmt.Printf("  SSM params     (none found)\n")
	}

	// Log groups
	if len(inv.logGroupNames) > 0 {
		fmt.Printf("  Log groups     %d: /aws/lambda/elastic-gcs-*-%s\n", len(inv.logGroupNames), dep.ID())
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

	// ── Step 1: Remove DTN slots ──────────────────────────────────────────────
	// Slot instances and Elastic IPs belong to the slot controller, not to
	// CloudFormation, and slot instances use the control stack's security group,
	// so they must go before the stacks. desired-dtns=0 first, so the controller
	// doesn't revive a slot while this runs.
	printStep(fmt.Sprintf("Remove %d DTN slot instance(s) and %d slot Elastic IP(s)...", len(inv.slots), len(inv.slotEIPs)))
	if len(inv.slots) == 0 && len(inv.slotEIPs) == 0 {
		printWarn("no DTN slots found")
	} else {
		fmt.Printf("\n")
		if _, err := ssmClient.PutParameter(ctx, &ssm.PutParameterInput{
			Name: aws.String(dep.Param(names.ParamDesiredDTNs)), Value: aws.String("0"),
			Type: ssmtypes.ParameterTypeString, Overwrite: aws.Bool(true),
		}); err != nil {
			printErr(fmt.Errorf("set desired-dtns=0: %w", err))
		}
		var ids []string
		for _, s := range inv.slots {
			ids = append(ids, s.ID)
		}
		if len(ids) > 0 {
			if _, err := ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids}); err != nil {
				printErr(fmt.Errorf("terminate slot instances: %w", err))
			} else if err := ec2.NewInstanceTerminatedWaiter(ec2Client).Wait(ctx,
				&ec2.DescribeInstancesInput{InstanceIds: ids}, 10*time.Minute); err != nil {
				printErr(fmt.Errorf("wait for slot instances to terminate: %w", err))
			} else {
				green.Printf("  ✓ %d slot instance(s) terminated\n", len(ids))
			}
		}
		released := 0
		for i, alloc := range inv.slotEIPs {
			if _, err := ec2Client.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: aws.String(alloc)}); err != nil {
				printErr(fmt.Errorf("release %s: %w", inv.slotEIPAddr[i], err))
			} else {
				released++
			}
		}
		if len(inv.slotEIPs) > 0 {
			green.Printf("  ✓ %d slot Elastic IP(s) released\n", released)
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
			if err := gcsid.Validate(nodeID); err != nil {
				printErr(err)
				anyErr = true
				continue
			}
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
			if err := gcsid.Validate(colID); err != nil {
				printErr(err)
				anyErr = true
				continue
			}
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
			if err := gcsid.Validate(gwID); err != nil {
				printErr(err)
				anyErr = true
				continue
			}
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
	bold.Printf("Deprovisioned: %s\n", dep.ID())
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
