package ssmexec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// fake returns the given statuses from successive GetCommandInvocation calls
// (an empty string means "invocation not registered yet").
type fake struct {
	sendErr  error
	statuses []ssmtypes.CommandInvocationStatus
	calls    int
	sent     *ssm.SendCommandInput
}

func (f *fake) SendCommand(_ context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	f.sent = in
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String("cmd-1")}}, nil
}

func (f *fake) GetCommandInvocation(_ context.Context, _ *ssm.GetCommandInvocationInput, _ ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	i := f.calls
	f.calls++
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	if f.statuses[i] == "" {
		return nil, errors.New("InvocationDoesNotExist")
	}
	return &ssm.GetCommandInvocationOutput{Status: f.statuses[i], StandardOutputContent: aws.String("out"), StandardErrorContent: aws.String("err")}, nil
}

var fast = Options{Wait: 200 * time.Millisecond, Poll: time.Millisecond}

func TestRun(t *testing.T) {
	S, F, P := ssmtypes.CommandInvocationStatusSuccess, ssmtypes.CommandInvocationStatusFailed, ssmtypes.CommandInvocationStatusInProgress
	cases := []struct {
		name     string
		f        *fake
		ok       bool // RunOK succeeds
		runErr   bool // Run returns an error
		wantStat ssmtypes.CommandInvocationStatus
	}{
		{"success after pending", &fake{statuses: []ssmtypes.CommandInvocationStatus{"", P, S}}, true, false, S},
		{"failed is a result, not an error", &fake{statuses: []ssmtypes.CommandInvocationStatus{F}}, false, false, F},
		{"timed out", &fake{statuses: []ssmtypes.CommandInvocationStatus{ssmtypes.CommandInvocationStatusTimedOut}}, false, true, ssmtypes.CommandInvocationStatusTimedOut},
		{"never finishes", &fake{statuses: []ssmtypes.CommandInvocationStatus{P}}, false, true, P},
		{"send fails", &fake{sendErr: errors.New("boom"), statuses: []ssmtypes.CommandInvocationStatus{S}}, false, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := Run(context.Background(), c.f, "i-1", "echo hi", fast)
			if (err != nil) != c.runErr || res.Status != c.wantStat {
				t.Fatalf("Run: status=%q err=%v; want status=%q err=%v", res.Status, err, c.wantStat, c.runErr)
			}
			c.f.calls = 0
			if _, err := RunOK(context.Background(), c.f, "i-1", "echo hi", fast); (err == nil) != c.ok {
				t.Fatalf("RunOK err=%v, want ok=%v", err, c.ok)
			}
		})
	}
}

func TestRunHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	f := &fake{statuses: []ssmtypes.CommandInvocationStatus{ssmtypes.CommandInvocationStatusInProgress}}
	_, err := Run(ctx, f, "i-1", "sleep 99", Options{Wait: time.Minute, Poll: time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline", err)
	}
}

func TestExecutionTimeoutFollowsWait(t *testing.T) {
	f := &fake{statuses: []ssmtypes.CommandInvocationStatus{ssmtypes.CommandInvocationStatusSuccess}}
	_, _ = Run(context.Background(), f, "i-1", "x", Options{Wait: 4 * time.Minute, Poll: time.Millisecond, Comment: "c"})
	if got := f.sent.Parameters["executionTimeout"][0]; got != "240" {
		t.Fatalf("executionTimeout=%s, want 240", got)
	}
	if !strings.Contains(aws.ToString(f.sent.Comment), "c") || aws.ToString(f.sent.DocumentName) != "AWS-RunShellScript" {
		t.Fatalf("unexpected input %+v", f.sent)
	}
}
