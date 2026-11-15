package scaling

import "testing"

func TestDesiredDTNs(t *testing.T) {
	const thr = 1_093_750_000 // 70% of c8in.2xlarge
	cases := []struct {
		active  int
		bps     int64
		max     int
		want    int
		comment string
	}{
		{0, 5e9, 8, 0, "idle"},
		{1, 0, 8, 1, "one task, no rate yet"},
		{3, thr, 8, 1, "exactly at threshold"},
		{3, thr + 1, 8, 2, "just over"},
		{8, 4 * thr, 8, 4, "four DTNs' worth"},
		{8, 20 * thr, 8, 8, "capped at max"},
		{2, 5e9, 0, 0, "no capacity configured"},
		{1, 5e9, 8, 5, "rounds up"},
	}
	for _, c := range cases {
		if got := DesiredDTNs(c.active, c.bps, thr, c.max); got != c.want {
			t.Errorf("%s: DesiredDTNs(%d,%d,%d)=%d want %d", c.comment, c.active, c.bps, c.max, got, c.want)
		}
	}
}

func TestScaleUp(t *testing.T) {
	if n, ok := ScaleUp(1, 3); n != 3 || !ok {
		t.Fatal(n, ok)
	}
	if n, ok := ScaleUp(4, 2); n != 4 || ok {
		t.Fatal(n, ok)
	}
}

func TestAnyPositive(t *testing.T) {
	if AnyPositive(nil) || AnyPositive([]float64{0, 0}) || !AnyPositive([]float64{0, 2}) {
		t.Fatal("AnyPositive")
	}
}
