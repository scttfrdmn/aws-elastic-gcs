// Package warmpool restores the DTN warm pool after cache eviction removed it (#33).
package warmpool

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
)

// API is the subset of the Auto Scaling client used here.
type API interface {
	DescribeWarmPool(ctx context.Context, in *autoscaling.DescribeWarmPoolInput, opts ...func(*autoscaling.Options)) (*autoscaling.DescribeWarmPoolOutput, error)
	PutWarmPool(ctx context.Context, in *autoscaling.PutWarmPoolInput, opts ...func(*autoscaling.Options)) (*autoscaling.PutWarmPoolOutput, error)
}

// Ensure puts the warm pool back with size stopped instances if it doesn't
// exist. It matches the stack's WarmPoolCR. size <= 0 (Spot) does nothing.
// It reports whether it created the pool.
func Ensure(ctx context.Context, api API, asgName string, size int) (bool, error) {
	if size <= 0 {
		return false, nil
	}
	out, err := api.DescribeWarmPool(ctx, &autoscaling.DescribeWarmPoolInput{AutoScalingGroupName: aws.String(asgName)})
	if err != nil {
		return false, fmt.Errorf("DescribeWarmPool: %w", err)
	}
	if out.WarmPoolConfiguration != nil {
		return false, nil
	}
	_, err = api.PutWarmPool(ctx, &autoscaling.PutWarmPoolInput{
		AutoScalingGroupName:     aws.String(asgName),
		MinSize:                  aws.Int32(int32(size)),
		MaxGroupPreparedCapacity: aws.Int32(int32(size)),
		PoolState:                astypes.WarmPoolStateStopped,
		InstanceReusePolicy:      &astypes.InstanceReusePolicy{ReuseOnScaleIn: aws.Bool(true)},
	})
	if err != nil {
		return false, fmt.Errorf("PutWarmPool: %w", err)
	}
	return true, nil
}
