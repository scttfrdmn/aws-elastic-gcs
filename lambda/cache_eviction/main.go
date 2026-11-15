package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/eviction"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/gcscli"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/logger"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/slotlock"
)

const (
	warmPoolEntryTagKey = names.TagWarmPoolEntryTime
	gcsNodeIDTagKey     = names.TagGCSNodeID
)

// handler terminates warm pool instances that have been stopped too long,
// deletes their GCS node registrations, and sweeps any other orphaned nodes.
//
// Triggered by: EventBridge scheduled rule (daily)
//
// Logic:
//  1. List instances in ASG warm pool with state "Warmed:Stopped"
//  2. For each, check the elastic-gcs:warm-pool-entry-time tag (set by the
//     lifecycle Lambda on scale-in). Falls back to LaunchTime if absent.
//  3. If stopped > CACHE_EVICTION_HOURS → terminate + delete GCS node
//  4. Orphan sweep: list all GCS nodes; delete inactive ones whose
//     EC2 instance no longer exists (catches ABANDON'd / hard-terminated cases)
func handler(ctx context.Context, _ interface{}) error {
	asgName := os.Getenv("ASG_NAME")
	region := os.Getenv("AWS_REGION")
	secretsPrefix := os.Getenv("SECRETS_PREFIX")
	controlNodeIDParam := os.Getenv("CONTROL_NODE_ID_PARAM")
	evictionHours, _ := strconv.Atoi(os.Getenv("CACHE_EVICTION_HOURS"))
	if evictionHours == 0 {
		evictionHours = 72
	}

	logger.Info("cache eviction check starting",
		"region", region,
		"asg", asgName,
		"ttl_hours", evictionHours,
	)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	asgClient := autoscaling.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
	ssmClient := ssm.NewFromConfig(cfg)

	// Resolve control node instance ID (needed for GCS CLI operations via SSM).
	controlNodeID := ""
	if controlNodeIDParam != "" {
		paramOut, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
			Name: aws.String(controlNodeIDParam),
		})
		if err != nil {
			logger.Warn("could not fetch control node ID, GCS node cleanup skipped",
				"param", controlNodeIDParam,
				"error", err.Error(),
			)
		} else {
			controlNodeID = aws.ToString(paramOut.Parameter.Value)
			logger.Info("control node resolved", "instance", controlNodeID)
		}
	}

	// List warm pool instances.
	warmPool, err := asgClient.DescribeWarmPool(ctx, &autoscaling.DescribeWarmPoolInput{
		AutoScalingGroupName: aws.String(asgName),
	})
	if err != nil {
		return fmt.Errorf("DescribeWarmPool failed: %w", err)
	}

	var stoppedIDs []string
	for _, inst := range warmPool.Instances {
		if string(inst.LifecycleState) == "Warmed:Stopped" {
			stoppedIDs = append(stoppedIDs, aws.ToString(inst.InstanceId))
		}
	}

	if len(stoppedIDs) == 0 {
		logger.Info("no Warmed:Stopped instances to check")
	} else {
		logger.Info("checking stopped warm pool instances for eviction",
			"count", len(stoppedIDs),
			"ttl_hours", evictionHours,
		)
		if err := evictStaleInstances(ctx, ec2Client, ssmClient, stoppedIDs,
			evictionHours, region, secretsPrefix, controlNodeID); err != nil {
			logger.Warn("eviction step failed", "error", err.Error())
		}
	}

	// Orphan sweep: delete GCS nodes whose EC2 instance no longer exists.
	// This catches nodes left behind by ABANDON'd lifecycle actions or hard terminates.
	if controlNodeID != "" && secretsPrefix != "" {
		if err := sweepOrphanedNodes(ctx, ssmClient, ec2Client, controlNodeID, asgName); err != nil {
			logger.Warn("orphan node sweep failed", "error", err.Error())
		}
	} else {
		logger.Info("skipping orphan sweep",
			"control_node_id_param", controlNodeIDParam,
			"secrets_prefix", secretsPrefix,
		)
	}

	return nil
}

// evictStaleInstances terminates warm pool instances past their TTL and
// deletes their GCS node registrations.
func evictStaleInstances(ctx context.Context, ec2Client *ec2.Client, ssmClient *ssm.Client,
	instanceIDs []string, evictionHours int, region, secretsPrefix, controlNodeID string) error {

	ec2Instances, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return fmt.Errorf("DescribeInstances failed: %w", err)
	}

	evictionCutoff := time.Now().Add(-time.Duration(evictionHours) * time.Hour)
	var evicted int

	for _, reservation := range ec2Instances.Reservations {
		for _, inst := range reservation.Instances {
			instanceID := aws.ToString(inst.InstanceId)
			entryTime := warmPoolEntryTime(inst.Tags, aws.ToTime(inst.LaunchTime))
			age := time.Since(entryTime)
			logger.Info("checking instance age",
				"instance", instanceID,
				"warm_pool_entry", entryTime.Format(time.RFC3339),
				"age", age.Round(time.Minute).String(),
			)

			if !entryTime.Before(evictionCutoff) {
				continue
			}

			logger.Info("evicting stale instance",
				"instance", instanceID,
				"age", age.Round(time.Hour).String(),
				"ttl_hours", evictionHours,
			)
			_, err := ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
				InstanceIds: []string{instanceID},
			})
			if err != nil {
				logger.Warn("failed to terminate instance",
					"instance", instanceID,
					"error", err.Error(),
				)
				continue
			}
			logger.Info("terminated warm pool instance", "instance", instanceID)
			evicted++

			// Release SSM slot lock (may be orphaned if shutdown script didn't run).
			if slotNum := slotNumber(inst.Tags); slotNum != "" {
				// Only if this instance still owns it (#50).
				outcome, ssmErr := slotlock.Release(ctx, ssmClient, dep, slotNum, instanceID)
				if ssmErr != nil {
					logger.Warn("could not release slot lock", "slot", slotNum, "error", ssmErr.Error())
				} else {
					logger.Info("slot lock release", "slot", slotNum, "instance", instanceID, "outcome", string(outcome))
				}
			} else {
				logger.Warn("no elastic-gcs:slot tag on instance, slot lock not released",
					"instance", instanceID,
				)
			}

			// Delete the GCS node registration for the terminated instance.
			if controlNodeID != "" {
				nodeID := gcsNodeIDTag(inst.Tags)
				if nodeID != "" {
					logger.Info("deleting GCS node for evicted instance",
						"node", nodeID,
						"instance", instanceID,
					)
					if err := deleteGCSNode(ctx, ssmClient, controlNodeID, nodeID); err != nil {
						logger.Warn("GCS node delete failed, will be caught by orphan sweep",
							"node", nodeID,
							"error", err.Error(),
						)
					}
				} else {
					logger.Warn("no gcs-node-id tag on instance, GCS node not deleted; will be caught by orphan sweep",
						"tag", gcsNodeIDTagKey,
						"instance", instanceID,
					)
				}
			}
		}
	}

	logger.Info("eviction complete", "checked", len(instanceIDs), "evicted", evicted)
	return nil
}

// sweepOrphanedNodes deletes GCS node registrations that are inactive and
// have no corresponding live EC2 instance (running, stopped, pending, or stopping).
//
// Safety: if any live ASG instance lacks the elastic-gcs:gcs-node-id tag (i.e., it
// was booted before that tag was added), the sweep is skipped entirely. Without the
// tag we cannot determine which GCS nodes belong to live instances, so deleting
// inactive nodes would break their next resume. This guards the transition period
// until all instances in the pool run a boot script version that sets the tag.
func sweepOrphanedNodes(ctx context.Context, ssmClient *ssm.Client, ec2Client *ec2.Client, controlNodeID, asgName string) error {

	logger.Info("starting orphan GCS node sweep")

	// Every non-terminated instance of this ASG (InService and warm pool), via
	// its aws:autoscaling:groupName tag, paginated (#52).
	live := map[string]bool{}
	pager := ec2.NewDescribeInstancesPaginator(ec2Client, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:aws:autoscaling:groupName"), Values: []string{asgName}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
		},
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("DescribeInstances for %s: %w", asgName, err)
		}
		for _, r := range page.Reservations {
			for _, inst := range r.Instances {
				nodeID := gcsNodeIDTag(inst.Tags)
				if nodeID == "" {
					// Without the tag we can't tell which node is this instance's;
					// deleting would break its next resume.
					logger.Warn("instance has no gcs-node-id tag, skipping orphan sweep",
						"instance", aws.ToString(inst.InstanceId), "state", string(inst.State.Name))
					return nil
				}
				live[nodeID] = true
			}
		}
	}

	// The control node isn't a DTN, and when it's out of the data path its node
	// is inactive: protect it by IP.
	protected := map[string]bool{}
	if out, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{controlNodeID}}); err == nil {
		for _, r := range out.Reservations {
			for _, inst := range r.Instances {
				if ip := aws.ToString(inst.PublicIpAddress); ip != "" {
					protected[ip] = true
				}
			}
		}
	}
	if len(protected) == 0 {
		logger.Warn("could not determine the control node's IP, skipping orphan sweep", "instance", controlNodeID)
		return nil
	}

	allNodes, err := gcscli.ListNodes(ctx, ssmClient, controlNodeID, dep)
	if err != nil {
		return fmt.Errorf("node list via SSM failed: %w", err)
	}
	orphans := eviction.OrphanNodes(allNodes, live, protected)
	var deleted int
	for _, id := range orphans {
		logger.Info("deleting orphaned inactive GCS node", "node", id)
		if err := deleteGCSNode(ctx, ssmClient, controlNodeID, id); err != nil {
			logger.Warn("failed to delete orphan node", "node", id, "error", err.Error())
		} else {
			deleted++
		}
	}
	logger.Info("orphan sweep complete", "total_nodes", len(allNodes), "live_dtns", len(live),
		"orphans", len(orphans), "deleted", deleted)
	return nil
}

// deleteGCSNode deletes a GCS node registration via `node delete` run on the control node.
// nodeID comes from the elastic-gcs:gcs-node-id EC2 tag (writable by DTNs) or
// from GCS CLI output, and is interpolated into a root shell command, so it
// must be validated as a UUID first.
func deleteGCSNode(ctx context.Context, ssmClient *ssm.Client, controlNodeID, nodeID string) error {
	out, err := gcscli.DeleteNode(ctx, ssmClient, controlNodeID, dep, nodeID)
	if err != nil {
		logger.Error("node delete failed", "node", nodeID, "error", err.Error())
		return err
	}
	logger.Info("node delete completed", "node", nodeID, "output", strings.TrimSpace(out))
	return nil
}

// gcsNodeIDTag returns the elastic-gcs:gcs-node-id EC2 tag value, or "".
func gcsNodeIDTag(tags []ec2types.Tag) string {
	for _, tag := range tags {
		if aws.ToString(tag.Key) == gcsNodeIDTagKey {
			return aws.ToString(tag.Value)
		}
	}
	return ""
}

// slotNumber returns the DTN slot number from the elastic-gcs:slot EC2 tag, or "".
func slotNumber(tags []ec2types.Tag) string {
	for _, tag := range tags {
		if aws.ToString(tag.Key) == names.TagSlot {
			return aws.ToString(tag.Value)
		}
	}
	return ""
}

// warmPoolEntryTime returns the time the instance entered the warm pool.
// Uses the elastic-gcs:warm-pool-entry-time tag when present, falls back to launch time.
func warmPoolEntryTime(tags []ec2types.Tag, launchTime time.Time) time.Time {
	for _, tag := range tags {
		if aws.ToString(tag.Key) == warmPoolEntryTagKey {
			if t, err := time.Parse(time.RFC3339, aws.ToString(tag.Value)); err == nil {
				return t
			}
		}
	}
	return launchTime
}

// dep identifies this deployment; set in main from AWS_REGION and DEPLOYMENT.
var dep names.Deployment

func main() {
	region := os.Getenv("AWS_REGION")
	logger.Init("elastic-gcs-cache-eviction", region)
	var err error
	if dep, err = names.FromEnv(); err != nil {
		logger.Error("invalid deployment configuration", "error", err.Error())
		os.Exit(1)
	}
	lambda.Start(handler)
}
