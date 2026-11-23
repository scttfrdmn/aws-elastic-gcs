package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestVersionReport(t *testing.T) {
	cases := []struct {
		name   string
		in     map[string]string
		expect string
		drift  []string
		agree  bool
	}{
		{"all match expected", map[string]string{"i-a": "5.4.101", "i-b": "5.4.101"}, "5.4.101", nil, true},
		{"all equal, no expectation", map[string]string{"i-a": "5.4.101", "i-b": "5.4.101"}, "", nil, true},
		{"one node behind", map[string]string{"i-a": "5.4.101", "i-b": "5.4.99"}, "5.4.101", []string{"i-b"}, false},
		{"mismatch without expectation", map[string]string{"i-a": "5.4.101", "i-b": "5.4.99"}, "", nil, false},
		{"unknown counts as drift", map[string]string{"i-a": "5.4.101", "i-b": "unknown"}, "", []string{"i-b"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			drift, agree := versionReport(c.in, c.expect)
			if !reflect.DeepEqual(drift, c.drift) || agree != c.agree {
				t.Fatalf("got drift=%v agree=%v, want %v %v", drift, agree, c.drift, c.agree)
			}
		})
	}
}

func TestExpectedGCSVersionFromCDKJSON(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cdk.json"), []byte(`{"context":{"globus:gcsVersion":"5.4.101"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}
	if v := expectedGCSVersion(); v != "5.4.101" {
		t.Fatalf("got %q", v)
	}
}
