package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/cowork"
	"m365-copilot2api/internal/outbound"
)

type coworkAccountState struct {
	gate      chan struct{}
	proxyHTTP *http.Client
	token     auth.TokenSet
	registry  cowork.Registry
	fetched   time.Time
}

// Reserve the namespace even when disabled/unknown: never silently route a
// misspelled Cowork model through ChatHub's default tone.
func isCoworkModel(model string) bool {
	return strings.HasPrefix(model, "cowork-") || model == "fable-5.1"
}
func coworkEnabled() bool { return strings.TrimSpace(os.Getenv("M365_COWORK_BASE_URL")) != "" }

func (s *Server) coworkSession(ctx context.Context, acc auth.AccountToken) (*cowork.Client, cowork.Request, cowork.Registry, error) {
	base := strings.TrimSpace(os.Getenv("M365_COWORK_BASE_URL"))
	client := &cowork.Client{BaseURL: base, HTTP: outbound.HTTPClient()}
	in := cowork.Request{TenantID: acc.TID, UserID: acc.OID, Config: os.Getenv("M365_COWORK_CONTAINER_CONFIG"), TimeZone: os.Getenv("M365_COWORK_TIMEZONE")}
	var empty cowork.Registry
	if base == "" {
		return nil, in, empty, errors.New("Cowork is not configured")
	}
	key := acc.ID + "\x00" + base + "\x00" + acc.BoundProxy
	s.coworkMu.Lock()
	if s.coworkAccounts == nil {
		s.coworkAccounts = map[string]*coworkAccountState{}
	}
	state := s.coworkAccounts[key]
	if state == nil {
		state = &coworkAccountState{gate: make(chan struct{}, 1)}
		s.coworkAccounts[key] = state
	}
	s.coworkMu.Unlock()
	select {
	case state.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, in, empty, ctx.Err()
	}
	defer func() { <-state.gate }()
	if ctx.Err() != nil {
		return nil, in, empty, ctx.Err()
	}
	if acc.BoundProxy != "" {
		if state.proxyHTTP == nil {
			clients, err := outbound.New(acc.BoundProxy)
			if err != nil {
				return nil, in, empty, errors.New("invalid Cowork account proxy")
			}
			state.proxyHTTP = clients.HTTP
		}
		client.HTTP = state.proxyHTTP
	}
	if !state.token.Valid() {
		if fresh, ok := s.tokens.Get(acc.ID); ok {
			acc = fresh
		}
		// This token has a different OAuth audience from ChatHub. Persist the
		// rotated refresh token, but keep the Cowork access token in memory only.
		token, err := auth.RefreshWithScopeContext(ctx, client.HTTP, acc.RefreshToken, acc.ClientID, cowork.Scope)
		if err != nil {
			return nil, in, empty, errors.New("Cowork authentication failed; the account must have Cowork access")
		}
		if token.RefreshToken != "" {
			if s.tokens.UpdateRefreshToken(acc.ID, token.RefreshToken) != nil {
				return nil, in, empty, errors.New("could not persist refreshed Cowork credentials")
			}
		}
		state.token = token
	}
	in.Token = state.token.AccessToken
	if state.token.HomeOID != "" {
		in.UserID = state.token.HomeOID
	}
	if state.token.TenantID != "" {
		in.TenantID = state.token.TenantID
	}
	if in.UserID == "" || in.TenantID == "" {
		in.UserID, in.TenantID = extractOIDTID(in.Token)
	}
	if time.Since(state.fetched) >= 5*time.Minute {
		reg, err := client.ListModels(ctx, in)
		if err != nil {
			var se *cowork.StatusError
			if errors.As(err, &se) && se.Status == http.StatusUnauthorized {
				state.token = auth.TokenSet{}
			}
			return nil, in, empty, err
		}
		state.registry, state.fetched = reg, time.Now()
	}
	return client, in, state.registry, nil
}

func (s *Server) invalidateCoworkToken(acc auth.AccountToken, token string) {
	key := acc.ID + "\x00" + strings.TrimSpace(os.Getenv("M365_COWORK_BASE_URL")) + "\x00" + acc.BoundProxy
	s.coworkMu.Lock()
	state := s.coworkAccounts[key]
	s.coworkMu.Unlock()
	if state == nil {
		return
	}
	// Avoid blocking a completed request behind another refresh. Expiry still
	// handles the rare case where a refresh/discovery is already in progress.
	select {
	case state.gate <- struct{}{}:
	default:
		return
	}
	defer func() { <-state.gate }()
	if state.token.AccessToken == token {
		state.token = auth.TokenSet{}
		state.fetched = time.Time{}
	}
}

func selectCoworkModel(reg cowork.Registry, name, effort string) (string, error) {
	if name == "cowork-auto" {
		if len(reg.Models) == 0 {
			return "", errors.New("no Cowork models are available for this account")
		}
		if effort != "" {
			return "", errors.New("Cowork Auto uses the selected model's default effort; omit reasoning_effort")
		}
		return "", nil
	}
	id := strings.TrimPrefix(name, "cowork-")
	// Compatibility aliases are usable only if melon is actually in the registry.
	if name == "fable-5.1" || name == "cowork-fable-5.1" {
		id = "melon"
	}
	for _, m := range reg.Models {
		if m.ID != id {
			continue
		}
		if effort != "" && !slices.Contains(m.SupportedEfforts, effort) {
			return "", errors.New("reasoning_effort is not supported by this Cowork model")
		}
		return m.ID, nil
	}
	return "", errors.New("unknown or unavailable Cowork model; refresh /v1/models for this account")
}

func (s *Server) catalogForRequest(r *http.Request) ([]map[string]any, string) {
	data := modelCatalog()
	if !coworkEnabled() {
		return data, ""
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	accountID := firstNonEmpty(r.URL.Query().Get("account_id"), r.URL.Query().Get("accountId"))
	acc, err := s.coworkAccount(accountID)
	if err != nil {
		return data, err.Error()
	}
	_, _, reg, err := s.coworkSession(ctx, acc)
	if err != nil {
		return data, err.Error()
	}
	return append(data, coworkCatalog(reg)...), ""
}

// Cowork obtains its own OAuth audience; model listing and chat must not
// trigger an unrelated ChatHub token refresh or health probe.
func (s *Server) coworkAccount(id string) (auth.AccountToken, error) {
	if id != "" {
		if acc, ok := s.tokens.Get(id); ok {
			return acc, nil
		}
		return auth.AccountToken{}, errors.New("Cowork account not found")
	}
	for _, acc := range s.tokens.List() {
		if !acc.ScheduleDisabled && acc.RefreshToken != "" {
			return acc, nil
		}
	}
	return auth.AccountToken{}, errors.New("no Cowork account is configured")
}

func coworkCatalog(reg cowork.Registry) []map[string]any {
	entries := []map[string]any{}
	if len(reg.Models) == 0 {
		return entries
	}
	entries = append(entries, coworkCatalogEntry("cowork-auto", cowork.Model{DisplayName: "Auto", Description: "Cowork automatic model selection"}))
	for _, m := range reg.Models {
		entries = append(entries, coworkCatalogEntry("cowork-"+m.ID, m))
		if m.ID == "melon" {
			alias := coworkCatalogEntry("fable-5.1", m)
			alias["alias_of"] = "cowork-melon"
			entries = append(entries, alias)
		}
	}
	return entries
}
func coworkCatalogEntry(id string, m cowork.Model) map[string]any {
	levels := make([]reasoningEffortPreset, 0, len(m.SupportedEfforts))
	for _, e := range m.SupportedEfforts {
		levels = append(levels, reasoningEffortPreset{Effort: e, Description: e})
	}
	return map[string]any{
		"id": id, "slug": id, "object": "model", "owned_by": "microsoft-365-cowork", "display_name": m.DisplayName + " (Cowork)", "description": m.Description,
		"upstream_model": m.ID, "provider": m.Provider, "supported_in_api": true,
		"supports_tools": false, "tool_calls": false, "function_calling": false, "supports_vision": false,
		"modalities": []string{"text"}, "input_modalities": []string{"text"}, "output_modalities": []string{"text"}, "supported_features": []string{"streaming"},
		"default_reasoning_level": m.DefaultEffort, "supported_reasoning_levels": levels,
		"capabilities": map[string]any{"chat_completions": true, "responses": true, "streaming": true, "tools": false, "vision": false, "reasoning": len(levels) > 0, "supported_reasoning_levels": levels},
	}
}
