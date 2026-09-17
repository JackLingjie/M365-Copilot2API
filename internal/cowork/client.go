// Package cowork implements the Cowork HTTP/SSE protocol observed in Edge.
package cowork

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const Scope = "6ab48b67-cd74-4ad4-81af-5932984589be/.default"

type Client struct {
	BaseURL string
	HTTP    *http.Client
}
type Request struct{ Token, TenantID, UserID, Model, Effort, Config, TimeZone, Text string }
type Result struct{ Text, SessionID, ConversationID, Stop string }
type StatusError struct {
	Operation string
	Status    int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("Cowork %s returned HTTP %d", e.Operation, e.Status)
}

// Chat starts a fresh workspace per API call. The caller includes the complete
// message history, avoiding accidental cross-client conversation reuse.
func (c *Client) Chat(ctx context.Context, in Request, delta func(string) error) (Result, error) {
	var result Result
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return result, errors.New("invalid Cowork base URL")
	}
	result.SessionID = uuid.NewString()
	result.ConversationID = in.TenantID + ":" + in.UserID + ":" + result.SessionID
	client := c.httpClient()
	makeRequest := func(method, path string, body io.Reader) (*http.Request, error) {
		return c.request(ctx, in, method, path, result.ConversationID, body)
	}
	openStream := func(last string) (*http.Response, error) {
		req, e := makeRequest(http.MethodGet, "/v1/subscribe?"+url.Values{"conversationId": {result.ConversationID}}.Encode(), nil)
		if e != nil {
			return nil, e
		}
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("last-event-id", last)
		resp, e := client.Do(req)
		if e != nil {
			return nil, errors.New("Cowork subscription connection failed")
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return nil, &StatusError{"subscribe", resp.StatusCode}
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			resp.Body.Close()
			return nil, errors.New("Cowork subscription is not an event stream")
		}
		return resp, nil
	}
	resp, err := openStream("0")
	if err != nil {
		return result, err
	}
	defer func() {
		if resp != nil {
			resp.Body.Close()
		}
	}()
	body, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": in.Text}}, "conversationId": result.ConversationID, "messageId": uuid.NewString(), "queue": true, "role": "user"})
	req, err := makeRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	posted, err := client.Do(req)
	if err != nil {
		return result, errors.New("Cowork message submission failed; delivery status unknown")
	}
	io.Copy(io.Discard, io.LimitReader(posted.Body, 1<<20))
	posted.Body.Close()
	if posted.StatusCode != http.StatusAccepted {
		return result, &StatusError{"messages", posted.StatusCode}
	}
	var text strings.Builder
	last := "0"
	seen := map[string]bool{}
	finished := false
	handler := func(id, event, data string) error {
		if id != "" {
			if seen[id] {
				return nil
			}
			seen[id] = true
			last = id
		}
		if event != "dx" && event != "fr" && event != "error" {
			return nil
		}
		var d struct {
			Text    string  `json:"t"`
			Content *string `json:"content"`
			Stop    string  `json:"stop"`
		}
		if json.Unmarshal([]byte(data), &d) != nil {
			return errors.New("invalid Cowork response event")
		}
		switch event {
		case "error":
			return errors.New("Cowork reported an upstream error")
		case "dx":
			text.WriteString(d.Text)
			if delta != nil {
				return delta(d.Text)
			}
		case "fr":
			if d.Content != nil {
				if !strings.HasPrefix(*d.Content, text.String()) {
					return errors.New("Cowork final content differs from emitted text")
				}
				tail := strings.TrimPrefix(*d.Content, text.String())
				text.WriteString(tail)
				if tail != "" && delta != nil {
					if e := delta(tail); e != nil {
						return e
					}
				}
			}
			result.Text = text.String()
			result.Stop = d.Stop
			finished = true
			return errFinished
		}
		return nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		err = readEvents(resp.Body, handler)
		if finished {
			return result, nil
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		// Reconnect subscriptions with the last event ID, never resubmit messages.
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, errStreamRead) {
			return result, err
		}
		resp.Body.Close()
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if attempt < 2 {
			resp, err = openStream(last)
			if err != nil {
				return result, err
			}
		}
	}
	return result, errors.New("Cowork stream ended before a final response")
}

var errFinished = errors.New("response finished")
var errStreamRead = errors.New("Cowork stream read failed")

func readEvents(r io.Reader, fn func(string, string, string) error) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var id, event string
	var lines []string
	dispatch := func() error {
		if event == "" && len(lines) == 0 {
			return nil
		}
		e := fn(id, event, strings.Join(lines, "\n"))
		id = ""
		event = ""
		lines = nil
		return e
	}
	for s.Scan() {
		line := s.Text()
		if line == "" {
			if e := dispatch(); e != nil {
				return e
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimPrefix(v, " ")
		switch k {
		case "id":
			id = v
		case "event":
			event = v
		case "data":
			lines = append(lines, v)
		}
	}
	if s.Err() != nil {
		return errStreamRead
	}
	if e := dispatch(); e != nil {
		return e
	}
	return io.EOF
}
