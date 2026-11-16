// aws-elastic-gcs-ctl is an operator CLI for managing Elastic GCS deployments.
// It provides a one-stop view of system state and commands for common operations,
// designed for live GlobusWorld demos and day-to-day operations.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/spf13/cobra"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/gcscli"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// toolVersion is set at release time by goreleaser (-X main.toolVersion=...).
var toolVersion = "dev"

var (
	regionFlag     string
	deploymentFlag string
	// dep is resolved from --region and --deployment before any command runs.
	dep         names.Deployment
	profileFlag string
	rootCmd     = &cobra.Command{
		Use:   "aws-elastic-gcs-ctl",
		Short: "Elastic GCS operator CLI",
		Long:  "aws-elastic-gcs-ctl — manage Elastic Globus Connect Server deployments on AWS",
		// --region and --deployment are interpolated into resource names and
		// into shell scripts run as root on the control node; names.New
		// validates both.
		PersistentPreRunE: func(*cobra.Command, []string) error {
			d, err := names.New(regionFlag, deploymentFlag)
			if err != nil {
				return err
			}
			dep = d
			return nil
		},
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
	rootCmd.PersistentFlags().StringVar(&deploymentFlag, "deployment", envOr("ELASTIC_GCS_DEPLOYMENT", names.DefaultDeployment),
		"deployment name within the region (regions.yaml `deployment`)")
	rootCmd.PersistentFlags().StringVar(&profileFlag, "profile", defaultProfile, "AWS profile")

	rootCmd.AddCommand(statusCmd())
	rootCmd.AddCommand(wakeCmd())
	rootCmd.AddCommand(scaleCmd())
	rootCmd.AddCommand(nodesCmd())
	rootCmd.AddCommand(logsCmd())
	rootCmd.AddCommand(metricsCmd())
	rootCmd.AddCommand(transferCmd())
	rootCmd.AddCommand(buildAMICmd())
	rootCmd.AddCommand(collectionsCmd())
	rootCmd.AddCommand(deprovisionCmd())
	rootCmd.AddCommand(endpointCmd())
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
	if out.Parameter == nil {
		return "", fmt.Errorf("SSM parameter %s: no value returned", name)
	}
	return aws.ToString(out.Parameter.Value), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func secretsPrefix() string          { return dep.SecretsPrefix() }
func controlNodeIDParamPath() string { return dep.Param(names.ParamControlNodeID) }
func endpointIDParamPath() string    { return dep.Param(names.ParamEndpointID) }

// runGCSCLIOnControlNode sends an SSM command to the control node that sets
// up GCS CLI auth from Secrets Manager and runs the given GCS command.
func runGCSCLIOnControlNode(ctx context.Context, ssmClient *ssm.Client, controlNodeID, gcsCmd string) (string, error) {
	return runGCSCLIOnControlNodeWait(ctx, ssmClient, controlNodeID, gcsCmd, 90*time.Second)
}

func runGCSCLIOnControlNodeWait(ctx context.Context, ssmClient *ssm.Client, controlNodeID, gcsCmd string, wait time.Duration) (string, error) {
	return gcscli.Run(ctx, ssmClient, controlNodeID, dep, gcsCmd, wait)
}

// listGCSNodes lists the endpoint's nodes via the control node.
func listGCSNodes(ctx context.Context, ssmClient *ssm.Client, controlNodeID string) ([]gcscli.Node, error) {
	return gcscli.ListNodes(ctx, ssmClient, controlNodeID, dep)
}

// instanceState returns an EC2 instance's state name, or "" when the API
// omitted State.
func instanceState(inst ec2types.Instance) string {
	if inst.State == nil {
		return ""
	}
	return string(inst.State.Name)
}

// lambdaFunctionBases maps the short Lambda names accepted by `logs` and
// reported by `metrics` to their function-name prefix. version-check, waker
// and idle-shutdown aren't deployed by the slots stack yet (#144, #130).
var lambdaFunctionBases = map[string]string{
	"slot-controller": "elastic-gcs-slot-controller",
	"demand-monitor":  "elastic-gcs-demand-monitor",
	"version-check":   "elastic-gcs-version-check",
	"waker":           "elastic-gcs-waker",
	"idle-shutdown":   "elastic-gcs-idle-shutdown",
}

func sortedLambdaNames() []string {
	names := make([]string, 0, len(lambdaFunctionBases))
	for k := range lambdaFunctionBases {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// parseDuration parses a --since value such as 30m, 1h or 2d.
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
		return time.Duration(val * float64(24*time.Hour)), nil
	default:
		return 0, fmt.Errorf("unknown duration unit %q (use m, h, d)", string(unit))
	}
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
