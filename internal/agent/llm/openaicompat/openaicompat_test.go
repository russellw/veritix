package openaicompat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/russellw/veritix/internal/agent/llm"
)

// serve stands in for Ollama or vLLM: it captures the request body and returns
// a canned reply.
func serve(t *testing.T, status int, body string) (*Provider, *chatRequest) {
	t.Helper()

	captured := &chatRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("posted to %s, want /chat/completions", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, captured); err != nil {
			t.Errorf("the request was not valid JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	p, err := New(Options{BaseURL: srv.URL, Model: "llama3.1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, captured
}

func TestToolCallRoundTrip(t *testing.T) {
	const reply = `{
	  "model": "llama3.1",
	  "choices": [{
	    "finish_reason": "tool_calls",
	    "message": {
	      "content": "",
	      "tool_calls": [{"id": "c1", "type": "function",
	        "function": {"name": "profile_column", "arguments": "{\"table\":\"orders\"}"}}]
	    }
	  }],
	  "usage": {"prompt_tokens": 120, "completion_tokens": 8}
	}`

	p, captured := serve(t, http.StatusOK, reply)

	res, err := p.Complete(t.Context(), &llm.Request{
		System: "audit this",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Parts: []llm.Part{{Kind: llm.PartText, Text: "begin"}}},
		},
		Tools: []llm.Tool{{
			Name:        "profile_column",
			Description: "measure a column",
			Properties:  map[string]any{"table": map[string]any{"type": "string"}},
			Required:    []string{"table"},
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// The system prompt becomes a message in this dialect rather than a field.
	if len(captured.Messages) != 2 || captured.Messages[0].Role != "system" {
		t.Fatalf("messages = %+v, want a system turn then a user turn", captured.Messages)
	}
	if len(captured.Tools) != 1 || captured.Tools[0].Function.Name != "profile_column" {
		t.Fatalf("tools = %+v", captured.Tools)
	}
	if captured.Tools[0].Function.Parameters["type"] != "object" {
		t.Errorf("the tool schema lost its envelope: %+v", captured.Tools[0].Function.Parameters)
	}

	calls := res.Message.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "profile_column" || calls[0].ID != "c1" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if string(calls[0].Input) != `{"table":"orders"}` {
		t.Errorf("arguments = %s", calls[0].Input)
	}
	if res.StopReason != llm.StopToolUse {
		t.Errorf("stop reason = %q, want tool_use", res.StopReason)
	}
	if res.Usage.Input != 120 || res.Usage.Output != 8 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

// A tool result is a message of its own here, keyed by call id, rather than a
// part of the following turn.
func TestToolResultsBecomeTheirOwnMessages(t *testing.T) {
	p, captured := serve(t, http.StatusOK,
		`{"choices":[{"finish_reason":"stop","message":{"content":"done"}}]}`)

	_, err := p.Complete(t.Context(), &llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Parts: []llm.Part{{Kind: llm.PartText, Text: "begin"}}},
			{Role: llm.RoleAssistant, Parts: []llm.Part{
				{Kind: llm.PartThinking, Text: "reasoning that cannot be replayed here"},
				{Kind: llm.PartToolUse, ID: "c1", Name: "list_tables", Input: []byte(`{}`)},
			}},
			{Role: llm.RoleUser, Parts: []llm.Part{
				{Kind: llm.PartToolResult, ID: "c1", Result: `{"tables":2}`},
				{Kind: llm.PartText, Text: "carry on"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var roles []string
	for _, m := range captured.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"user", "assistant", "tool", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v", roles, want)
	}

	if captured.Messages[2].ToolCallID != "c1" {
		t.Errorf("the tool result lost its call id: %+v", captured.Messages[2])
	}
	// An assistant turn that only calls tools must send a null content, which
	// some servers require.
	if captured.Messages[1].Content != nil {
		t.Errorf("a tool-only assistant turn sent content %q", *captured.Messages[1].Content)
	}
	// Thinking has nowhere to go in this dialect and must not be smuggled in
	// as prose, which would feed the model its own reasoning as if it were an
	// instruction.
	body, _ := json.Marshal(captured)
	if strings.Contains(string(body), "cannot be replayed") {
		t.Error("a thinking block was sent to an endpoint that has no place for one")
	}
}

// Several servers say "stop" while returning tool calls. What arrived is more
// reliable than what the server said about it.
func TestToolCallsOutrankAWrongFinishReason(t *testing.T) {
	p, _ := serve(t, http.StatusOK, `{
	  "choices": [{"finish_reason": "stop", "message": {"content": "",
	    "tool_calls": [{"id":"c1","type":"function","function":{"name":"list_tables","arguments":""}}]}}]
	}`)

	res, err := p.Complete(t.Context(), &llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.StopReason != llm.StopToolUse {
		t.Errorf("stop reason = %q, want tool_use", res.StopReason)
	}
	// Empty arguments must still parse as an object, or the tool cannot run.
	if got := string(res.Message.ToolCalls()[0].Input); got != "{}" {
		t.Errorf("empty arguments came through as %q, want {}", got)
	}
}

func TestErrorsCarryRetryability(t *testing.T) {
	for _, tc := range []struct {
		status    int
		body      string
		retryable bool
	}{
		{http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, true},
		{http.StatusInternalServerError, `{"error":{"message":"boom"}}`, true},
		{http.StatusBadRequest, `{"error":{"message":"no such model"}}`, false},
	} {
		p, _ := serve(t, tc.status, tc.body)
		_, err := p.Complete(t.Context(), &llm.Request{})
		if err == nil {
			t.Fatalf("status %d: want an error", tc.status)
		}
		var le *llm.Error
		if !asLLMError(err, &le) {
			t.Fatalf("status %d: got %T, want *llm.Error", tc.status, err)
		}
		if le.Retryable != tc.retryable {
			t.Errorf("status %d: retryable = %v, want %v", tc.status, le.Retryable, tc.retryable)
		}
		if le.Message == "" {
			t.Errorf("status %d: the error carries no message", tc.status)
		}
	}
}

// An endpoint that is not what it claims to be — a proxy's HTML error page, a
// misconfigured port — has to fail with something an operator can act on.
func TestNonJSONResponseIsReported(t *testing.T) {
	p, _ := serve(t, http.StatusOK, "<html>gateway timeout</html>")
	_, err := p.Complete(t.Context(), &llm.Request{})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "chat-completions JSON") {
		t.Errorf("the error should say what was wrong with the response, got: %v", err)
	}
}

func TestModelIsRequired(t *testing.T) {
	if _, err := New(Options{BaseURL: "http://localhost:11434/v1"}); err == nil {
		t.Error("a provider with no model name should not be constructible")
	}
}

func asLLMError(err error, target **llm.Error) bool {
	if e, ok := err.(*llm.Error); ok { //nolint:errorlint // deliberate direct check
		*target = e
		return true
	}
	return false
}
