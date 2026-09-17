package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"m365-copilot2api/internal/cowork"
)

func validateCoworkRequest(body *oaiReq) error {
	if len(body.Tools) > 0 || len(body.Functions) > 0 || body.ToolChoice != nil || body.FunctionCall != nil {
		return errors.New("Cowork client tool calls are not yet supported")
	}
	if len(body.Attachments) > 0 {
		return errors.New("Cowork attachments are not yet supported")
	}
	for _, m := range body.Messages {
		if m.Role == "tool" || m.Role == "function" || m.ToolCallID != "" || len(m.ToolCalls) > 0 {
			return errors.New("Cowork tool-result messages are not yet supported")
		}
		switch c := m.Content.(type) {
		case nil, string:
		case []any:
			for _, p := range c {
				v, ok := p.(map[string]any)
				if !ok {
					return errors.New("invalid Cowork message content")
				}
				typ, _ := v["type"].(string)
				if typ != "text" && typ != "input_text" && typ != "output_text" {
					return errors.New("Cowork currently accepts text content only")
				}
			}
		default:
			return errors.New("invalid Cowork message content")
		}
	}
	if body.ResponseFormat != nil {
		return errors.New("Cowork structured output formats are not yet supported")
	}
	return nil
}

func (s *Server) coworkChat(w http.ResponseWriter, r *http.Request, body *oaiReq) {
	if !coworkEnabled() {
		writeOpenAIError(w, 503, "configuration_error", "Cowork is not configured")
		return
	}
	if err := validateCoworkRequest(body); err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}
	prompt, _ := flattenPromptMessages(body.Messages, nil)
	if strings.TrimSpace(prompt) == "" {
		writeOpenAIError(w, 400, "invalid_request_error", "messages required")
		return
	}
	acc, err := s.coworkAccount(body.AccountID)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	release, err := s.accountConcurrency.Acquire(ctx, acc.ID)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	defer release()
	client, in, registry, err := s.coworkSession(ctx, acc)
	if err != nil {
		writeCoworkError(w, ctx, err)
		return
	}
	effort := body.ReasoningEffort
	if body.Reasoning != nil && body.Reasoning.Effort != "" {
		effort = body.Reasoning.Effort
	}
	upstream, err := selectCoworkModel(registry, body.Model, effort)
	if err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}
	in.Model, in.Effort = upstream, effort
	in.Text = "Respond in chat to the conversation below. Do not use tools, files, connectors, search, or browser.\n\n" + prompt
	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	started := false
	emit := func(delta map[string]any, finish any) error {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			started = true
		}
		chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": body.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		b, _ := json.Marshal(chunk)
		if _, e := fmt.Fprintf(w, "data: %s\n\n", b); e != nil {
			return e
		}
		return http.NewResponseController(w).Flush()
	}
	var onDelta func(string) error
	if body.Stream {
		onDelta = func(t string) error {
			if !started {
				if e := emit(map[string]any{"role": "assistant"}, nil); e != nil {
					return e
				}
			}
			return emit(map[string]any{"content": t}, nil)
		}
	}
	res, err := client.Chat(ctx, in, onDelta)
	if err != nil {
		var se *cowork.StatusError
		if errors.As(err, &se) && se.Status == http.StatusUnauthorized {
			s.invalidateCoworkToken(acc, in.Token)
		}
		if started {
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "cowork_error", "message": err.Error()}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			http.NewResponseController(w).Flush()
			return
		}
		writeCoworkError(w, ctx, err)
		return
	}
	finish := "stop"
	if res.Stop == "max_tokens" {
		finish = "length"
	}
	if body.Stream {
		if emit(map[string]any{}, finish) == nil {
			fmt.Fprint(w, "data: [DONE]\n\n")
			http.NewResponseController(w).Flush()
		}
		return
	}
	estimate := estimateResponsesUsage(body.Model, body.Messages, nil, nil, res.Text)
	usage := map[string]any{"prompt_tokens": estimate.Values["input_tokens"], "completion_tokens": estimate.Values["output_tokens"], "total_tokens": estimate.Values["total_tokens"]}
	jsonOut(w, map[string]any{"id": id, "object": "chat.completion", "created": created, "model": body.Model, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": res.Text}, "finish_reason": finish}}, "usage": usage, "m365": map[string]any{"backend": "cowork", "upstream_model": upstream, "session_id": res.SessionID, "usage_source": "local_estimate"}})
}

func writeCoworkError(w http.ResponseWriter, ctx context.Context, err error) {
	status := http.StatusBadGateway
	var se *cowork.StatusError
	if errors.As(err, &se) && se.Status == http.StatusTooManyRequests {
		status = http.StatusTooManyRequests
	}
	if ctx.Err() != nil {
		status = http.StatusGatewayTimeout
	}
	writeOpenAIError(w, status, "cowork_error", err.Error())
}
