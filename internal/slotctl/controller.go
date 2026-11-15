package slotctl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/gcscli"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/identity"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/slots"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/ssmexec"
)

// EC2 is the subset of the EC2 client the controller uses.
type EC2 interface {
	identity.EC2
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	StartInstances(ctx context.Context, in *ec2.StartInstancesInput, opts ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(ctx context.Context, in *ec2.StopInstancesInput, opts ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, opts ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	DeleteTags(ctx context.Context, in *ec2.DeleteTagsInput, opts ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error)
	DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, opts ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
	AllocateAddress(ctx context.Context, in *ec2.AllocateAddressInput, opts ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error)
	AssociateAddress(ctx context.Context, in *ec2.AssociateAddressInput, opts ...func(*ec2.Options)) (*ec2.AssociateAddressOutput, error)
	ReleaseAddress(ctx context.Context, in *ec2.ReleaseAddressInput, opts ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error)
}

// SSM is the subset of the SSM client the controller uses.
type SSM interface {
	ssmexec.API
	DescribeInstanceInformation(ctx context.Context, in *ssm.DescribeInstanceInformationInput, opts ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error)
	GetParameter(ctx context.Context, in *ssm.GetParameterInput, opts ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	PutParameter(ctx context.Context, in *ssm.PutParameterInput, opts ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	DeleteParameter(ctx context.Context, in *ssm.DeleteParameterInput, opts ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
}

// Env is the deployment the controller manages.
type Env struct {
	Dep              names.Deployment
	MaxDTNs          int
	LaunchTemplateID string
	InstanceTypes    []string // fallback order
	RuntimeProfile   string
	SetupProfile     string
	SetupRole        string
}

// Controller observes and acts on one deployment's slots.
type Controller struct {
	EC2    EC2
	SSM    SSM
	IAM    identity.IAM
	Env    Env
	Now    func() time.Time
	Lookup func(host string) ([]string, error) // DNS; default net.LookupHost
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

func (c *Controller) lookup(host string) ([]string, error) {
	if c.Lookup != nil {
		return c.Lookup(host)
	}
	return net.LookupHost(host)
}

// --- Gather -----------------------------------------------------------------

// Gather observes the deployment and keeps the health tags current.
func (c *Controller) Gather(ctx context.Context) (slots.Observed, error) {
	insts, err := c.slotInstances(ctx)
	if err != nil {
		return slots.Observed{}, err
	}
	domain := c.param(ctx, names.ParamEndpointDomain)
	var inDNS map[string]bool
	if domain != "" {
		inDNS = map[string]bool{}
		if ips, err := c.lookup(domain); err == nil {
			for _, ip := range ips {
				inDNS[ip] = true
			}
		}
	}

	var running []string
	for _, in := range insts {
		if in.State != nil && in.State.Name == ec2types.InstanceStateNameRunning {
			running = append(running, aws.ToString(in.InstanceId))
		}
	}
	online := c.ssmOnline(ctx, running)
	conns := c.dataConns(ctx, running, online)

	probes := map[string]Probe{}
	now := c.now()
	for _, in := range insts {
		id := aws.ToString(in.InstanceId)
		p := Probe{SSMOnline: online[id], DataConns: conns[id]}
		p.InDNS = inDNS != nil && inDNS[aws.ToString(in.PublicIpAddress)]
		probes[id] = p
		if slices.Contains(running, id) {
			c.trackHealth(ctx, in, p.SSMOnline && p.InDNS, now)
		}
	}
	serving := c.controlServing(ctx, inDNS)
	return Observed(insts, probes, serving, c.Env.MaxDTNs, now), nil
}

// trackHealth sets elastic-gcs:unhealthy-since when a running slot first fails
// its check, and clears it (and the restart count) once it passes.
func (c *Controller) trackHealth(ctx context.Context, in ec2types.Instance, healthy bool, now time.Time) {
	t := tagMap(in.Tags)
	id := aws.ToString(in.InstanceId)
	switch {
	case !healthy && t[TagUnhealthySince] == "":
		c.tag(ctx, id, TagUnhealthySince, now.Format(time.RFC3339))
	case healthy && (t[TagUnhealthySince] != "" || t[TagRestarts] != ""):
		_, _ = c.EC2.DeleteTags(ctx, &ec2.DeleteTagsInput{Resources: []string{id},
			Tags: []ec2types.Tag{{Key: aws.String(TagUnhealthySince)}, {Key: aws.String(TagRestarts)}}})
	}
}

func (c *Controller) slotInstances(ctx context.Context) ([]ec2types.Instance, error) {
	var out []ec2types.Instance
	p := ec2.NewDescribeInstancesPaginator(c.EC2, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:" + names.TagDeployment), Values: []string{c.Env.Dep.ID()}},
		{Name: aws.String("tag:" + TagPool), Values: []string{"slots"}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	}})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe slot instances: %w", err)
		}
		for _, r := range page.Reservations {
			out = append(out, r.Instances...)
		}
	}
	slices.SortFunc(out, func(a, b ec2types.Instance) int {
		return strings.Compare(aws.ToString(a.InstanceId), aws.ToString(b.InstanceId))
	})
	return out, nil
}

func (c *Controller) ssmOnline(ctx context.Context, ids []string) map[string]bool {
	m := map[string]bool{}
	if len(ids) == 0 {
		return m
	}
	out, err := c.SSM.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
		Filters: []ssmtypes.InstanceInformationStringFilter{{Key: aws.String("InstanceIds"), Values: ids}},
	})
	if err != nil {
		return m
	}
	for _, i := range out.InstanceInformationList {
		m[aws.ToString(i.InstanceId)] = i.PingStatus == ssmtypes.PingStatusOnline
	}
	return m
}

// dataConnsScript counts established GridFTP data connections.
const dataConnsScript = `ss -Htn state established '( sport >= :50000 and sport <= :51000 ) or ( dport >= :50000 and dport <= :51000 )' | wc -l`

func (c *Controller) dataConns(ctx context.Context, ids []string, online map[string]bool) map[string]int {
	m := map[string]int{}
	for _, id := range ids {
		if !online[id] {
			continue
		}
		res, err := ssmexec.RunOK(ctx, c.SSM, id, dataConnsScript, ssmexec.Options{Wait: 30 * time.Second, Poll: 2 * time.Second, Comment: "elastic-gcs: count data connections"})
		if err == nil {
			m[id], _ = strconv.Atoi(strings.TrimSpace(res.Stdout))
		}
	}
	return m
}

func (c *Controller) controlServing(ctx context.Context, inDNS map[string]bool) bool {
	id := c.param(ctx, names.ParamControlNodeID)
	if id == "" || inDNS == nil {
		return false
	}
	out, err := c.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil || len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return false
	}
	in := out.Reservations[0].Instances[0]
	return in.State != nil && in.State.Name == ec2types.InstanceStateNameRunning && inDNS[aws.ToString(in.PublicIpAddress)]
}

// --- Execute ----------------------------------------------------------------

// Execute performs one action. It returns once the action has been initiated
// (revive runs node setup to completion); later rounds observe the result.
func (c *Controller) Execute(ctx context.Context, a slots.Action) error {
	switch a.Kind {
	case slots.CreateSlot:
		return nil // identities are created by the control node; COLD needs no resource
	case slots.EnsureControl:
		return c.ensureControl(ctx)
	}
	in, found, err := c.slotInstance(ctx, a.Slot)
	if err != nil {
		return err
	}
	id := aws.ToString(in.InstanceId)
	now := c.now().Format(time.RFC3339)
	switch a.Kind {
	case slots.Start:
		if !found {
			return fmt.Errorf("slot %d has no instance to start", a.Slot)
		}
		if _, err := c.EC2.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}}); err != nil {
			return c.withFallback(ctx, in, err)
		}
		c.tags(ctx, id, TagPhase, string(slots.Starting), TagPhaseSince, now)
		return nil
	case slots.BeginDrain:
		// Local disable: no Globus credentials on the DTN (#42). Then wait for
		// data connections in later rounds.
		if _, err := ssmexec.Run(ctx, c.SSM, id, "globus-connect-server node disable 2>&1 | tail -1; "+gcscli.LockFix,
			ssmexec.Options{Wait: 2 * time.Minute, Comment: "elastic-gcs: drain slot"}); err != nil {
			return err
		}
		c.tags(ctx, id, TagPhase, string(slots.Draining), TagPhaseSince, now)
		return nil
	case slots.Stop:
		if _, err := c.EC2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{id}}); err != nil {
			return err
		}
		c.tags(ctx, id, TagPhase, string(slots.Stopping), TagPhaseSince, now, TagStoppedAt, now)
		return nil
	case slots.Restart:
		n, _ := strconv.Atoi(tagMap(in.Tags)[TagRestarts])
		c.tags(ctx, id, TagRestarts, strconv.Itoa(n+1))
		if _, err := c.EC2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{id}}); err != nil {
			return err
		}
		if err := ec2.NewInstanceStoppedWaiter(c.EC2).Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}}, 10*time.Minute); err != nil {
			return err
		}
		if _, err := c.EC2.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}}); err != nil {
			return c.withFallback(ctx, in, err)
		}
		c.tags(ctx, id, TagPhase, string(slots.Starting), TagPhaseSince, now)
		return nil
	case slots.Rebuild, slots.Evict:
		if found {
			if _, err := c.EC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
				return err
			}
		}
		return c.releaseEIP(ctx, a.Slot) // COLD: no instance, no address
	case slots.Revive:
		return c.revive(ctx, a.Slot)
	}
	return fmt.Errorf("unknown action %s", a.Kind)
}

func (c *Controller) ensureControl(ctx context.Context) error {
	id := c.param(ctx, names.ParamControlNodeID)
	if id == "" {
		return errors.New("no control node")
	}
	out, err := c.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil || len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return fmt.Errorf("describe control node: %v", err)
	}
	if st := out.Reservations[0].Instances[0].State; st == nil || st.Name != ec2types.InstanceStateNameRunning {
		// elastic-gcs-control.service enables the node on boot.
		_, err := c.EC2.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}})
		return err
	}
	_, err = ssmexec.RunOK(ctx, c.SSM, id, "/opt/elastic-gcs/scripts/control_node_boot.sh start",
		ssmexec.Options{Wait: 4 * time.Minute, Comment: "elastic-gcs: control node must serve before the last DTN stops"})
	return err
}

// revive launches a slot's instance, gives it the slot's Elastic IP, and runs
// node setup through identity.Setup (setup role only for its duration).
func (c *Controller) revive(ctx context.Context, slot int) error {
	eip, alloc, err := c.slotEIP(ctx, slot)
	if err != nil {
		return err
	}
	now := c.now().Format(time.RFC3339)
	tags := []ec2types.Tag{
		{Key: aws.String(names.TagDeployment), Value: aws.String(c.Env.Dep.ID())},
		{Key: aws.String(names.TagRegion), Value: aws.String(c.Env.Dep.Region)},
		{Key: aws.String(names.TagRole), Value: aws.String(names.RoleDTN)},
		{Key: aws.String(names.TagSlot), Value: aws.String(strconv.Itoa(slot))},
		{Key: aws.String(TagPool), Value: aws.String("slots")},
		{Key: aws.String(TagPhase), Value: aws.String(string(slots.Reviving))},
		{Key: aws.String(TagPhaseSince), Value: aws.String(now)},
		{Key: aws.String("Name"), Value: aws.String(fmt.Sprintf("%s-slot-%d", c.Env.Dep.Resource("dtn"), slot))},
	}
	var id string
	var lastErr error
	for _, typ := range c.Env.InstanceTypes {
		out, err := c.EC2.RunInstances(ctx, &ec2.RunInstancesInput{
			MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
			LaunchTemplate:     &ec2types.LaunchTemplateSpecification{LaunchTemplateId: aws.String(c.Env.LaunchTemplateID), Version: aws.String("$Latest")},
			InstanceType:       ec2types.InstanceType(typ),
			IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{Name: aws.String(c.Env.RuntimeProfile)},
			TagSpecifications:  []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeInstance, Tags: tags}},
		})
		if err == nil {
			id = aws.ToString(out.Instances[0].InstanceId)
			break
		}
		lastErr = err
		if !isCapacityError(err) {
			return fmt.Errorf("launch slot %d: %w", slot, err)
		}
	}
	if id == "" {
		return fmt.Errorf("launch slot %d: no capacity in %v: %w", slot, c.Env.InstanceTypes, lastErr)
	}
	if err := ec2.NewInstanceRunningWaiter(c.EC2).Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}}, 10*time.Minute); err != nil {
		return err
	}
	if _, err := c.EC2.AssociateAddress(ctx, &ec2.AssociateAddressInput{AllocationId: aws.String(alloc), InstanceId: aws.String(id)}); err != nil {
		return fmt.Errorf("associate EIP: %w", err)
	}
	if err := c.waitSSM(ctx, id, 10*time.Minute); err != nil {
		return err
	}
	pfx := c.Env.Dep.SecretsPrefix()
	region := c.Env.Dep.Region
	setup := fmt.Sprintf(`set -euo pipefail
umask 077; D=$(mktemp -d); trap 'rm -rf "$D"' EXIT
aws secretsmanager get-secret-value --secret-id %[1]s/deployment-key --query SecretString --output text --region %[2]s > "$D/key.json"
aws secretsmanager get-secret-value --secret-id %[1]s/node-slot-%[3]d --query SecretString --output text --region %[2]s > "$D/node.json"
globus-connect-server node setup --deployment-key "$D/key.json" --import-node "$D/node.json" --ip-address %[4]s
%[5]s
jq -r '.domain_name' /var/lib/globus-connect-server/info.json`, pfx, region, slot, eip, gcscli.LockFix)
	out, err := identity.Setup(ctx, identity.Deps{EC2: c.EC2, IAM: c.IAM, SSM: c.SSM, Now: c.now}, identity.Spec{
		InstanceID: id, SetupProfile: c.Env.SetupProfile, RuntimeProfile: c.Env.RuntimeProfile, SetupRole: c.Env.SetupRole,
		ProbeScript: fmt.Sprintf("aws secretsmanager describe-secret --secret-id %s/deployment-key --region %s", pfx, region),
		SetupScript: setup,
	})
	if err != nil {
		return fmt.Errorf("slot %d setup: %w", slot, err)
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) > 0 {
		if d := strings.TrimSpace(lines[len(lines)-1]); strings.Contains(d, ".") && c.param(ctx, names.ParamEndpointDomain) == "" {
			c.putParam(ctx, names.ParamEndpointDomain, d)
		}
	}
	return nil
}

// slotEIP returns the slot's Elastic IP, allocating one (tagged) if needed.
func (c *Controller) slotEIP(ctx context.Context, slot int) (ip, allocationID string, err error) {
	out, err := c.EC2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{Filters: c.eipFilters(slot)})
	if err != nil {
		return "", "", err
	}
	if len(out.Addresses) > 0 {
		a := out.Addresses[0]
		return aws.ToString(a.PublicIp), aws.ToString(a.AllocationId), nil
	}
	alloc, err := c.EC2.AllocateAddress(ctx, &ec2.AllocateAddressInput{Domain: ec2types.DomainTypeVpc,
		TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeElasticIp, Tags: []ec2types.Tag{
			{Key: aws.String(names.TagDeployment), Value: aws.String(c.Env.Dep.ID())},
			{Key: aws.String(names.TagSlot), Value: aws.String(strconv.Itoa(slot))},
			{Key: aws.String(TagPool), Value: aws.String("slots")},
		}}}})
	if err != nil {
		return "", "", fmt.Errorf("allocate EIP for slot %d: %w", slot, err)
	}
	return aws.ToString(alloc.PublicIp), aws.ToString(alloc.AllocationId), nil
}

func (c *Controller) releaseEIP(ctx context.Context, slot int) error {
	out, err := c.EC2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{Filters: c.eipFilters(slot)})
	if err != nil {
		return err
	}
	for _, a := range out.Addresses {
		if _, err := c.EC2.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: a.AllocationId}); err != nil {
			return fmt.Errorf("release EIP %s: %w", aws.ToString(a.PublicIp), err)
		}
	}
	return nil
}

func (c *Controller) eipFilters(slot int) []ec2types.Filter {
	return []ec2types.Filter{
		{Name: aws.String("tag:" + names.TagDeployment), Values: []string{c.Env.Dep.ID()}},
		{Name: aws.String("tag:" + names.TagSlot), Values: []string{strconv.Itoa(slot)}},
		{Name: aws.String("tag:" + TagPool), Values: []string{"slots"}},
	}
}

func (c *Controller) slotInstance(ctx context.Context, slot int) (ec2types.Instance, bool, error) {
	insts, err := c.slotInstances(ctx)
	if err != nil {
		return ec2types.Instance{}, false, err
	}
	for _, in := range insts {
		if tagMap(in.Tags)[names.TagSlot] == strconv.Itoa(slot) {
			return in, true, nil
		}
	}
	return ec2types.Instance{}, false, nil
}

// withFallback retries a capacity-failed start with the next instance type.
func (c *Controller) withFallback(ctx context.Context, in ec2types.Instance, startErr error) error {
	if !isCapacityError(startErr) {
		return startErr
	}
	// Changing type requires the stopped instance; the next round retries Start.
	cur := string(in.InstanceType)
	for i, t := range c.Env.InstanceTypes {
		if t == cur && i+1 < len(c.Env.InstanceTypes) {
			return fmt.Errorf("start: no capacity for %s (next round tries %s): %w", cur, c.Env.InstanceTypes[i+1], startErr)
		}
	}
	return startErr
}

func isCapacityError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "InsufficientInstanceCapacity")
}

func (c *Controller) waitSSM(ctx context.Context, id string, max time.Duration) error {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if c.ssmOnline(ctx, []string{id})[id] {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return fmt.Errorf("instance %s never came online in SSM", id)
}

func (c *Controller) tag(ctx context.Context, id, k, v string) { c.tags(ctx, id, k, v) }

func (c *Controller) tags(ctx context.Context, id string, kv ...string) {
	var ts []ec2types.Tag
	for i := 0; i+1 < len(kv); i += 2 {
		ts = append(ts, ec2types.Tag{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
	}
	_, _ = c.EC2.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{id}, Tags: ts})
}

func (c *Controller) param(ctx context.Context, leaf string) string {
	out, err := c.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(c.Env.Dep.Param(leaf))})
	if err != nil || out.Parameter == nil {
		return ""
	}
	return aws.ToString(out.Parameter.Value)
}

func (c *Controller) putParam(ctx context.Context, leaf, v string) {
	_, _ = c.SSM.PutParameter(ctx, &ssm.PutParameterInput{Name: aws.String(c.Env.Dep.Param(leaf)), Value: aws.String(v),
		Type: ssmtypes.ParameterTypeString, Overwrite: aws.Bool(true)})
}
