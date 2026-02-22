package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const warmPoolEntryTagKey = "elastic-gcs:warm-pool-entry-time"

// handler terminates warm pool instances that have been stopped too long.
//
// Triggered by: EventBridge scheduled rule (daily)
//
// Logic:
//  1. List instances in ASG warm pool with state "Warmed:Stopped"
//  2. For each, check the elastic-gcs:warm-pool-entry-time EC2 tag (set by
//     the lifecycle Lambda when the instance was scaled in to the warm pool).
//     Falls back to LaunchTime if the tag is absent.
//  3. If stopped for longer than CACHE_EVICTION_HOURS → terminate it
//  4. ASG automatically launches a replacement (cold boot from AMI)
//
// Note: We do NOT run node cleanup before termination here. The GCS node
// was already disabled when the instance moved to Warmed:Stopped (via
// the lifecycle Lambda's node disable). Terminating the EC2 instance
// leaves a disabled (but still registered) node in the GCS endpoint.
// The control node's maintenance script should periodically clean up
// terminated node entries via: globus-connect-server node delete <id>
func handler(ctx context.Context, _ interface{}) error {
	asgName := os.Getenv("ASG_NAME")
	region := os.Getenv("AWS_REGION")
	evictionHours, _ := strconv.Atoi(os.Getenv("CACHE_EVICTION_HOURS"))
	if evictionHours == 0 {
		evictionHours = 72
	}

	log.Printf("Cache eviction check: region=%s asg=%s ttl=%dh", region, asgName, evictionHours)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	asgClient := autoscaling.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
	ssmClient := ssm.NewFromConfig(cfg)

	// List warm pool instances
	warmPool, err := asgClient.DescribeWarmPool(ctx, &autoscaling.DescribeWarmPoolInput{
		AutoScalingGroupName: aws.String(asgName),
	})
	if err != nil {
		return fmt.Errorf("DescribeWarmPool failed: %w", err)
	}

	if len(warmPool.Instances) == 0 {
		log.Printf("No warm pool instances found")
		return nil
	}

	// Find stopped instances to check for eviction
	var stoppedIDs []string
	for _, inst := range warmPool.Instances {
		state := string(inst.LifecycleState)
		if state == "Warmed:Stopped" {
			stoppedIDs = append(stoppedIDs, aws.ToString(inst.InstanceId))
		}
	}

	if len(stoppedIDs) == 0 {
		log.Printf("No Warmed:Stopped instances to check")
		return nil
	}

	log.Printf("Checking %d stopped warm pool instances for eviction (TTL=%dh)", len(stoppedIDs), evictionHours)

	// Describe EC2 instances to get tags and launch time
	ec2Instances, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: stoppedIDs,
	})
	if err != nil {
		return fmt.Errorf("DescribeInstances failed: %w", err)
	}

	evictionCutoff := time.Now().Add(-time.Duration(evictionHours) * time.Hour)
	var evicted int

	for _, reservation := range ec2Instances.Reservations {
		for _, inst := range reservation.Instances {
			instanceID := aws.ToString(inst.InstanceId)

			// Prefer warm-pool entry time tag (set by lifecycle Lambda on scale-in).
			// Fall back to launch time if tag is absent (instances created before this fix).
			entryTime := warmPoolEntryTime(inst.Tags, aws.ToTime(inst.LaunchTime))

			age := time.Since(entryTime)
			log.Printf("Instance %s: warm-pool-entry=%s age=%s",
				instanceID, entryTime.Format(time.RFC3339), age.Round(time.Minute))

			if entryTime.Before(evictionCutoff) {
				log.Printf("Evicting %s (age %s > TTL %dh)", instanceID, age.Round(time.Hour), evictionHours)
				_, err := ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
					InstanceIds: []string{instanceID},
				})
				if err != nil {
					log.Printf("WARNING: Failed to terminate %s: %v", instanceID, err)
					continue
				}
				log.Printf("Terminated warm pool instance %s", instanceID)
				evicted++

				// Release the SSM slot lock for this instance. The instance was
				// terminated hard — its shutdown script may not have run — so the
				// lock could be orphaned. Read the slot number from the EC2 tag
				// set by the boot script and delete the corresponding parameter.
				slotNum := slotNumber(inst.Tags)
				if slotNum != "" {
					lockParam := fmt.Sprintf("/elastic-gcs/%s/slot-lock/%s", region, slotNum)
					_, ssmErr := ssmClient.DeleteParameter(ctx, &ssm.DeleteParameterInput{
						Name: aws.String(lockParam),
					})
					if ssmErr != nil {
						log.Printf("WARNING: Could not release slot lock %s for %s: %v", lockParam, instanceID, ssmErr)
					} else {
						log.Printf("Released slot lock %s for terminated instance %s", lockParam, instanceID)
					}
				} else {
					log.Printf("WARNING: No elastic-gcs:slot tag on %s — slot lock not released", instanceID)
				}
			}
		}
	}

	log.Printf("Cache eviction complete: checked=%d evicted=%d", len(stoppedIDs), evicted)
	return nil
}

// slotNumber returns the DTN slot number from the elastic-gcs:slot EC2 tag,
// or an empty string if the tag is absent.
func slotNumber(tags []ec2types.Tag) string {
	for _, tag := range tags {
		if aws.ToString(tag.Key) == "elastic-gcs:slot" {
			return aws.ToString(tag.Value)
		}
	}
	return ""
}

// warmPoolEntryTime returns the time the instance entered the warm pool.
// Uses the elastic-gcs:warm-pool-entry-time tag when present (set by the
// lifecycle Lambda), falling back to the EC2 launch time if the tag is absent.
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

func main() {
	log.Printf("Elastic GCS Cache Eviction Lambda starting")
	lambda.Start(handler)
}
