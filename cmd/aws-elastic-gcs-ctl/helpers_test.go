package main

import (
	"testing"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestInstanceState(t *testing.T) {
	if got := instanceState(ec2types.Instance{}); got != "" {
		t.Errorf("nil State: got %q, want empty", got)
	}
	inst := ec2types.Instance{State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}}
	if got := instanceState(inst); got != "running" {
		t.Errorf("got %q, want running", got)
	}
}

func TestParseDuration(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"1.5h", 90 * time.Minute, false},
		{"2d", 48 * time.Hour, false},
		{"m", 0, true},
		{"10s", 0, true},
		{"abc", 0, true},
	} {
		got, err := parseDuration(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseDuration(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSparkline(t *testing.T) {
	if got := sparkline(nil, 0); got != "" {
		t.Errorf("empty: got %q", got)
	}
	if got := sparkline([]float64{0, 0}, 0); got != "▁▁" {
		t.Errorf("zero max: got %q", got)
	}
	if got := sparkline([]float64{0, 7, 14, 99}, 14); got != "▁▅██" { // 7/14 of 7 steps rounds up to index 4; 99 clamps to the top
		t.Errorf("got %q", got)
	}
}

func TestTruncID(t *testing.T) {
	if got := truncID("abcdef", 3); got != "abc" {
		t.Errorf("got %q", got)
	}
	if got := truncID("ab", 3); got != "ab" {
		t.Errorf("got %q", got)
	}
	got := truncIDs([]string{"12345678-aaaa", "1"}, 8)
	if len(got) != 2 || got[0] != "12345678" || got[1] != "1" {
		t.Errorf("truncIDs = %v", got)
	}
}
