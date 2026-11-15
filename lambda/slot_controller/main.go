// slot_controller is the slot controller (#65, #128): a Lambda durable
// function that converges a deployment's slots to desired-dtns. Triggered every
// minute and on desired-dtns changes; one execution at a time (an SSM lock).
//
// Each round: observe (a recorded step) -> plan (pure, from recorded results,
// so replay is deterministic) -> execute each action as its own step -> a
// durable wait. It stops when a round plans nothing, or after maxRounds.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-durable-execution-sdk-go/durable"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/logger"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/slotctl"
	"github.com/scttfrdmn/aws-elastic-gcs/internal/slots"
)

// Engine is what the handler needs from the controller (faked in tests).
type Engine interface {
	Acquire(ctx context.Context, owner string, stale time.Duration) error
	Release(ctx context.Context, owner string)
	Gather(ctx context.Context) (slots.Observed, error)
	Desired(ctx context.Context) int
	Execute(ctx context.Context, a slots.Action) error
}

const (
	maxRounds  = 40
	roundPause = 15 * time.Second
	lockStale  = 30 * time.Minute
)

// Result summarizes an execution.
type Result struct {
	Skipped bool     `json:"skipped,omitempty"` // another execution held the lock
	Rounds  int      `json:"rounds"`
	Actions []string `json:"actions,omitempty"`
	Errors  []string `json:"errors,omitempty"`
}

func newHandler(eng Engine, cfg slots.Config) func(durable.Context, any) (Result, error) {
	return func(ctx durable.Context, _ any) (Result, error) {
		owner := ctx.ExecutionArn()
		got, err := durable.Step(ctx, "lock", func(sc durable.StepContext) (bool, error) {
			err := eng.Acquire(sc, owner, lockStale)
			if errors.Is(err, slotctl.ErrLocked) {
				return false, nil
			}
			return err == nil, err
		})
		if err != nil {
			return Result{}, err
		}
		if !got {
			return Result{Skipped: true}, nil
		}
		var res Result
		for round := 0; round < maxRounds; round++ {
			res.Rounds = round + 1
			obs, err := durable.Step(ctx, fmt.Sprintf("observe-%d", round), func(sc durable.StepContext) (slots.Observed, error) {
				return eng.Gather(sc)
			})
			if err != nil {
				res.Errors = append(res.Errors, "observe: "+err.Error())
				break
			}
			desired, _ := durable.Step(ctx, fmt.Sprintf("desired-%d", round), func(sc durable.StepContext) (int, error) {
				return eng.Desired(sc), nil
			})
			acts := slots.Plan(obs, desired, cfg)
			if len(acts) == 0 {
				break
			}
			for i, a := range acts {
				msg, _ := durable.Step(ctx, fmt.Sprintf("r%d-a%d-%s-%d", round, i, a.Kind, a.Slot), func(sc durable.StepContext) (string, error) {
					if err := eng.Execute(sc, a); err != nil {
						return "error: " + err.Error(), nil // logged; the next round re-plans
					}
					return "ok", nil
				})
				res.Actions = append(res.Actions, a.String())
				if strings.HasPrefix(msg, "error: ") {
					res.Errors = append(res.Errors, a.String()+": "+strings.TrimPrefix(msg, "error: "))
				}
			}
			if err := durable.Wait(ctx, fmt.Sprintf("pause-%d", round), roundPause); err != nil {
				return res, err
			}
		}
		_, _ = durable.Step(ctx, "unlock", func(sc durable.StepContext) (bool, error) {
			eng.Release(sc, owner)
			return true, nil
		})
		return res, nil
	}
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return n
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return d
	}
	return def
}

func main() {
	logger.Init("elastic-gcs-slot-controller", os.Getenv("AWS_REGION"))
	dep, err := names.FromEnv()
	if err != nil {
		logger.Error("invalid deployment configuration", "error", err.Error())
		os.Exit(1)
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		logger.Error("aws config", "error", err.Error())
		os.Exit(1)
	}
	cfg := slots.Config{
		MaxDTNs:        envInt("MAX_DTNS", 1),
		WarmSlots:      envInt("WARM_SLOTS", 2),
		DrainTimeout:   envDur("DRAIN_TIMEOUT", 10*time.Minute),
		ColdTTL:        envDur("COLD_TTL", 72*time.Hour),
		UnhealthyAfter: envDur("UNHEALTHY_AFTER", 5*time.Minute),
		MaxRestarts:    envInt("MAX_RESTARTS", 2),
	}
	ctl := &slotctl.Controller{
		EC2: ec2.NewFromConfig(awsCfg), SSM: ssm.NewFromConfig(awsCfg), IAM: iam.NewFromConfig(awsCfg),
		Env: slotctl.Env{
			Dep: dep, MaxDTNs: cfg.MaxDTNs,
			LaunchTemplateID: os.Getenv("LAUNCH_TEMPLATE_ID"),
			InstanceTypes:    strings.Split(os.Getenv("INSTANCE_TYPES"), ","),
			SubnetIDs:        strings.Split(os.Getenv("SUBNET_IDS"), ","),
			RuntimeProfile:   os.Getenv("RUNTIME_PROFILE"),
			SetupProfile:     os.Getenv("SETUP_PROFILE"),
			SetupRole:        os.Getenv("SETUP_ROLE"),
		},
	}
	durable.Start(newHandler(ctl, cfg))
}
