package identity

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type log struct{ events []string }

type fakeEC2 struct{ l *log }

func (f fakeEC2) DescribeIamInstanceProfileAssociations(context.Context, *ec2.DescribeIamInstanceProfileAssociationsInput, ...func(*ec2.Options)) (*ec2.DescribeIamInstanceProfileAssociationsOutput, error) {
	return &ec2.DescribeIamInstanceProfileAssociationsOutput{IamInstanceProfileAssociations: []ec2types.IamInstanceProfileAssociation{{AssociationId: aws.String("assoc-1")}}}, nil
}
func (f fakeEC2) ReplaceIamInstanceProfileAssociation(_ context.Context, in *ec2.ReplaceIamInstanceProfileAssociationInput, _ ...func(*ec2.Options)) (*ec2.ReplaceIamInstanceProfileAssociationOutput, error) {
	f.l.events = append(f.l.events, "swap:"+aws.ToString(in.IamInstanceProfile.Name))
	return &ec2.ReplaceIamInstanceProfileAssociationOutput{}, nil
}

type fakeIAM struct{ l *log }

func (f fakeIAM) PutRolePolicy(_ context.Context, in *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	f.l.events = append(f.l.events, "revoke:"+aws.ToString(in.RoleName))
	return &iam.PutRolePolicyOutput{}, nil
}

// fakeSSM fails the command whose text contains failOn.
type fakeSSM struct {
	l      *log
	failOn string
	last   string
}

func (f *fakeSSM) SendCommand(_ context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	f.last = in.Parameters["commands"][0]
	kind := "probe"
	if strings.Contains(f.last, "node setup") {
		kind = "setup"
	}
	f.l.events = append(f.l.events, "ssm:"+kind)
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String("c")}}, nil
}
func (f *fakeSSM) GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	st := ssmtypes.CommandInvocationStatusSuccess
	if f.failOn != "" && strings.Contains(f.last, f.failOn) {
		st = ssmtypes.CommandInvocationStatusFailed
	}
	return &ssm.GetCommandInvocationOutput{Status: st, StandardOutputContent: aws.String("ok")}, nil
}

func spec() Spec {
	return Spec{InstanceID: "i-1", SetupProfile: "setup", RuntimeProfile: "runtime", SetupRole: "setup-role",
		ProbeScript: "aws ssm get-parameter --name x", SetupScript: "globus-connect-server node setup ...",
		ProbeWait: time.Second, SetupWait: 5 * time.Second}
}

func TestSetupOrder(t *testing.T) {
	l := &log{}
	_, err := Setup(context.Background(), Deps{EC2: fakeEC2{l}, IAM: fakeIAM{l}, SSM: &fakeSSM{l: l}}, spec())
	if err != nil {
		t.Fatal(err)
	}
	want := "swap:setup ssm:probe ssm:setup swap:runtime revoke:setup-role"
	if got := strings.Join(l.events, " "); got != want {
		t.Fatalf("order\n got %s\nwant %s", got, want)
	}
}

func TestSetupFailureStillSwapsBackAndRevokes(t *testing.T) {
	l := &log{}
	_, err := Setup(context.Background(), Deps{EC2: fakeEC2{l}, IAM: fakeIAM{l}, SSM: &fakeSSM{l: l, failOn: "node setup"}}, spec())
	if err == nil {
		t.Fatal("want setup error")
	}
	got := strings.Join(l.events, " ")
	if !strings.HasSuffix(got, "swap:runtime revoke:setup-role") {
		t.Fatalf("cleanup missing: %s", got)
	}
}

func TestProbeFailureSkipsSetup(t *testing.T) {
	l := &log{}
	_, err := Setup(context.Background(), Deps{EC2: fakeEC2{l}, IAM: fakeIAM{l}, SSM: &fakeSSM{l: l, failOn: "get-parameter"}}, spec())
	if err == nil || strings.Contains(strings.Join(l.events, " "), "ssm:setup") {
		t.Fatalf("err=%v events=%v", err, l.events)
	}
}

func TestRevokePolicy(t *testing.T) {
	p := RevokePolicy(time.Date(2026, 11, 15, 8, 4, 36, 0, time.UTC))
	for _, want := range []string{`"Effect":"Deny"`, `"aws:TokenIssueTime":"2026-11-15T08:04:36Z"`, `"DateLessThan"`} {
		if !strings.Contains(p, want) {
			t.Errorf("policy missing %s: %s", want, p)
		}
	}
}
