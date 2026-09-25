package api

import (
	"strings"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestConfigOwnedDefinitionIsViewOnly(t *testing.T) {
	s, token, st := setup(t)
	def := model.Definition{Name: "internal", Kind: model.KindJob, Command: "true", Schedule: "@every 1m"}
	if err := st.SyncConfigDefinitions(t.Context(), []model.Definition{def}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/jobs", "/api/v1/jobs/internal"} {
		response := call(s, false, "GET", path, token, "", nil)
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"source":"config"`) {
			t.Fatalf("GET %s: %d %s", path, response.Code, response.Body.String())
		}
	}
	for _, request := range []struct{ method, path, body string }{
		{"PUT", "/api/v1/jobs/internal", `{"name":"internal","command":"false"}`},
		{"DELETE", "/api/v1/jobs/internal", ""},
		{"POST", "/api/v1/jobs/internal/disable", ""},
		{"POST", "/api/v1/jobs/internal/trigger", ""},
	} {
		response := call(s, false, request.method, request.path, token, request.body, nil)
		if response.Code != 403 {
			t.Fatalf("%s %s: %d %s", request.method, request.path, response.Code, response.Body.String())
		}
	}
}
