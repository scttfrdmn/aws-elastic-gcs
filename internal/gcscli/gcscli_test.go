package gcscli

import (
	"context"
	"strings"
	"testing"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

func TestParseNodeList(t *testing.T) {
	out := `[{"code":"success","data":[{"id":"a","status":"active","ip_addresses":["1.2.3.4"]},{"id":"b","status":"inactive","ip_address":"5.6.7.8"}]},{"data":[{"id":"c","status":"inactive"}]}]`
	nodes, err := ParseNodeList(out)
	if err != nil || len(nodes) != 3 {
		t.Fatalf("nodes=%v err=%v", nodes, err)
	}
	if nodes[0].IP() != "1.2.3.4" || !nodes[0].Active() || nodes[1].IP() != "5.6.7.8" || nodes[1].Active() || nodes[2].IP() != "" {
		t.Fatalf("unexpected %+v", nodes)
	}
	if _, err := ParseNodeList("Error: You must log in"); err == nil {
		t.Fatal("want error for non-JSON output")
	}
}

func TestScript(t *testing.T) {
	dep, _ := names.New("us-west-2", "src")
	s := Script(dep, "globus-connect-server node list")
	for _, want := range []string{"elastic-gcs/us-west-2/src/globus-client-id", "/elastic-gcs/us-west-2/src/endpoint-id", "node list\nrc=$?", "exit $rc"} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q", want)
		}
	}
}

func TestDeleteNodeRejectsBadID(t *testing.T) {
	dep, _ := names.New("us-west-2", "src")
	if _, err := DeleteNode(context.Background(), nil, "i-1", dep, "abc; rm -rf /"); err == nil {
		t.Fatal("want validation error")
	}
}
