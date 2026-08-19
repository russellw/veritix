package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/russellw/veritix/internal/agent/llm"
)

// wire is the part of a request this file cares about: where the cache
// breakpoints land once the SDK has marshaled it. Asserting on the JSON rather
// than on the parameter structs is deliberate — the breakpoints are only worth
// anything if they reach the API, and the SDK elides an unset one.
type wire struct {
	System []struct {
		CacheControl *json.RawMessage `json:"cache_control"`
	} `json:"system"`
	Messages []struct {
		Content []struct {
			CacheControl *json.RawMessage `json:"cache_control"`
		} `json:"content"`
	} `json:"messages"`
}

func marshal(t *testing.T, req *llm.Request) wire {
	t.Helper()
	params, err := New(Options{APIKey: "test"}).params(req)
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return w
}

// breakpoints reports, per message, the indexes of the blocks carrying one.
func breakpoints(w wire) [][]int {
	out := make([][]int, len(w.Messages))
	for i, m := range w.Messages {
		for j, b := range m.Content {
			if b.CacheControl != nil {
				out[i] = append(out[i], j)
			}
		}
	}
	return out
}

func step(id, text string) []llm.Message {
	return []llm.Message{
		{Role: llm.RoleAssistant, Parts: []llm.Part{
			{Kind: llm.PartText, Text: text},
			{Kind: llm.PartToolUse, ID: id, Name: "run_sql", Input: json.RawMessage(`{"query":"SELECT 1"}`)},
		}},
		{Role: llm.RoleUser, Parts: []llm.Part{
			{Kind: llm.PartToolResult, ID: id, Result: "1"},
		}},
	}
}

// The transcript is what grows over a run, so the breakpoint has to move with
// it. Without this the same tool calls are re-sent and re-billed at full input
// price on every step, and a run costs quadratically more than it needs to.
func TestTheConversationTailCarriesCacheBreakpoints(t *testing.T) {
	req := &llm.Request{System: "you audit datasets"}
	req.Messages = append(req.Messages, llm.Message{
		Role:  llm.RoleUser,
		Parts: []llm.Part{{Kind: llm.PartText, Text: "the brief"}},
	})
	for _, id := range []string{"a", "b", "c"} {
		req.Messages = append(req.Messages, step(id, "looking")...)
	}

	w := marshal(t, req)

	if len(w.System) != 1 || w.System[0].CacheControl == nil {
		t.Error("the system prompt and the tools should be cached: they are identical on every call")
	}

	got := breakpoints(w)
	if len(got) != 7 {
		t.Fatalf("got %d messages, want 7", len(got))
	}
	// The last two messages are the most recent step; everything before it was
	// already cached by the previous call.
	want := [][]int{nil, nil, nil, nil, nil, {1}, {0}}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Errorf("message %d: breakpoints at %v, want %v", i, got[i], want[i])
			continue
		}
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Errorf("message %d: breakpoints at %v, want %v", i, got[i], want[i])
				break
			}
		}
	}
}

// Four is the API's limit and exceeding it is a 400, so the count must not grow
// with the conversation.
func TestBreakpointCountStaysUnderTheLimit(t *testing.T) {
	for _, steps := range []int{1, 5, 24} {
		req := &llm.Request{System: "you audit datasets"}
		for i := 0; i < steps; i++ {
			req.Messages = append(req.Messages, step("t", "looking")...)
		}

		w := marshal(t, req)
		count := len(w.System)
		for _, m := range breakpoints(w) {
			count += len(m)
		}
		if count > 4 {
			t.Errorf("%d steps: %d breakpoints, the API allows 4", steps, count)
		}
		if count != 3 {
			t.Errorf("%d steps: %d breakpoints, want 3 (system, and the last two messages)", steps, count)
		}
	}
}

// A thinking block cannot carry a breakpoint. Marking nothing and calling the
// message done would leave the transcript uncached from there on, which is the
// failure this whole mechanism exists to avoid.
func TestAMessageEndingInThinkingIsWalkedPast(t *testing.T) {
	req := &llm.Request{
		System: "you audit datasets",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Parts: []llm.Part{{Kind: llm.PartText, Text: "the brief"}}},
			{Role: llm.RoleAssistant, Parts: []llm.Part{
				{Kind: llm.PartText, Text: "looking"},
				{Kind: llm.PartThinking, Text: "", Signature: "sig"},
			}},
		},
	}

	got := breakpoints(marshal(t, req))
	want := [][]int{{0}, {0}}
	for i := range want {
		if len(got[i]) != len(want[i]) || (len(got[i]) > 0 && got[i][0] != want[i][0]) {
			t.Errorf("message %d: breakpoints at %v, want %v", i, got[i], want[i])
		}
	}
}
