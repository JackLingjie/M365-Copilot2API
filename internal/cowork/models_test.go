package cowork

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistryPreservesNewModelsAndContainerIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer token" {
			t.Error("incorrect discovery request")
		}
		if strings.Contains(r.Header.Get("x-container-config"), "model=") || strings.Contains(r.Header.Get("x-container-config"), "reasoningEffort=") {
			t.Error("discovery leaked captured selection")
		}
		fmt.Fprint(w, `{"models":[{"id":"new-model:copilot","alias":"new-model","display_name":"New model","provider":"new-provider","supported_effort_levels":["low","high"],"default_effort_level":"high"},{"alias":"next-model"}]}`)
	}))
	defer srv.Close()
	reg, err := (&Client{BaseURL: srv.URL}).ListModels(context.Background(), Request{Token: "token", TenantID: "tenant", UserID: "user", Config: "model=old;reasoningEffort=max"})
	if err != nil || len(reg.Models) != 2 || reg.Models[0].ID != "new-model:copilot" || reg.Models[1].ID != "next-model" {
		t.Fatalf("registry: %+v %v", reg, err)
	}
	for _, tc := range []struct{ model, effort, header string }{{"new-model:copilot", "low", "feature=true;model=new-model:copilot;reasoningEffort=low"}, {"", "", "feature=true"}} {
		req, err := (&Client{BaseURL: srv.URL}).request(context.Background(), Request{Token: "token", TenantID: "tenant", UserID: "user", Model: tc.model, Effort: tc.effort, Config: "model=old;feature=true;reasoningEffort=max"}, "POST", "/v1/messages", "tenant:user:session", nil)
		if err != nil || req.Header.Get("x-container-config") != tc.header {
			t.Fatalf("incorrect selection: %v %v", req, err)
		}
	}
}
func TestRegistryErrorsAndRedirects(t *testing.T) {
	for _, body := range []string{`{}`, `{"models":[{"id":"injected;model=x"}]}`, `{"models":[{"id":"same"},{"id":"same"}]}`, `not json`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		_, err := (&Client{BaseURL: srv.URL}).ListModels(context.Background(), Request{Token: "secret", TenantID: "t", UserID: "u"})
		srv.Close()
		if err == nil {
			t.Fatalf("accepted malformed registry %s", body)
		}
	}
	for _, status := range []int{302, 401, 403, 429, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/credentials-must-not-follow")
			w.WriteHeader(status)
			fmt.Fprint(w, "sensitive response")
		}))
		_, err := (&Client{BaseURL: srv.URL}).ListModels(context.Background(), Request{Token: "secret", TenantID: "t", UserID: "u"})
		srv.Close()
		if err == nil || strings.Contains(err.Error(), "sensitive") {
			t.Fatalf("status %d: %v", status, err)
		}
	}
}
