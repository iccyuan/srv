package jobnotify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"srv/internal/jobs"
	"testing"
)

func TestForJobCopiesFields(t *testing.T) {
	j := &jobs.Record{
		ID: "abc", Profile: "prod", Cmd: "sleep 1", Pid: 42,
		Log: "/x.log", Started: "S", Finished: "F",
	}
	p := ForJob(j)
	if p.ID != "abc" || p.Profile != "prod" || p.Cmd != "sleep 1" ||
		p.Pid != 42 || p.Log != "/x.log" || p.Started != "S" || p.Finished != "F" {
		t.Errorf("ForJob copy mismatch: %+v", p)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 80); got != "short" {
		t.Errorf("untouched: %q", got)
	}
	if got := truncate("abcdefghij", 7); got != "abcd..." {
		t.Errorf("truncated: %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc" {
		t.Errorf("tiny limit: %q", got)
	}
}

func TestWebhookEmptyURL(t *testing.T) {
	if err := Webhook("", Payload{ID: "x"}); err == nil {
		t.Errorf("empty url should error")
	}
}

func TestWebhookPostsJSON(t *testing.T) {
	var got Payload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type: %s", ct)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	p := Payload{ID: "j1", Profile: "p", Cmd: "ls"}
	if err := Webhook(server.URL, p); err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	if got.ID != "j1" || got.Profile != "p" || got.Cmd != "ls" {
		t.Errorf("payload mismatch: %+v", got)
	}
}

func TestWebhookSurfacesNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	if err := Webhook(server.URL, Payload{ID: "x"}); err == nil {
		t.Errorf("expected error on 500")
	}
}
