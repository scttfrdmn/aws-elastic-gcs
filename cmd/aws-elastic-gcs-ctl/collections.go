package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/scttfrdmn/aws-elastic-gcs/config"
)

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
	buckets := cfg.BucketsFor(dep.Region, dep.Name)
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

	// Marshal bucket list as JSON for embedding in the SSM shell script. It is
	// base64-encoded because bucket and collection names can contain quotes
	// and the script runs as root on the control node.
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
BUCKETS_JSON=$(echo '%s' | base64 -d)
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
`, base64.StdEncoding.EncodeToString(bucketsJSON), regionFlag)

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
