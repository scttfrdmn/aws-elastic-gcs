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
)

// handler terminates warm pool instances that have been stopped too long.
//
// Triggered by: EventBridge scheduled rule (daily)
//
// Logic:
//   1. List instances in ASG warm pool with state "Warmed:Stopped"
//   2. For each, check the EC2 instance's StateTransitionReason / launch time
//   3. If stopped for longer than CACHE_EVICTION_HOURS → terminate it
//   4. ASG automatically launches a replacement (cold boot from AMI)
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

	// Describe EC2 instances to get launch time / state transition time
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
			launchTime := aws.ToTime(inst.LaunchTime)

			// Use launch time as proxy for "time in warm pool".
			// Instances that were launched long ago and are still stopped are
			// candidates for eviction. A more precise approach would track the
			// time they entered Warmed:Stopped, but launch time is sufficient
			// for the 72-hour TTL use case.
			age := time.Since(launchTime)
			log.Printf("Instance %s: launch=%s age=%s", instanceID, launchTime.Format(time.RFC3339), age.Round(time.Minute))

			if launchTime.Before(evictionCutoff) {
				log.Printf("Evicting %s (age %s > TTL %dh)", instanceID, age.Round(time.Hour), evictionHours)
				_, err := ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
					InstanceIds: []string{instanceID},
				})
				if err != nil {
					log.Printf("WARNING: Failed to terminate %s: %v", instanceID, err)
				} else {
					log.Printf("Terminated warm pool instance %s", instanceID)
					evicted++
				}
			}
		}
	}

	log.Printf("Cache eviction complete: checked=%d evicted=%d", len(stoppedIDs), evicted)
	return nil
}

func main() {
	log.Printf("Elastic GCS Cache Eviction Lambda starting")
	lambda.Start(handler)
}
