package stacks

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/config"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// TestSlotPoolStack synthesizes pool: slots and checks the shape: no ASG, a
// durable controller, and secret access only on the setup role (#42, #128).
func TestSlotPoolStack(t *testing.T) {
	wd, _ := os.Getwd()
	if err := os.Chdir(".."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	dep, _ := names.New("us-west-2", "tst")
	cfg, err := config.Resolve(config.RegionConfig{Name: "us-west-2", Deployment: "tst", Pool: "slots", MaxDTNs: 4}, config.Defaults{})
	if err != nil {
		t.Fatal(err)
	}
	app := awscdk.NewApp(nil)
	env := &awscdk.Environment{Account: jsii.String("123456789012"), Region: jsii.String("us-west-2")}
	ctl := NewControlNodeStack(app, "Ctl", &ControlNodeStackProps{StackProps: awscdk.StackProps{Env: env}, Deployment: dep, Config: cfg})
	stack := NewDTNPoolStack(app, "Pool", &DTNPoolStackProps{StackProps: awscdk.StackProps{Env: env}, Deployment: dep, Config: cfg, ControlStack: ctl})
	tpl := assertions.Template_FromStack(stack, nil)

	tpl.ResourceCountIs(jsii.String("AWS::AutoScaling::AutoScalingGroup"), jsii.Number(0))
	tpl.HasResourceProperties(jsii.String("AWS::Lambda::Function"), map[string]any{
		"FunctionName":  "elastic-gcs-slot-controller-us-west-2-tst",
		"DurableConfig": assertions.Match_ObjectLike(&map[string]any{"ExecutionTimeout": 3600}),
	})
	tpl.HasResourceProperties(jsii.String("AWS::IAM::InstanceProfile"), map[string]any{"InstanceProfileName": "elastic-gcs-dtn-runtime-us-west-2-tst"})
	tpl.HasResourceProperties(jsii.String("AWS::IAM::InstanceProfile"), map[string]any{"InstanceProfileName": "elastic-gcs-dtn-setup-us-west-2-tst"})

	// Slots launch with the named runtime profile the controller swaps back to.
	tpl.ResourceCountIs(jsii.String("AWS::IAM::InstanceProfile"), jsii.Number(2))
	tpl.HasResourceProperties(jsii.String("AWS::EC2::LaunchTemplate"), map[string]any{
		"LaunchTemplateData": assertions.Match_ObjectLike(&map[string]any{"IamInstanceProfile": map[string]any{
			"Arn": map[string]any{"Fn::GetAtt": []any{assertions.Match_StringLikeRegexp(jsii.String("SlotRuntimeProfile")), "Arn"}}}}),
	})

	// Durable functions reject unqualified invokes: both controller rules must
	// target the alias, and each needs an invoke permission on it.
	tpl.HasResourceProperties(jsii.String("AWS::Lambda::Alias"), map[string]any{"Name": "live"})
	tpl.ResourceCountIs(jsii.String("AWS::Lambda::Permission"), jsii.Number(3)) // 2 controller rules + demand monitor
	for _, rule := range []string{"elastic-gcs-slot-controller-us-west-2-tst", "elastic-gcs-slot-desired-us-west-2-tst"} {
		tpl.HasResourceProperties(jsii.String("AWS::Events::Rule"), map[string]any{
			"Name":    rule,
			"Targets": assertions.Match_ArrayWith(&[]any{assertions.Match_ObjectLike(&map[string]any{"Arn": map[string]any{"Ref": assertions.Match_StringLikeRegexp(jsii.String("SlotControllerLive"))}})}),
		})
	}

	// Secret access: setup role only.
	raw, _ := json.Marshal(tpl.ToJSON())
	var doc struct {
		Resources map[string]struct {
			Type       string
			Properties map[string]any
		}
	}
	_ = json.Unmarshal(raw, &doc)
	roleOf := map[string]string{} // policy -> role logical ID
	for id, r := range doc.Resources {
		if r.Type != "AWS::IAM::Policy" {
			continue
		}
		pol, _ := json.Marshal(r.Properties["PolicyDocument"])
		roles, _ := json.Marshal(r.Properties["Roles"])
		if strings.Contains(string(pol), "secretsmanager:GetSecretValue") {
			roleOf[id] = string(roles)
		}
	}
	for id, roles := range roleOf {
		if strings.Contains(roles, "SlotRuntimeRole") {
			t.Errorf("runtime role can read secrets via %s", id)
		}
	}
	found := false
	for _, roles := range roleOf {
		if strings.Contains(roles, "SlotSetupRole") {
			found = true
		}
	}
	if !found {
		t.Error("setup role has no secret access")
	}
}
