package names

import "testing"

func TestNew(t *testing.T) {
	for _, tc := range []struct {
		region, name, wantName string
		ok                     bool
	}{
		{"us-west-2", "", DefaultDeployment, true},
		{"us-west-2", "src", "src", true},
		{"eu-central-1", "b2", "b2", true},
		{"us-gov-west-1", "a", "a", true},
		{"us-west-2", "Src", "", false},
		{"us-west-2", "2src", "", false},
		{"us-west-2", "src-a", "", false},
		{"us-west-2", "abcdefghijklm", "", false},
		{"us-west-2", "a;id", "", false},
		{"uswest2", "a", "", false},
		{"us-west-2;id", "a", "", false},
	} {
		d, err := New(tc.region, tc.name)
		if (err == nil) != tc.ok {
			t.Errorf("New(%q, %q) error = %v, want ok=%v", tc.region, tc.name, err, tc.ok)
			continue
		}
		if tc.ok && d.Name != tc.wantName {
			t.Errorf("New(%q, %q).Name = %q, want %q", tc.region, tc.name, d.Name, tc.wantName)
		}
	}
}

func TestNames(t *testing.T) {
	d, _ := New("us-west-2", "src")
	for got, want := range map[string]string{
		d.ID():                   "us-west-2-src",
		d.Resource("dtn"):        "elastic-gcs-dtn-us-west-2-src",
		d.Stack("Control"):       "ElasticGCS-Control-us-west-2-src",
		d.SecretsPrefix():        "elastic-gcs/us-west-2/src",
		d.Param(ParamEndpointID): "/elastic-gcs/us-west-2/src/endpoint-id",
		d.Dashboard():            "ElasticGCS-us-west-2-src",
		d.AMIParam("5.4.101"):    "/elastic-gcs/us-west-2/dtn-ami/gcs-5.4.101",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// TestLengths checks the longest derived names fit AWS limits at the maximum
// deployment name length in the longest commercial region name.
func TestLengths(t *testing.T) {
	d, err := New("ap-southeast-5", "abcdefghijkl")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{
		d.Resource("cache-eviction"), d.Resource("demand-monitor"), d.Resource("version-check"),
		d.Resource("idle-shutdown"), d.Resource("scaleout-throughput"), d.Resource("version-drift"),
		d.Resource("control"),
	} {
		if len(n) > 64 {
			t.Errorf("%q is %d chars, over the 64-char Lambda/IAM limit", n, len(n))
		}
	}
}
