package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type scopeTestTransport func(*http.Request) (*http.Response, error)

func (f scopeTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRefreshWithScopeUsesSuppliedClientAndContext(t *testing.T) {
	seen := false
	client := &http.Client{Transport: scopeTestTransport(func(r *http.Request) (*http.Response, error) {
		seen = true
		if r.ParseForm() != nil || r.Form.Get("scope") != "test-resource/.default" || r.Form.Get("client_id") != "test-client" || r.Form.Get("refresh_token") != "test-refresh" {
			t.Error("scope refresh form not forwarded")
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := RefreshWithScopeContext(ctx, client, "test-refresh", "test-client", "test-resource/.default"); err == nil || !seen {
		t.Fatal("custom transport or cancellation ignored")
	}
}
func TestScopeRefreshDoesNotFollowRedirects(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: scopeTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": []string{"https://untrusted.example/token"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})}
	if _, err := RefreshWithScopeContext(context.Background(), client, "test-refresh", "test-client", "test-resource/.default"); err == nil || calls != 1 {
		t.Fatal("refresh followed credential redirect")
	}
}
