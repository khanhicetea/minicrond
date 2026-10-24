package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestExhaustionVisibleInListDetailStatesAndStatus(t *testing.T) {
	s, token, st := setup(t)
	d, err := st.PutDefinition(t.Context(), model.Definition{Name: "queue", Kind: model.KindWorker, Command: "false"}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	outcome := model.WorkerExhaustion{Reason: "restart attempts exhausted", Attempts: 5, LastRunID: "last-worker-run", Revision: d.Revision, At: time.Now().UTC()}
	if err := st.SaveWorkerExhaustion(t.Context(), d, outcome); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/jobs", "/api/v1/jobs/queue", "/api/v1/workers/states", "/api/v1/daemon"} {
		rec := call(s, false, "GET", path, token, "", nil)
		if rec.Code != 200 || !json.Valid(rec.Body.Bytes()) || !strings.Contains(rec.Body.String(), outcome.LastRunID) || !strings.Contains(rec.Body.String(), outcome.Reason) {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	rec := call(s, true, "POST", "/api/v1/workers/queue/start", "", "", nil)
	if rec.Code != 202 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	saved, err := st.WorkerExhaustions(t.Context())
	if err != nil || len(saved) != 0 {
		t.Fatalf("start did not clear: %v %v", saved, err)
	}
}

// Config-owned workers accept lifecycle actions (they are runtime control,
// not edits); editing them stays refused.
func TestConfigOwnedWorkerAcceptsLifecycleActions(t *testing.T) {
	s, token, st := setup(t)
	enabled := true
	def := model.Definition{Name: "svc", Kind: model.KindWorker, Command: "sleep 30", Shell: "/bin/sh", Enabled: &enabled, Source: "config",
		Restart: "always", RestartDelay: 1, HealthyAfter: 30, MaxRestartAttempts: 5, Grace: 1, SuccessCodes: []int{0}, Timezone: "UTC"}
	if err := st.SyncConfigDefinitions(t.Context(), []model.Definition{def}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"start", "restart", "stop"} {
		rec := call(s, false, "POST", "/api/v1/workers/svc/"+action, token, "", nil)
		if rec.Code != 202 {
			t.Fatalf("%s: %d %s", action, rec.Code, rec.Body.String())
		}
	}
	rec := call(s, false, "DELETE", "/api/v1/jobs/svc", token, "", nil)
	if rec.Code != 403 {
		t.Fatalf("delete of a config-owned worker: %d %s", rec.Code, rec.Body.String())
	}
}
