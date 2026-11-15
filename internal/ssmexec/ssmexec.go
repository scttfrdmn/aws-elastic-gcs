// Package ssmexec runs a shell script on an instance with SSM Run Command
// (AWS-RunShellScript) and waits for the result (#51).
//
// Run returns an error only when the command could not be sent, did not finish
// before the wait deadline or the context deadline, or ended TimedOut/Cancelled.
// A command that ran and exited non-zero returns Status Failed with a nil error:
// each caller decides whether that matters. RunOK treats anything but Success
// as an error.
package ssmexec

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// API is the subset of the SSM client used here (satisfied by *ssm.Client).
type API interface {
	SendCommand(ctx context.Context, in *ssm.SendCommandInput, opts ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(ctx context.Context, in *ssm.GetCommandInvocationInput, opts ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
}

// Options tune one Run. Zero values take the defaults.
type Options struct {
	Wait    time.Duration // how long to wait for a result (default 90s)
	Poll    time.Duration // polling interval (default 3s)
	Comment string        // SSM command comment (shows in the console)
}

// Result is the outcome of a command that reached a final state.
type Result struct {
	CommandID string
	Status    ssmtypes.CommandInvocationStatus
	Stdout    string
	Stderr    string
}

// OK reports whether the command exited 0.
func (r Result) OK() bool { return r.Status == ssmtypes.CommandInvocationStatusSuccess }

// Run sends script to instanceID and waits for it to finish.
func Run(ctx context.Context, api API, instanceID, script string, opt Options) (Result, error) {
	if opt.Wait <= 0 {
		opt.Wait = 90 * time.Second
	}
	if opt.Poll <= 0 {
		opt.Poll = 3 * time.Second
	}
	// The script may run as long as we're willing to wait for it (min 30 s).
	execTimeout := int(opt.Wait.Seconds())
	if execTimeout < 30 {
		execTimeout = 30
	}
	in := &ssm.SendCommandInput{
		InstanceIds:  []string{instanceID},
		DocumentName: aws.String("AWS-RunShellScript"),
		Parameters: map[string][]string{
			"commands":         {script},
			"executionTimeout": {strconv.Itoa(execTimeout)},
		},
	}
	if opt.Comment != "" {
		in.Comment = aws.String(opt.Comment)
	}
	out, err := api.SendCommand(ctx, in)
	if err != nil {
		return Result{}, fmt.Errorf("ssm SendCommand on %s: %w", instanceID, err)
	}
	res := Result{CommandID: aws.ToString(out.Command.CommandId)}

	deadline := time.NewTimer(opt.Wait)
	defer deadline.Stop()
	tick := time.NewTicker(opt.Poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return res, fmt.Errorf("ssm command %s on %s: %w", res.CommandID, instanceID, ctx.Err())
		case <-deadline.C:
			return res, fmt.Errorf("ssm command %s on %s: no result after %s", res.CommandID, instanceID, opt.Wait)
		case <-tick.C:
		}
		inv, err := api.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(res.CommandID),
			InstanceId: aws.String(instanceID),
		})
		if err != nil {
			continue // InvocationDoesNotExist right after sending; retry
		}
		res.Status = inv.Status
		res.Stdout = aws.ToString(inv.StandardOutputContent)
		res.Stderr = aws.ToString(inv.StandardErrorContent)
		switch inv.Status {
		case ssmtypes.CommandInvocationStatusSuccess, ssmtypes.CommandInvocationStatusFailed:
			return res, nil
		case ssmtypes.CommandInvocationStatusTimedOut, ssmtypes.CommandInvocationStatusCancelled,
			ssmtypes.CommandInvocationStatusCancelling:
			return res, fmt.Errorf("ssm command %s on %s: %s", res.CommandID, instanceID, inv.Status)
		}
	}
}

// RunOK is Run, with any status other than Success reported as an error.
func RunOK(ctx context.Context, api API, instanceID, script string, opt Options) (Result, error) {
	res, err := Run(ctx, api, instanceID, script, opt)
	if err == nil && !res.OK() {
		err = fmt.Errorf("ssm command %s on %s failed: %s", res.CommandID, instanceID, trim(res.Stderr, 300))
	}
	return res, err
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
