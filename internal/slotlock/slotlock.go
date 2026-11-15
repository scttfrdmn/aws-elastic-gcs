// Package slotlock releases DTN slot locks: the SSM parameters
// <param-prefix>/slot-lock/<N> whose value is the owning instance ID (#50).
package slotlock

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// API is the subset of the SSM client used here (satisfied by *ssm.Client).
type API interface {
	GetParameter(ctx context.Context, in *ssm.GetParameterInput, opts ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	DeleteParameter(ctx context.Context, in *ssm.DeleteParameterInput, opts ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
}

// Outcome of a Release.
type Outcome string

const (
	Released Outcome = "released"
	NotHeld  Outcome = "not-held"   // no lock for this slot
	OtherOwn Outcome = "other-owner" // the lock belongs to a different instance
)

// Param is the SSM parameter for a slot's lock.
func Param(dep names.Deployment, slot string) string {
	return dep.Param(names.ParamSlotLock + "/" + slot)
}

// Release deletes the slot's lock only if owner holds it. Deleting a lock owned
// by another instance would let a third instance claim a slot that is in use.
func Release(ctx context.Context, api API, dep names.Deployment, slot, owner string) (Outcome, error) {
	if slot == "" || owner == "" {
		return NotHeld, errors.New("slotlock: slot and owner are required")
	}
	name := Param(dep, slot)
	out, err := api.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
	if err != nil {
		var nf *ssmtypes.ParameterNotFound
		if errors.As(err, &nf) {
			return NotHeld, nil
		}
		return NotHeld, fmt.Errorf("slotlock: get %s: %w", name, err)
	}
	if got := aws.ToString(out.Parameter.Value); got != owner {
		return OtherOwn, nil
	}
	if _, err := api.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(name)}); err != nil {
		return NotHeld, fmt.Errorf("slotlock: delete %s: %w", name, err)
	}
	return Released, nil
}
