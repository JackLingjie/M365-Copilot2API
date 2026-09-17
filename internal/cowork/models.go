package cowork

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Model is the account-scoped registry entry returned by Cowork /v1/models.
// IDs may include a container suffix; preserve them verbatim when routing.
type Model struct {
	ID               string   `json:"id"`
	Alias            string   `json:"alias"`
	DisplayName      string   `json:"display_name"`
	Description      string   `json:"description"`
	Provider         string   `json:"provider"`
	DefaultEffort    string   `json:"default_effort_level"`
	SupportedEfforts []string `json:"supported_effort_levels"`
}
type Registry struct {
	Models  []Model `json:"models"`
	Default string  `json:"default"`
}

var modelID = regexp.MustCompile(`^[a-z0-9.-]+(?::[a-z0-9.-]+)?$`)

func ValidModelID(id string) bool { return modelID.MatchString(id) }
func (m Model) SupportsEffort(effort string) bool {
	for _, e := range m.SupportedEfforts {
		if e == effort {
			return true
		}
	}
	return false
}

func (c *Client) httpClient() *http.Client {
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	local := *client
	// Credentials must never follow a redirect to another service.
	local.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &local
}

func (c *Client) request(ctx context.Context, in Request, method, path, conversationID string, body io.Reader) (*http.Request, error) {
	if in.Token == "" || in.TenantID == "" || in.UserID == "" {
		return nil, errors.New("incomplete Cowork credentials")
	}
	if in.Model != "" && !ValidModelID(in.Model) {
		return nil, errors.New("invalid Cowork model ID")
	}
	if strings.ContainsAny(in.Effort, ";=\r\n") {
		return nil, errors.New("invalid Cowork reasoning effort")
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return nil, errors.New("invalid Cowork endpoint")
	}
	config := []string{}
	for _, part := range strings.Split(in.Config, ";") {
		part = strings.TrimSpace(part)
		key, _, _ := strings.Cut(part, "=")
		if part != "" && key != "model" && key != "reasoningEffort" {
			config = append(config, part)
		}
	}
	// Auto omits model entirely, matching the browser. A captured selection or
	// effort in the base config must not leak into another request.
	if in.Model != "" {
		config = append(config, "model="+in.Model)
	}
	if in.Effort != "" {
		config = append(config, "reasoningEffort="+in.Effort)
	}
	req.Header.Set("Authorization", "Bearer "+in.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://copilot.cloud.microsoft")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-container-config", strings.Join(config, ";"))
	if conversationID != "" {
		req.Header.Set("x-conversation-id", conversationID)
	}
	req.Header.Set("x-tenant-id", in.TenantID)
	req.Header.Set("x-user-id", in.UserID)
	req.Header.Set("x-request-id", uuid.NewString())
	if in.TimeZone != "" {
		req.Header.Set("x-copilot-timezone", in.TimeZone)
	}
	return req, nil
}

func (c *Client) ListModels(ctx context.Context, in Request) (Registry, error) {
	var out Registry
	in.Model, in.Effort = "", ""
	req, err := c.request(ctx, in, http.MethodGet, "/v1/models", "", nil)
	if err != nil {
		return out, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return out, errors.New("Cowork model discovery connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, &StatusError{"models", resp.StatusCode}
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil || out.Models == nil {
		return Registry{}, errors.New("invalid Cowork model registry")
	}
	seen := map[string]bool{}
	for i := range out.Models {
		m := &out.Models[i]
		if m.ID == "" {
			m.ID = m.Alias
		}
		if !ValidModelID(m.ID) || seen[m.ID] {
			return Registry{}, errors.New("invalid or duplicate Cowork model ID")
		}
		seen[m.ID] = true
		if m.DisplayName == "" {
			m.DisplayName = m.Alias
		}
		if m.DisplayName == "" {
			m.DisplayName = m.ID
		}
	}
	return out, nil
}
