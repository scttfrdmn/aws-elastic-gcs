package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/spf13/cobra"

	"github.com/scttfrdmn/aws-elastic-gcs/config"
)

// endpointCmd groups endpoint-level settings.
func endpointCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "endpoint", Short: "Endpoint-level settings (network use)"}
	var regionsFile string
	var apply bool
	nu := &cobra.Command{
		Use:   "network-use",
		Short: "Show the endpoint's network use, and with --apply set it from regions.yaml (#56)",
		Long: `Globus gives each transfer task the lower of the two endpoints' maximum
concurrency. Left at the default ("normal", 4), this endpoint caps every task
at a fraction of one DTN. The values come from regions.yaml network_use, or
are derived from max_dtns (32 preferred / 64 max per DTN, capped at 128).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runNetworkUse(cmd.Context(), regionsFile, apply)
		},
	}
	nu.Flags().StringVar(&regionsFile, "regions", "config/regions.yaml", "path to regions.yaml")
	nu.Flags().BoolVar(&apply, "apply", false, "set the endpoint's network use to the configured values")
	cmd.AddCommand(nu)
	return cmd
}

func runNetworkUse(ctx context.Context, regionsFile string, apply bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	want, err := configuredNetworkUse(regionsFile)
	if err != nil {
		return err
	}
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return err
	}
	ssmClient := ssm.NewFromConfig(cfg)
	controlNodeID, err := getSSMParam(ctx, ssmClient, controlNodeIDParamPath())
	if err != nil {
		return fmt.Errorf("control node ID: %w", err)
	}
	show := func() error {
		out, err := runGCSCLIOnControlNode(ctx, ssmClient, controlNodeID, "globus-connect-server endpoint show -F json")
		if err != nil {
			return fmt.Errorf("endpoint show (is the control node running and the service account an admin?): %w", err)
		}
		var ep map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &ep); err != nil {
			return fmt.Errorf("parse endpoint show: %w", err)
		}
		for _, k := range []string{"network_use", "preferred_concurrency", "max_concurrency", "preferred_parallelism", "max_parallelism"} {
			fmt.Printf("  %-22s %v\n", k, ep[k])
		}
		return nil
	}
	fmt.Printf("Endpoint %s (%s), current:\n", dep.ID(), regionFlag)
	if err := show(); err != nil {
		return err
	}
	fmt.Printf("Configured: %s\n", want.Args())
	if !apply {
		fmt.Println("Run with --apply to set it.")
		return nil
	}
	if _, err := gcscliRun(ctx, ssmClient, controlNodeID, "globus-connect-server endpoint update "+want.Args()); err != nil {
		return fmt.Errorf("endpoint update: %w", err)
	}
	fmt.Println("Updated. Now:")
	return show()
}

// configuredNetworkUse resolves this deployment's network use from regions.yaml.
func configuredNetworkUse(regionsFile string) (config.NetworkUse, error) {
	regions, err := config.LoadRegions(regionsFile)
	if err != nil {
		return config.NetworkUse{}, err
	}
	for _, rc := range regions.Regions {
		d := rc.Deployment
		if d == "" {
			d = "default"
		}
		if rc.Name == dep.Region && d == dep.Name {
			r, err := config.Resolve(rc, config.Defaults{})
			if err != nil {
				return config.NetworkUse{}, err
			}
			return r.NetworkUse, nil
		}
	}
	return config.NetworkUse{}, fmt.Errorf("%s: no entry for %s", regionsFile, dep.ID())
}

// gcscliRun runs a GCS CLI command on the control node with a longer wait than
// the default (endpoint update can take a while).
func gcscliRun(ctx context.Context, ssmClient *ssm.Client, controlNodeID, command string) (string, error) {
	return runGCSCLIOnControlNodeWait(ctx, ssmClient, controlNodeID, command, 3*time.Minute)
}
