package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

func buildAMICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "build-ami",
		Short: "Build a new DTN AMI (runs scripts/build_ami.sh)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("Building DTN AMI for %s...\n", dep.ID())
			c := exec.Command("bash", "scripts/build_ami.sh", dep.Region, dep.Name)
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
