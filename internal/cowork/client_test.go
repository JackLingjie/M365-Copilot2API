package cowork

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
)

func TestChatReplaysObservedProtocolAndResumes(t *testing.T) {
	submitted := make(chan struct{})
	var calls atomic.Int32
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing bearer token")
		}
		if r.Header.Get("x-container-config") != "renderUi=true;model=melon" {
			t.Errorf("wrong model/config: %s", r.Header.Get("x-container-config"))
		}
		switch r.URL.Path {
		case "/v1/messages":
			posts.Add(1)
			var d struct {
				ConversationID, MessageID, Role string
				Queue                           bool
				Content                         []struct{ Type, Text string }
			}
			if e := json.NewDecoder(r.Body).Decode(&d); e != nil {
				t.Error(e)
			}
			if d.ConversationID != r.Header.Get("x-conversation-id") || !strings.HasPrefix(d.ConversationID, "tenant:user:") || d.MessageID == "" || !d.Queue || d.Role != "user" || len(d.Content) != 1 || d.Content[0].Type != "text" || d.Content[0].Text != "test prompt" {
				t.Errorf("incorrect message: %+v", d)
			}
			close(submitted)
			w.WriteHeader(202)
		case "/v1/subscribe":
			if r.URL.Query().Get("conversationId") != r.Header.Get("x-conversation-id") {
				t.Error("subscription conversation mismatch")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			select {
			case <-submitted:
			case <-r.Context().Done():
				return
			}
			if calls.Add(1) == 1 {
				if r.Header.Get("last-event-id") != "0" {
					t.Error("wrong initial cursor")
				}
				fmt.Fprint(w, "id: seq:1\nevent: dx\ndata: {\"t\":\"你好\"}\n\n")
			} else {
				if r.Header.Get("last-event-id") != "seq:1" {
					t.Error("did not resume from cursor")
				}
				fmt.Fprint(w, "id: seq:1\nevent: dx\ndata: {\"t\":\"你好\"}\n\nid: seq:2\nevent: fr\ndata: {\"stop\":\"end_turn\",\"content\":\"你好 world\"}\n\n")
			}
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	var got strings.Builder
	c := Client{BaseURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := c.Chat(ctx, Request{Token: "test-token", TenantID: "tenant", UserID: "user", Model: "melon", Config: "model=old;renderUi=true", Text: "test prompt"}, func(s string) error { got.WriteString(s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "你好 world" || got.String() != res.Text || res.Stop != "end_turn" {
		t.Fatalf("wrong completion: %+v delta=%q", res, got.String())
	}
	if posts.Load() != 1 || calls.Load() != 2 {
		t.Fatal("reconnection must not resubmit message")
	}
}

func TestChatRejectsFailedSubmissionAndHonorsCancellation(t *testing.T) {
	for _, status := range []int{401, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/subscribe" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				w.WriteHeader(status)
				fmt.Fprint(w, "sensitive backend response")
			}))
			defer srv.Close()
			c := Client{BaseURL: srv.URL}
			_, err := c.Chat(context.Background(), Request{Token: "token", TenantID: "tenant", UserID: "user", Model: "melon"}, nil)
			if err == nil || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := (&Client{BaseURL: srv.URL}).Chat(ctx, Request{Token: "token", TenantID: "tenant", UserID: "user", Model: "melon"}, nil)
	if err == nil {
		t.Fatal("missing cancellation error")
	}
}

func TestReadEventsHandlesMultilineData(t *testing.T) {
	input := ": heartbeat\r\nid: seq:1\r\nevent: dx\r\ndata: {\r\ndata: \"t\":\"中文\"}\r\n\r\n"
	count := 0
	readEvents(strings.NewReader(input), func(id, event, data string) error {
		count++
		var d map[string]string
		if json.Unmarshal([]byte(data), &d) != nil || d["t"] != "中文" || event != "dx" || id != "seq:1" {
			t.Fatal("bad SSE parsing")
		}
		return nil
	})
	if count != 1 {
		t.Fatal("wrong frame count")
	}
}

// A failed reconnect returns a nil response; cleanup must not dereference it.
func TestChatFailedReconnectReturnsErrorWithoutResubmitting(t *testing.T) {
	var subscriptions, posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			posts.Add(1)
			w.WriteHeader(202)
			return
		}
		if subscriptions.Add(1) > 1 {
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: seq:1\nevent: dx\ndata: {\"t\":\"partial\"}\n\n")
	}))
	defer srv.Close()
	_, err := (&Client{BaseURL: srv.URL}).Chat(context.Background(), Request{Token: "token", TenantID: "t", UserID: "u", Model: "test"}, nil)
	if err == nil || subscriptions.Load() != 2 || posts.Load() != 1 {
		t.Fatalf("bad reconnect outcome: %v, %d subscriptions, %d posts", err, subscriptions.Load(), posts.Load())
	}
}
