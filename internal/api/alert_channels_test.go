package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestAlertChannelContract(t *testing.T) {
	var contract struct {
		Paths map[string]map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(OpenAPIContract("test"), &contract); err != nil {
		t.Fatal(err)
	}
	path := contract.Paths["/api/v1/alert-channels/{name}"]
	body, err := json.Marshal(path["put"]["requestBody"])
	if err != nil || !strings.Contains(string(body), `"writeOnly":true`) || !strings.Contains(string(body), `"bot_token"`) {
		t.Fatalf("channel input contract = %s, %v", body, err)
	}
	parameters, err := json.Marshal(path["delete"]["parameters"])
	if err != nil || !strings.Contains(string(parameters), "remove_from_definitions") {
		t.Fatalf("delete contract = %s, %v", parameters, err)
	}
}

func TestAlertChannelCRUD(t *testing.T) {
	s, token, st := setup(t)
	var mu sync.Mutex
	var channels []model.AlertChannel
	load := func(ctx context.Context) error {
		entries, err := st.AlertChannels(ctx)
		if err != nil {
			return err
		}
		mu.Lock()
		channels = entries
		mu.Unlock()
		return nil
	}
	s.SetAlertChannels(func() []model.AlertChannel {
		mu.Lock()
		defer mu.Unlock()
		return append([]model.AlertChannel(nil), channels...)
	})
	s.SetAlertMutations(func(ctx context.Context, c model.AlertChannel) error {
		if err := st.PutAlertChannel(ctx, c, "api"); err != nil {
			return err
		}
		return load(ctx)
	}, func(ctx context.Context, name string, remove bool) error {
		if err := st.DeleteAlertChannel(ctx, name, remove, "api"); err != nil {
			return err
		}
		return load(ctx)
	})
	body := `{"type":"telegram","bot_token":"123:private_token","chat_id":"-123","batch_window":12,"disable_notification":true}`
	if got := call(s, false, "PUT", "/api/v1/alert-channels/ops", "", body, nil); got.Code != 401 {
		t.Fatalf("unauthorized save = %d", got.Code)
	}
	got := call(s, false, "PUT", "/api/v1/alert-channels/ops", token, body, nil)
	if got.Code != 200 || strings.Contains(got.Body.String(), "private_token") {
		t.Fatalf("save = %d %s", got.Code, got.Body.String())
	}
	got = call(s, false, "GET", "/api/v1/alert-channels", token, "", nil)
	if got.Code != 200 || strings.Contains(got.Body.String(), "private_token") || !strings.Contains(got.Body.String(), `"chat_id":"-123"`) || !strings.Contains(got.Body.String(), `"has_bot_token":true`) {
		t.Fatalf("list = %d %s", got.Code, got.Body.String())
	}
	got = call(s, false, "PUT", "/api/v1/alert-channels/ops", token, `{"type":"telegram","chat_id":"-456"}`, nil)
	if got.Code != 200 {
		t.Fatalf("edit = %d %s", got.Code, got.Body.String())
	}
	entries, err := st.AlertChannels(t.Context())
	if err != nil || entries[0].BotToken != "123:private_token" || entries[0].ChatID != "-456" {
		t.Fatal("edit did not retain credential")
	}
	if got := call(s, false, "POST", "/api/v1/jobs", token, `{"name":"job","command":"true","alerts":["ops"]}`, nil); got.Code != 200 {
		t.Fatalf("job = %d %s", got.Code, got.Body.String())
	}
	if got := call(s, false, "DELETE", "/api/v1/alert-channels/ops", token, "", nil); got.Code != 409 {
		t.Fatalf("referenced delete = %d %s", got.Code, got.Body.String())
	}
	if got := call(s, false, "DELETE", "/api/v1/alert-channels/ops?remove_from_definitions=true", token, "", nil); got.Code != 200 {
		t.Fatalf("delete = %d %s", got.Code, got.Body.String())
	}
	d, _, err := st.Definition(t.Context(), "job")
	if err != nil || len(d.Alerts) != 0 || d.Revision != 2 {
		t.Fatalf("reference removal = %+v, %v", d, err)
	}
	if got := call(s, false, "DELETE", "/api/v1/alert-channels/ops", token, "", nil); got.Code != 404 {
		t.Fatalf("missing delete = %d", got.Code)
	}
	if got := call(s, false, "POST", "/api/v1/jobs", token, `{"name":"job","command":"true","alerts":["ops"]}`, nil); got.Code != 422 {
		t.Fatalf("dangling reference = %d", got.Code)
	}
	for _, bad := range []string{
		`{"type":"telegram","chat_id":"123","bot_token":"env:private_token"}`,
		`{"type":"telegram","chat_id":"123","bot_token":123,"private_token":true}`,
		`{"type":"telegram","chat_id":"123","bot_token":"private_token`,
	} {
		got := call(s, false, "PUT", "/api/v1/alert-channels/bad", token, bad, nil)
		if got.Code != 422 || strings.Contains(got.Body.String(), "private_token") {
			t.Fatalf("validation leaked token: %d %s", got.Code, got.Body.String())
		}
	}
	// The read API never exposes credentials through a broad JSON envelope.
	var list struct {
		Items []map[string]any `json:"items"`
	}
	got = call(s, false, "GET", "/api/v1/alert-channels", token, "", nil)
	if err := json.Unmarshal(got.Body.Bytes(), &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("deleted list = %s", got.Body.String())
	}
}
