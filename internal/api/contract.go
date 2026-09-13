package api

import (
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

type contractRoute struct{ method, path, summary string }

var contractRoutes = []contractRoute{
	{"GET", "/healthz", "Liveness"}, {"GET", "/readyz", "Readiness"}, {"GET", "/api/v1/daemon", "Daemon metadata"}, {"POST", "/api/v1/daemon/reload", "Atomic configuration reload"},
	{"GET", "/api/v1/jobs", "List definitions"}, {"POST", "/api/v1/jobs", "Create DB definition"}, {"GET", "/api/v1/jobs/{name}", "Definition detail"}, {"PUT", "/api/v1/jobs/{name}", "Replace DB definition"}, {"DELETE", "/api/v1/jobs/{name}", "Delete DB definition"},
	{"POST", "/api/v1/jobs/{name}/trigger", "Trigger job"}, {"POST", "/api/v1/jobs/{name}/enable", "Enable definition"}, {"POST", "/api/v1/jobs/{name}/disable", "Disable definition"},
	{"POST", "/api/v1/workers/{name}/start", "Start worker"}, {"POST", "/api/v1/workers/{name}/stop", "Hold worker"}, {"POST", "/api/v1/workers/{name}/restart", "Restart worker"},
	{"GET", "/api/v1/metrics/runs", "Run metrics"}, {"GET", "/api/v1/runs", "List runs"}, {"GET", "/api/v1/runs/{id}", "Run detail"}, {"POST", "/api/v1/runs/{id}/stop", "Stop run"}, {"GET", "/api/v1/runs/{id}/log", "Windowed tagged logs"}, {"GET", "/api/v1/runs/{id}/log/raw", "Raw log download"}, {"GET", "/api/v1/runs/{id}/log/stream", "Resumable SSE logs"},
	{"POST", "/api/v1/token/rotate", "Rotate bearer token"}, {"GET", "/api/v1/export", "Export definitions"}, {"POST", "/api/v1/import/preview", "Preview hash-bound import"}, {"POST", "/api/v1/import/apply", "Apply DB-authority import"},
}

func OpenAPIContract(version string) []byte {
	config := huma.DefaultConfig("minicron API", version)
	config.OpenAPIPath = ""
	config.DocsPath = ""
	config.SchemasPath = ""
	registry := humago.New(http.NewServeMux(), config)
	for _, route := range contractRoutes {
		registry.OpenAPI().AddOperation(&huma.Operation{Method: route.method, Path: route.path, Summary: route.summary, OperationID: route.method + "-" + route.path, Errors: []int{400, 401, 404, 409, 422, 500}})
	}
	body, err := json.Marshal(registry.OpenAPI())
	if err != nil {
		return []byte(`{"openapi":"3.1.0"}`)
	}
	return body
}
