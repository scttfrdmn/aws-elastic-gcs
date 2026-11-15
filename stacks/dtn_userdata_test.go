package stacks

import (
	"os"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/config"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// maxTokenLen bounds what one unresolved token (an asset bucket name or key)
// renders to: S3 bucket names are at most 63 characters, asset keys ~70.
const maxTokenLen = 128

// renderedLen is an upper bound on the length of a resolved CloudFormation
// value: literal strings count in full, each intrinsic counts maxTokenLen.
func renderedLen(v interface{}) int {
	switch t := v.(type) {
	case string:
		return len(t)
	case []interface{}:
		n := 0
		for _, e := range t {
			n += renderedLen(e)
		}
		return n
	case map[string]interface{}:
		if parts, ok := t["Fn::Join"].([]interface{}); ok && len(parts) == 2 {
			return renderedLen(parts[1])
		}
		return maxTokenLen
	default:
		return maxTokenLen
	}
}

// EC2 rejects user data over 16 KB (before base64). The scripts are S3 assets
// so this holds however large they grow.
func TestDTNUserDataUnder16KB(t *testing.T) {
	// Asset paths (scripts/userdata/...) are relative to the repo root.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(".."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	dep, err := names.New("us-east-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("UserDataTest"), &awscdk.StackProps{
		Env: &awscdk.Environment{Account: jsii.String("123456789012"), Region: jsii.String(dep.Region)},
	})
	p := &dtnPool{
		stack:         stack,
		dep:           dep,
		cfg:           config.Resolved{MaxDTNs: 64, WarmPoolSize: 64, TotalSlots: 128},
		secretsPrefix: dep.SecretsPrefix(),
		asgName:       dep.Resource("dtn"),
	}
	role := awsiam.NewRole(stack, jsii.String("Role"), &awsiam.RoleProps{
		AssumedBy: awsiam.NewServicePrincipal(jsii.String("ec2.amazonaws.com"), nil),
	})
	ud := p.dtnUserData(role)

	n := renderedLen(stack.Resolve(ud.Render()))
	t.Logf("DTN UserData: at most %d bytes", n)
	if n > 16*1024 {
		t.Fatalf("DTN UserData is up to %d bytes; EC2 allows 16384", n)
	}
	if n < 1000 {
		t.Fatalf("DTN UserData is only %d bytes; the size check is not measuring it", n)
	}
}
