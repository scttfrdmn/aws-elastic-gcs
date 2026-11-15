package idle

import (
	"testing"
	"time"
)

func TestShouldStop(t *testing.T) {
	base := Inputs{Running: true, Uptime: time.Hour, MinUptime: 30 * time.Minute,
		WarmStates: []string{"Warmed:Stopped"}, ActiveTransfers: []float64{0, 0}}
	mod := func(f func(*Inputs)) Inputs { in := base; f(&in); return in }
	cases := []struct {
		name string
		in   Inputs
		want bool
	}{
		{"idle", base, true},
		{"stopped", mod(func(i *Inputs) { i.Running = false }), false},
		{"young", mod(func(i *Inputs) { i.Uptime = time.Minute }), false},
		{"desired", mod(func(i *Inputs) { i.Desired = 1 }), false},
		{"in service", mod(func(i *Inputs) { i.InService = 1 }), false},
		{"warm pool moving", mod(func(i *Inputs) { i.WarmStates = []string{"Warmed:Pending"} }), false},
		{"no data", mod(func(i *Inputs) { i.ActiveTransfers = nil }), false},
		{"second datapoint active", mod(func(i *Inputs) { i.ActiveTransfers = []float64{0, 1} }), false},
	}
	for _, c := range cases {
		if got, why := ShouldStop(c.in); got != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.name, got, why, c.want)
		}
	}
}
