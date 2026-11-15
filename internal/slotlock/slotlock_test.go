package slotlock

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

type fake struct {
	params  map[string]string
	deleted []string
}

func (f *fake) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	v, ok := f.params[aws.ToString(in.Name)]
	if !ok {
		return nil, &ssmtypes.ParameterNotFound{}
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(v)}}, nil
}

func (f *fake) DeleteParameter(_ context.Context, in *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	f.deleted = append(f.deleted, aws.ToString(in.Name))
	delete(f.params, aws.ToString(in.Name))
	return &ssm.DeleteParameterOutput{}, nil
}

func TestRelease(t *testing.T) {
	dep, _ := names.New("us-west-2", "src")
	f := &fake{params: map[string]string{Param(dep, "3"): "i-aaa", Param(dep, "4"): "i-bbb"}}
	cases := []struct {
		slot, owner string
		want        Outcome
	}{
		{"3", "i-aaa", Released},
		{"4", "i-aaa", OtherOwn},
		{"9", "i-aaa", NotHeld},
	}
	for _, c := range cases {
		got, err := Release(context.Background(), f, dep, c.slot, c.owner)
		if err != nil || got != c.want {
			t.Errorf("Release(%s,%s) = %v, %v; want %v", c.slot, c.owner, got, err, c.want)
		}
	}
	if len(f.deleted) != 1 || f.deleted[0] != "/elastic-gcs/us-west-2/src/slot-lock/3" {
		t.Fatalf("deleted %v", f.deleted)
	}
	if _, err := Release(context.Background(), f, dep, "", "i-aaa"); err == nil {
		t.Fatal("want error for empty slot")
	}
}
