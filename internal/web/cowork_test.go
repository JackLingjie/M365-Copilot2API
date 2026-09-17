package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/cowork"
)

func TestCoworkRejectsUnsupportedCapabilitiesBeforeAuthentication(t *testing.T) {
	t.Setenv("M365_COWORK_BASE_URL", "https://example.test")
	for _, body := range []string{
		`{"model":"cowork-opus-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"x"}}]}`,
		`{"model":"fable-5.1","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/image"}}]}]}`,
	} {
		s := &Server{}
		w := httptest.NewRecorder()
		s.openaiChat(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}
}
func TestCoworkSelectsOnlyDiscoveredModelsAndEfforts(t *testing.T) {
	reg := cowork.Registry{Models: []cowork.Model{{ID: "melon", SupportedEfforts: []string{"low", "max"}}, {ID: "future-model:copilot", SupportedEfforts: []string{"high"}}}}
	for _, tc := range []struct {
		name, effort, id string
		bad              bool
	}{
		{"fable-5.1", "max", "melon", false}, {"cowork-fable-5.1", "", "melon", false}, {"cowork-melon", "low", "melon", false},
		{"cowork-future-model:copilot", "high", "future-model:copilot", false}, {"cowork-auto", "", "", false},
		{"cowork-auto", "high", "", true}, {"cowork-melon", "xhigh", "", true}, {"cowork-missing", "", "", true},
	} {
		id, err := selectCoworkModel(reg, tc.name, tc.effort)
		if (err != nil) != tc.bad || id != tc.id {
			t.Fatalf("%+v got %s %v", tc, id, err)
		}
	}
	if _, err := selectCoworkModel(cowork.Registry{}, "fable-5.1", ""); err == nil {
		t.Fatal("legacy alias bypassed registry")
	}
	for _, m := range coworkCatalog(reg) {
		if m["supports_tools"] != false || m["supports_vision"] != false || m["context_window"] != nil {
			t.Fatal("unsupported capabilities advertised")
		}
	}
	if isCoworkModel("gpt-5.5") || !isCoworkModel("cowork-unknown") {
		t.Fatal("Cowork and ChatHub routes collide")
	}
}
func TestCoworkDiscoveryCacheIsolationRoutingAndAdminTest(t *testing.T) {
	var lists, posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			lists.Add(1)
			fmt.Fprintf(w, `{"models":[{"id":"model-%s","display_name":"Test","supported_effort_levels":["low"]}]}`, r.Header.Get("x-user-id"))
		case "/v1/subscribe":
			if r.Header.Get("x-container-config") != "model=model-u-1;reasoningEffort=low" && r.Header.Get("x-container-config") != "model=model-u-1" {
				t.Errorf("bad config %s", r.Header.Get("x-container-config"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: fr\ndata: {\"content\":\"OK\",\"stop\":\"end_turn\"}\n\n")
		case "/v1/messages":
			posts.Add(1)
			w.WriteHeader(202)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("M365_COWORK_BASE_URL", srv.URL)
	s := &Server{tokens: testAccountFiles(t), settings: &settingsStore{v: runtimeSettings{ChatTimeoutSeconds: 5}}, coworkAccounts: map[string]*coworkAccountState{}}
	for _, id := range []string{"u-1", "u-2"} {
		s.coworkAccounts[id+"\x00"+srv.URL+"\x00"] = &coworkAccountState{gate: make(chan struct{}, 1), token: auth.TokenSet{AccessToken: "token", HomeOID: id, TenantID: "tenant", ExpiresAt: time.Now().Add(time.Hour)}}
	}
	for _, id := range []string{"u-1", "u-1", "u-2"} {
		data, err := s.catalogForRequest(httptest.NewRequest("GET", "/v1/models?account_id="+id, nil))
		if err != "" {
			t.Fatal(err)
		}
		found := false
		for _, m := range data {
			if m["id"] == "cowork-model-"+id {
				found = true
			}
		}
		if !found {
			t.Fatalf("account %s model missing", id)
		}
	}
	if lists.Load() != 2 {
		t.Fatal("account discovery cache broken")
	}
	for _, stream := range []bool{false, true} {
		body := fmt.Sprintf(`{"model":"cowork-model-u-1","accountId":"u-1","reasoning_effort":"low","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream)
		w := httptest.NewRecorder()
		s.openaiChat(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "OK") || (stream && !strings.Contains(w.Body.String(), "[DONE]")) {
			t.Fatalf("chat failed %d %s", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s.adminModelTest(w, httptest.NewRequest("POST", "/api/models/test", strings.NewReader(`{"model":"cowork-model-u-1","account_id":"u-1"}`)))
	var d map[string]any
	json.Unmarshal(w.Body.Bytes(), &d)
	if w.Code != 200 || d["reply"] != "OK" {
		t.Fatalf("admin test: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	s.adminModelTest(w, httptest.NewRequest("POST", "/api/models/test", strings.NewReader(`{"model":"cowork-missing","account_id":"u-1"}`)))
	if w.Code != 400 || posts.Load() != 3 {
		t.Fatalf("unknown model bypass: %d posts %d", w.Code, posts.Load())
	}
	// Waiting for the same account must honor cancellation.
	state := s.coworkAccounts["u-1\x00"+srv.URL+"\x00"]
	state.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	acc, _ := s.tokens.Get("u-1")
	if _, _, _, err := s.coworkSession(ctx, acc); err == nil {
		t.Fatal("ignored cancellation")
	}
	<-state.gate
}
