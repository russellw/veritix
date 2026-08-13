package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/russellw/veritix/internal/agent/llm"
)

// writtenCall reports the tool a model described in its message instead of
// calling, and whether it found one.
//
// Weak models on the chat-completions dialect do this: qwen3-4b finished a run
// here by emitting three complete record_finding payloads as message content,
// one of them a real finding the engine would have confirmed, and the loop read
// a model that had stopped calling tools and ended the run with nothing. The
// work had been done. Only the handover failed.
//
// So this is treated as what it is — a malformed tool call — and goes back to
// the model the way every other malformed call does, for it to correct. What it
// deliberately does not do is execute the written-out arguments: that would put
// a finding in the report without it ever passing through the tool that checks
// the count against the query, which is the one thing that must not happen.
//
// The test is the tool schemas rather than any particular field name: an object
// carrying every required parameter of a tool and nothing that is not one of
// its parameters is a call. Tools with no required parameters are skipped,
// since every object satisfies them.
func writtenCall(text string, tools []llm.Tool) (string, bool) {
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			continue
		}
		if name, ok := namesATool(obj, tools); ok {
			return name, true
		}
		// Past this object rather than into it: a nested object cannot be a
		// call its parent is not.
		i += int(dec.InputOffset()) - 1
	}
	return "", false
}

func namesATool(obj map[string]any, tools []llm.Tool) (string, bool) {
	for _, t := range tools {
		if len(t.Required) == 0 || len(obj) < len(t.Required) {
			continue
		}
		if !hasAll(obj, t.Required) || !onlyKnown(obj, t.Properties) {
			continue
		}
		return t.Name, true
	}
	return "", false
}

func hasAll(obj map[string]any, required []string) bool {
	for _, r := range required {
		if _, ok := obj[r]; !ok {
			return false
		}
	}
	return true
}

func onlyKnown(obj map[string]any, properties map[string]any) bool {
	for k := range obj {
		if _, ok := properties[k]; !ok {
			return false
		}
	}
	return true
}

// writtenCallCorrection is what the model is told, once per run.
//
// It says what happened and stops. It does not ask for a finding: a model that
// has genuinely concluded there is nothing more to report must be able to say
// so, and a nudge that reads as "you were supposed to record something" is how
// a report fills up with padding nobody can act on.
func writtenCallCorrection(tool string) string {
	return fmt.Sprintf("That was a call to %s written out as text. Veritix reads tool "+
		"calls, not messages: nothing in that one was parsed, and nothing in it was "+
		"recorded. Make the call itself if you want it recorded. If you have finished, "+
		"say so plainly — finding nothing beyond the deterministic pass is a legitimate "+
		"result, and nothing needs to be invented to fill a report.", tool)
}
