// Package identity delivers a slot's GCS identity to its instance exactly once
// (#42, #127). A DTN's runtime role can't read secrets; for `node setup` the
// controller swaps the instance to a setup role that can, runs setup over SSM,
// swaps back, and revokes every setup-role session issued before the swap-back,
// so credentials copied during setup stop working (verified live: about 5 s).
//
// Revoking "every session issued before now" is safe only because slots are
// revived one at a time (internal/slots).
package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/ssmexec"
)

// EC2 is the subset of the EC2 client used here.
type EC2 interface {
	DescribeIamInstanceProfileAssociations(ctx context.Context, in *ec2.DescribeIamInstanceProfileAssociationsInput, opts ...func(*ec2.Options)) (*ec2.DescribeIamInstanceProfileAssociationsOutput, error)
	ReplaceIamInstanceProfileAssociation(ctx context.Context, in *ec2.ReplaceIamInstanceProfileAssociationInput, opts ...func(*ec2.Options)) (*ec2.ReplaceIamInstanceProfileAssociationOutput, error)
}

// IAM is the subset of the IAM client used here.
type IAM interface {
	PutRolePolicy(ctx context.Context, in *iam.PutRolePolicyInput, opts ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
}

// Deps are the AWS clients Setup uses.
type Deps struct {
	EC2 EC2
	IAM IAM
	SSM ssmexec.API
	Now func() time.Time
}

// Spec describes one setup.
type Spec struct {
	InstanceID     string
	SetupProfile   string // instance profile name with secret access
	RuntimeProfile string // instance profile name without it
	SetupRole      string // role in SetupProfile, whose sessions get revoked
	// ProbeScript exits 0 once the instance's credentials can read the
	// identity (i.e. the setup role is active), e.g. an `aws ssm get-parameter`.
	ProbeScript string
	// SetupScript runs `node setup` on the node, reading secrets there.
	SetupScript string
	ProbeWait   time.Duration // default 2m
	SetupWait   time.Duration // default 15m
}

// RevokePolicyName is the inline policy on the setup role holding the cutoff.
const RevokePolicyName = "elastic-gcs-revoke-older-sessions"

// RevokePolicy denies every action to setup-role sessions issued before cutoff.
func RevokePolicy(cutoff time.Time) string {
	doc := map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect":    "Deny",
			"Action":    "*",
			"Resource":  "*",
			"Condition": map[string]any{"DateLessThan": map[string]any{"aws:TokenIssueTime": cutoff.UTC().Format(time.RFC3339)}},
		}},
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// Setup runs the one-time identity delivery for a slot. Whatever happens after
// the swap to the setup profile, the instance is swapped back and setup-role
// sessions are revoked before Setup returns. The setup output is returned for
// logging.
func Setup(ctx context.Context, d Deps, s Spec) (out string, err error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if s.ProbeWait <= 0 {
		s.ProbeWait = 2 * time.Minute
	}
	if s.SetupWait <= 0 {
		s.SetupWait = 15 * time.Minute
	}
	assoc, err := association(ctx, d.EC2, s.InstanceID)
	if err != nil {
		return "", err
	}
	if err := swap(ctx, d.EC2, assoc, s.SetupProfile); err != nil {
		return "", fmt.Errorf("swap to setup profile: %w", err)
	}
	defer func() {
		// Always back to runtime and revoke, even if setup failed. Use a fresh
		// context so a cancelled ctx can't leave the setup role attached.
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		a, aerr := association(cctx, d.EC2, s.InstanceID)
		if aerr == nil {
			aerr = swap(cctx, d.EC2, a, s.RuntimeProfile)
		}
		_, rerr := d.IAM.PutRolePolicy(cctx, &iam.PutRolePolicyInput{
			RoleName:       aws.String(s.SetupRole),
			PolicyName:     aws.String(RevokePolicyName),
			PolicyDocument: aws.String(RevokePolicy(d.Now())),
		})
		if aerr != nil || rerr != nil {
			err = fmt.Errorf("identity cleanup (swap back: %v, revoke: %v); setup: %w", aerr, rerr, err)
		}
	}()

	if _, err := ssmexec.RunOK(ctx, d.SSM, s.InstanceID,
		fmt.Sprintf("for i in $(seq 1 %d); do (%s) >/dev/null 2>&1 && exit 0; sleep 3; done; exit 1", int(s.ProbeWait.Seconds()/3)+1, s.ProbeScript),
		ssmexec.Options{Wait: s.ProbeWait + 30*time.Second, Comment: "elastic-gcs: wait for setup credentials"}); err != nil {
		return "", fmt.Errorf("setup credentials never became active: %w", err)
	}
	res, err := ssmexec.RunOK(ctx, d.SSM, s.InstanceID, s.SetupScript,
		ssmexec.Options{Wait: s.SetupWait, Comment: "elastic-gcs: node setup"})
	if err != nil {
		return res.Stdout, fmt.Errorf("node setup: %w", err)
	}
	return res.Stdout, nil
}

func association(ctx context.Context, c EC2, instanceID string) (string, error) {
	out, err := c.DescribeIamInstanceProfileAssociations(ctx, &ec2.DescribeIamInstanceProfileAssociationsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("instance-id"), Values: []string{instanceID}},
			{Name: aws.String("state"), Values: []string{"associated", "associating"}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("describe profile association: %w", err)
	}
	if len(out.IamInstanceProfileAssociations) == 0 {
		return "", fmt.Errorf("instance %s has no instance profile", instanceID)
	}
	return aws.ToString(out.IamInstanceProfileAssociations[0].AssociationId), nil
}

func swap(ctx context.Context, c EC2, associationID, profile string) error {
	_, err := c.ReplaceIamInstanceProfileAssociation(ctx, &ec2.ReplaceIamInstanceProfileAssociationInput{
		AssociationId:      aws.String(associationID),
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{Name: aws.String(profile)},
	})
	return err
}
