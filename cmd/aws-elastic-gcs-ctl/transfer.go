package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"
)

// Well-known collection and endpoint IDs for test transfers.
const (
	collection1000Genomes = "28daf55e-07a4-4a65-9f54-292b0b4b8739"
	tutorialEndpoint2     = "31ce9ba0-176d-45a5-add3-f37d233ba47d"
	testTransferFile      = "/1000genomes/release/20130502/20140625_related_individuals.txt"
	testTransferDest      = "/~/elastic-gcs-test.txt"
)

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
