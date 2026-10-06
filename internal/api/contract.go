package api

import (
	"encoding/json"
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

type contractRoute struct{ method, path, summary string }

var contractRoutes = []contractRoute{
	{"GET", "/healthz", "Liveness"}, {"GET", "/readyz", "Readiness"}, {"GET", "/api/v1/daemon", "Daemon metadata"}, {"POST", "/api/v1/daemon/reload", "Atomic configuration reload"},
	{"GET", "/api/v1/jobs", "List definitions"}, {"POST", "/api/v1/jobs", "Create DB definition"}, {"GET", "/api/v1/jobs/{name}", "Definition detail"}, {"PUT", "/api/v1/jobs/{name}", "Replace DB definition"}, {"DELETE", "/api/v1/jobs/{name}", "Delete DB definition"},
	{"POST", "/api/v1/jobs/{name}/trigger", "Trigger job"}, {"POST", "/api/v1/jobs/{name}/enable", "Enable definition"}, {"POST", "/api/v1/jobs/{name}/disable", "Disable definition"},
	{"GET", "/api/v1/workers/states", "List worker states"}, {"POST", "/api/v1/workers/{name}/start", "Start worker"}, {"POST", "/api/v1/workers/{name}/stop", "Hold worker"}, {"POST", "/api/v1/workers/{name}/restart", "Restart worker"},
	{"GET", "/api/v1/monitor", "On-demand daemon and direct-child resource stats (Linux)"},
	{"GET", "/api/v1/metrics/runs", "Run metrics"}, {"GET", "/api/v1/metrics/alerts", "Alert delivery metrics"}, {"GET", "/api/v1/alert-channels", "Redacted database-backed alert channels"}, {"PUT", "/api/v1/alert-channels/{name}", "Save alert channel (literal credentials)"}, {"DELETE", "/api/v1/alert-channels/{name}", "Delete alert channel; optionally remove definition references"}, {"POST", "/api/v1/alert-channels/{name}/test", "Send test alert"}, {"GET", "/api/v1/runs/{id}/alerts", "Run alert deliveries"}, {"GET", "/api/v1/runs", "List runs"}, {"GET", "/api/v1/runs/{id}", "Run detail"}, {"POST", "/api/v1/runs/{id}/stop", "Stop run"}, {"GET", "/api/v1/runs/{id}/log", "Windowed tagged logs"}, {"GET", "/api/v1/runs/{id}/log/raw", "Raw log download"}, {"GET", "/api/v1/runs/{id}/log/stream", "Resumable SSE logs"},
	{"POST", "/api/v1/token/rotate", "Rotate bearer token"}, {"GET", "/api/v1/export", "Export definitions"}, {"POST", "/api/v1/import/preview", "Preview hash-bound import"}, {"POST", "/api/v1/import/apply", "Apply definition import"},
}

// busyRoutes can answer 503 with Retry-After when expensive-read admission is
// saturated or the request's work budget runs out (see readlimit.go).
var busyRoutes = map[string]bool{
	"GET /api/v1/monitor":           true,
	"GET /api/v1/metrics/runs":      true,
	"GET /api/v1/runs/{id}/log":     true,
	"GET /api/v1/runs/{id}/log/raw": true,
}

func busyResponses() map[string]*huma.Response {
	return map[string]*huma.Response{"503": {
		Description: "Expensive reads are at capacity or the request exceeded its work budget; retry after the Retry-After delay. Error codes: read_busy, read_timeout",
		Headers:     map[string]*huma.Param{"Retry-After": {Description: "Seconds to wait before retrying", Schema: &huma.Schema{Type: "integer"}}},
	}}
}

func OpenAPIContract(version string) []byte {
	config := huma.DefaultConfig("minicrond API", version)
	config.OpenAPIPath = ""
	config.DocsPath = ""
	config.SchemasPath = ""
	registry := humago.New(http.NewServeMux(), config)
	for _, route := range contractRoutes {
		operation := &huma.Operation{Method: route.method, Path: route.path, Summary: route.summary, OperationID: route.method + "-" + route.path, Errors: []int{400, 401, 404, 409, 422, 500}}
		if route.path == "/api/v1/alert-channels/{name}" {
			operation.Parameters = []*huma.Param{{Name: "name", In: "path", Required: true, Schema: &huma.Schema{Type: "string", Pattern: `^[a-z0-9][a-z0-9_.-]{0,99}$`}}}
			if route.method == "PUT" {
				operation.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/json": {Schema: huma.SchemaFromType(registry.OpenAPI().Components.Schemas, reflect.TypeFor[alertChannelInput]())}}}
			}
			if route.method == "DELETE" {
				operation.Parameters = append(operation.Parameters, &huma.Param{Name: "remove_from_definitions", In: "query", Description: "Atomically remove registry-owned job/worker references. Config-owned references must be edited in their file first. Queued alerts keep their original channel.", Schema: &huma.Schema{Type: "boolean", Default: false}})
			}
		}
		if busyRoutes[route.method+" "+route.path] {
			operation.Responses = busyResponses()
		}
		registry.OpenAPI().AddOperation(operation)
	}
	body, err := json.Marshal(registry.OpenAPI())
	if err != nil {
		return []byte(`{"openapi":"3.1.0"}`)
	}
	return body
}
