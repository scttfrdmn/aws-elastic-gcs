package slotctl

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// ErrLocked means another controller execution holds the lock.
var ErrLocked = errors.New("another controller execution holds the lock")

// Acquire takes the deployment's controller lock for owner. A lock older than
// stale is considered abandoned (its execution died) and is taken over. The
// value is "owner|RFC3339".
func (c *Controller) Acquire(ctx context.Context, owner string, stale time.Duration) error {
	name := c.Env.Dep.Param(names.ParamControllerLock)
	val := owner + "|" + c.now().Format(time.RFC3339)
	for attempt := 0; attempt < 2; attempt++ {
		_, err := c.SSM.PutParameter(ctx, &ssm.PutParameterInput{Name: aws.String(name), Value: aws.String(val), Type: ssmtypes.ParameterTypeString})
		if err == nil {
			return nil
		}
		var exists *ssmtypes.ParameterAlreadyExists
		if !errors.As(err, &exists) {
			return err
		}
		cur, err := c.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
		if err != nil || cur.Parameter == nil {
			continue
		}
		holder, at, _ := strings.Cut(aws.ToString(cur.Parameter.Value), "|")
		if holder == owner {
			return nil // re-entrant: a replayed execution
		}
		t, perr := time.Parse(time.RFC3339, at)
		if perr == nil && c.now().Sub(t) < stale {
			return ErrLocked
		}
		_, _ = c.SSM.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(name)}) // abandoned
	}
	return ErrLocked
}

// Release drops the lock if owner holds it.
func (c *Controller) Release(ctx context.Context, owner string) {
	name := c.Env.Dep.Param(names.ParamControllerLock)
	cur, err := c.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
	if err != nil || cur.Parameter == nil {
		return
	}
	if holder, _, _ := strings.Cut(aws.ToString(cur.Parameter.Value), "|"); holder == owner {
		_, _ = c.SSM.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(name)})
	}
}

// Desired reads desired-dtns (0 if unset or invalid).
func (c *Controller) Desired(ctx context.Context) int {
	v := c.param(ctx, names.ParamDesiredDTNs)
	n := 0
	for _, r := range v {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
