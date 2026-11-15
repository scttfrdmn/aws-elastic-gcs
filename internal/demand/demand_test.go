package demand

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListPaginates(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Query().Get("filter_endpoint") != "ep" || r.URL.Query().Get("filter_status") != "ACTIVE" {
			http.Error(w, "bad request", 400)
			return
		}
		calls = append(calls, r.URL.Query().Get("last_key"))
		var p page
		switch r.URL.Query().Get("last_key") {
		case "":
			p = page{DATA: []Task{{TaskID: "a", EffectiveBytesPerS: 100}, {TaskID: "b", EffectiveBytesPerS: 50}}, HasNextPage: true, LastKey: "k1"}
		case "k1":
			p = page{DATA: []Task{{TaskID: "c", EffectiveBytesPerS: 25}}}
		}
		_ = json.NewEncoder(w).Encode(p)
	}))
	defer srv.Close()

	tasks, err := Lister{BaseURL: srv.URL, Token: "tok", Limit: 2}.List(context.Background(), "ep", "ACTIVE")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 || len(calls) != 2 || calls[1] != "k1" {
		t.Fatalf("tasks=%v calls=%v", tasks, calls)
	}
	if s := Aggregate(tasks); s.Count != 3 || s.TotalBytesPerSec != 175 {
		t.Fatalf("aggregate %+v", s)
	}
}

func TestListHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 403) }))
	defer srv.Close()
	if _, err := (Lister{BaseURL: srv.URL}).List(context.Background(), "ep", "ACTIVE"); err == nil {
		t.Fatal("want error")
	}
}
