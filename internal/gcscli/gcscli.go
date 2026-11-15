// Package gcscli runs Globus Connect Server CLI commands on a node over SSM,
// authenticated as the deployment's service account (#51).
//
// The credential preamble reads the client ID/secret from Secrets Manager and
// the endpoint ID from SSM on the target node itself, so secrets never pass
// through SSM command parameters or Lambda logs.
package gcscli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/gcsid"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/ssmexec"
)

// LockFix hands the subscription_pause lock back to gcsweb after a root GCS
// CLI call; without it the gcsweb-run manager API breaks.
const LockFix = "chown gcsweb:gcsweb /var/lib/globus-connect-server/gcs-manager/locks/subscription_pause 2>/dev/null || true"

// Preamble returns shell that exports GCS_CLI_CLIENT_ID, GCS_CLI_CLIENT_SECRET
// and GCS_CLI_ENDPOINT_ID for dep.
func Preamble(dep names.Deployment) string {
	return fmt.Sprintf(`export GCS_CLI_CLIENT_ID=$(aws secretsmanager get-secret-value --secret-id %[1]s/globus-client-id --query SecretString --output text --region %[3]s 2>/dev/null)
export GCS_CLI_CLIENT_SECRET=$(aws secretsmanager get-secret-value --secret-id %[1]s/globus-client-secret --query SecretString --output text --region %[3]s 2>/dev/null)
export GCS_CLI_ENDPOINT_ID=$(aws ssm get-parameter --name %[2]s --query Parameter.Value --output text --region %[3]s 2>/dev/null)
`, dep.SecretsPrefix(), dep.Param(names.ParamEndpointID), dep.Region)
}

// Script wraps a GCS CLI command with the credential preamble and LockFix.
// The command's exit status is the script's exit status.
func Script(dep names.Deployment, command string) string {
	return Preamble(dep) + command + "\nrc=$?\n" + LockFix + "\nexit $rc\n"
}

// Run executes command on instanceID and returns its stdout. A non-zero exit is
// an error (stdout is still returned).
func Run(ctx context.Context, api ssmexec.API, instanceID string, dep names.Deployment, command string, wait time.Duration) (string, error) {
	res, err := ssmexec.RunOK(ctx, api, instanceID, Script(dep, command),
		ssmexec.Options{Wait: wait, Comment: "elastic-gcs: GCS CLI"})
	return res.Stdout, err
}

// Node is one entry of `globus-connect-server node list -F json`.
type Node struct {
	ID          string   `json:"id"`
	Status      string   `json:"status"`
	IPAddresses []string `json:"ip_addresses"`
	IPAddress   string   `json:"ip_address"` // older output
}

// IP returns the node's first IP address, or "".
func (n Node) IP() string {
	if len(n.IPAddresses) > 0 {
		return n.IPAddresses[0]
	}
	return n.IPAddress
}

// Active reports whether the node is serving.
func (n Node) Active() bool { return n.Status == "active" }

// ParseNodeList parses `node list -F json` output: a list of result pages,
// each with a data array.
func ParseNodeList(out string) ([]Node, error) {
	var pages []struct {
		Data []Node `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &pages); err != nil {
		return nil, fmt.Errorf("parse node list: %w (output: %.200s)", err, out)
	}
	var nodes []Node
	for _, p := range pages {
		nodes = append(nodes, p.Data...)
	}
	return nodes, nil
}

// ListNodes lists the endpoint's nodes by running the CLI on instanceID.
func ListNodes(ctx context.Context, api ssmexec.API, instanceID string, dep names.Deployment) ([]Node, error) {
	out, err := Run(ctx, api, instanceID, dep, "globus-connect-server node list -F json", 90*time.Second)
	if err != nil {
		return nil, err
	}
	return ParseNodeList(out)
}

// DeleteNode removes nodeID from the endpoint. The ID is validated as a UUID
// before it reaches a root shell.
func DeleteNode(ctx context.Context, api ssmexec.API, instanceID string, dep names.Deployment, nodeID string) (string, error) {
	if err := gcsid.Validate(nodeID); err != nil {
		return "", err
	}
	return Run(ctx, api, instanceID, dep, "globus-connect-server node delete "+nodeID, 90*time.Second)
}
