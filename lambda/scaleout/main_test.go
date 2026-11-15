package main

import (
	"encoding/json"
	"testing"
)

func snsWrap(t *testing.T, inner string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"Records": []any{map[string]any{"Sns": map[string]any{"Message": inner}}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseEvent(t *testing.T) {
	allowed := alarmSet("elastic-gcs-scaleout-us-west-2-src, elastic-gcs-scaleout-throughput-us-west-2-src")
	cases := []struct {
		name       string
		raw        json.RawMessage
		wantIgnore bool
		wantTarget int
	}{
		{"scale-out alarm", snsWrap(t, `{"AlarmName":"elastic-gcs-scaleout-us-west-2-src","NewStateValue":"ALARM"}`), false, 0},
		{"throughput alarm", snsWrap(t, `{"AlarmName":"elastic-gcs-scaleout-throughput-us-west-2-src","NewStateValue":"ALARM"}`), false, 0},
		{"version drift alarm (#27)", snsWrap(t, `{"AlarmName":"elastic-gcs-version-drift-us-west-2-src","NewStateValue":"ALARM"}`), true, 0},
		{"other deployment's alarm", snsWrap(t, `{"AlarmName":"elastic-gcs-scaleout-us-west-2-dst","NewStateValue":"ALARM"}`), true, 0},
		{"OK transition", snsWrap(t, `{"AlarmName":"elastic-gcs-scaleout-us-west-2-src","NewStateValue":"OK"}`), true, 0},
		{"unparseable SNS", snsWrap(t, `not json`), true, 0},
		{"SNS manual target", snsWrap(t, `{"source":"manual","target_dtn":3}`), false, 3},
		{"direct manual", json.RawMessage(`{"source":"manual","target_dtn":2}`), false, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := parseEvent(c.raw, allowed)
			if ev.Ignore != c.wantIgnore || ev.TargetDTN != c.wantTarget {
				t.Fatalf("got ignore=%v target=%d, want ignore=%v target=%d (%+v)", ev.Ignore, ev.TargetDTN, c.wantIgnore, c.wantTarget, ev)
			}
		})
	}
}
