package warmpool

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
)

type fake struct {
	exists bool
	put    *autoscaling.PutWarmPoolInput
}

func (f *fake) DescribeWarmPool(context.Context, *autoscaling.DescribeWarmPoolInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeWarmPoolOutput, error) {
	out := &autoscaling.DescribeWarmPoolOutput{}
	if f.exists {
		out.WarmPoolConfiguration = &astypes.WarmPoolConfiguration{}
	}
	return out, nil
}

func (f *fake) PutWarmPool(_ context.Context, in *autoscaling.PutWarmPoolInput, _ ...func(*autoscaling.Options)) (*autoscaling.PutWarmPoolOutput, error) {
	f.put = in
	return &autoscaling.PutWarmPoolOutput{}, nil
}

func TestEnsure(t *testing.T) {
	f := &fake{}
	if made, err := Ensure(context.Background(), f, "asg", 4); !made || err != nil || aws.ToInt32(f.put.MaxGroupPreparedCapacity) != 4 || f.put.PoolState != astypes.WarmPoolStateStopped {
		t.Fatalf("made=%v err=%v put=%+v", made, err, f.put)
	}
	f = &fake{exists: true}
	if made, _ := Ensure(context.Background(), f, "asg", 4); made || f.put != nil {
		t.Fatal("must not touch an existing pool")
	}
	f = &fake{}
	if made, _ := Ensure(context.Background(), f, "asg", 0); made || f.put != nil {
		t.Fatal("spot: no warm pool")
	}
}
