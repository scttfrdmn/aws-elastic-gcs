package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// Slot-controller views for the CLI (#65). The controller owns slot instances
// and Elastic IPs (elastic-gcs:pool=slots); the CLI reads them and sets
// desired-dtns. Richer slot commands (list, rebuild, evict) are #131.

const (
	tagPool  = "elastic-gcs:pool"
	tagPhase = "elastic-gcs:phase"
)

// slotInstance is one live slot instance.
type slotInstance struct {
	ID, State, Phase, IP string
	Slot                 int
}

// slotFilters select this deployment's controller-owned resources.
func slotFilters() []ec2types.Filter {
	return []ec2types.Filter{
		{Name: aws.String("tag:" + names.TagDeployment), Values: []string{dep.ID()}},
		{Name: aws.String("tag:" + tagPool), Values: []string{"slots"}},
	}
}

// listSlotInstances returns the deployment's slot instances that aren't
// terminated, ordered by slot number.
func listSlotInstances(ctx context.Context, c *ec2.Client) ([]slotInstance, error) {
	in := &ec2.DescribeInstancesInput{Filters: append(slotFilters(), ec2types.Filter{
		Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped", "shutting-down"},
	})}
	var out []slotInstance
	p := ec2.NewDescribeInstancesPaginator(c, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe slot instances: %w", err)
		}
		for _, r := range page.Reservations {
			for _, i := range r.Instances {
				s := slotInstance{ID: aws.ToString(i.InstanceId), State: instanceState(i), IP: aws.ToString(i.PublicIpAddress)}
				for _, t := range i.Tags {
					switch aws.ToString(t.Key) {
					case names.TagSlot:
						s.Slot, _ = strconv.Atoi(aws.ToString(t.Value))
					case tagPhase:
						s.Phase = aws.ToString(t.Value)
					}
				}
				out = append(out, s)
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Slot < out[b].Slot })
	return out, nil
}

// desiredDTNs reads desired-dtns (0 when unset).
func desiredDTNs(ctx context.Context, c *ssm.Client) int {
	v, err := getSSMParam(ctx, c, dep.Param(names.ParamDesiredDTNs))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

// setDesiredDTNs writes desired-dtns and refreshes last-active, so
// demand_monitor holds the new value for scale_in_idle instead of resetting it
// on its next run while no transfers are active.
func setDesiredDTNs(ctx context.Context, c *ssm.Client, n int) error {
	put := func(leaf, v string) error {
		_, err := c.PutParameter(ctx, &ssm.PutParameterInput{Name: aws.String(dep.Param(leaf)), Value: aws.String(v),
			Type: ssmtypes.ParameterTypeString, Overwrite: aws.Bool(true)})
		return err
	}
	if err := put(names.ParamLastActive, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("put last-active: %w", err)
	}
	if err := put(names.ParamDesiredDTNs, strconv.Itoa(n)); err != nil {
		return fmt.Errorf("put desired-dtns: %w", err)
	}
	return nil
}

// maxDTNs reads MAX_DTNS from the slot controller's configuration (0 if it
// can't be read).
func maxDTNs(ctx context.Context, c *lambda.Client) int {
	out, err := c.GetFunctionConfiguration(ctx, &lambda.GetFunctionConfigurationInput{
		FunctionName: aws.String(dep.Resource("slot-controller")),
	})
	if err != nil || out.Environment == nil {
		return 0
	}
	n, _ := strconv.Atoi(out.Environment.Variables["MAX_DTNS"])
	return n
}
