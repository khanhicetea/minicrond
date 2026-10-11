package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkerCommands(t *testing.T) {
	for _, action := range []string{"start", "stop", "restart", "delete", "runs"} {
		t.Run(action, func(t *testing.T) {
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				method, path := "POST", "/api/v1/workers/queue/"+action
				if action == "delete" {
					method, path = "DELETE", "/api/v1/jobs/queue"
				}
				if action == "runs" {
					method, path = "GET", "/api/v1/runs"
					if r.URL.Query().Get("job") != "queue" || r.URL.Query().Get("limit") != "7" {
						t.Error(r.URL)
					}
				}
				if r.Method != method || r.URL.Path != path {
					t.Errorf("%s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer server.Close()
			t.Setenv("MINICRON_URL", server.URL)
			defer closeClient()
			var err error
			if action == "runs" {
				err = recentRuns([]string{"queue", "--limit", "7"})
			} else {
				err = definitionAction(action, []string{"queue"})
			}
			if err != nil || !called {
				t.Fatalf("called=%v err=%v", called, err)
			}
		})
	}
	for _, args := range [][]string{nil, {"queue", "--limit", "0"}, {"queue", "--limit", "bad"}, {"../queue"}} {
		if recentRuns(args) == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
