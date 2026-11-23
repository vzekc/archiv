package archiv

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scriptLLM answers each turn with the next of its turns and keeps the
// requests.
type scriptLLM struct {
	turns []*LLMTurn
	reqs  []LLMRequest
}

func (l *scriptLLM) Turn(_ context.Context, req *LLMRequest) (*LLMTurn, error) {
	l.reqs = append(l.reqs, *req)
	t := l.turns[0]
	l.turns = l.turns[1:]
	t.Message = LLMMessage{Role: "assistant", Raw: t.Text}
	return t, nil
}

// A task runs the model's tools, sends back what was wrong with an answer,
// and makes the last round answer without tools.
func TestLLMTask(t *testing.T) {
	l := &scriptLLM{turns: []*LLMTurn{
		{Stop: LLMToolUse, Calls: []LLMToolCall{{ID: "c1", Name: "search", Input: json.RawMessage(`{"q":"pdp"}`)}}},
		{Stop: LLMDone, Text: `{"ok": false}`},
		{Stop: LLMDone, Text: `{"ok": true}`, Model: "m1"},
	}}
	ai := &AI{llm: l}
	var tools []string
	text, model, err := ai.run(context.Background(), &LLMTask{
		Label: "test", System: "sys", User: []LLMPart{{Text: "frage"}},
		Tools:     []LLMTool{{Name: "search"}},
		MaxRounds: 3,
		RunTool: func(name string, in json.RawMessage) (string, bool) {
			tools = append(tools, name+string(in))
			return "gefunden", false
		},
		Check: func(text string) string {
			if !strings.Contains(text, "true") {
				return "nicht ok"
			}
			return ""
		},
		Feedback: func(p string) string { return "Bitte noch einmal: " + p },
	})
	if err != nil || text != `{"ok": true}` || model != "m1" {
		t.Fatalf("answer %q %q %v", text, model, err)
	}
	if len(tools) != 1 || tools[0] != `search{"q":"pdp"}` {
		t.Errorf("tools: %v", tools)
	}
	if len(l.reqs) != 3 || l.reqs[0].NoTools || !l.reqs[2].NoTools {
		t.Errorf("rounds: %d, tools off in the last: %v", len(l.reqs), l.reqs[len(l.reqs)-1].NoTools)
	}
	last := l.reqs[2].Messages
	if r := last[2].Results; len(r) != 1 || r[0].ID != "c1" || r[0].Content != "gefunden" {
		t.Errorf("tool result not sent: %+v", last[2])
	}
	if p := last[len(last)-1].Parts; len(p) != 1 || p[0].Text != "Bitte noch einmal: nicht ok" {
		t.Errorf("feedback not sent: %+v", last[len(last)-1])
	}
}

func TestLLMRefusal(t *testing.T) {
	ai := &AI{llm: &scriptLLM{turns: []*LLMTurn{{Stop: LLMRefused}}}}
	if _, _, err := ai.run(context.Background(), &LLMTask{}); err == nil || !strings.Contains(err.Error(), "abgelehnt") {
		t.Errorf("refusal: %v", err)
	}
	if _, _, err := (&AI{}).run(context.Background(), &LLMTask{}); err == nil {
		t.Errorf("no model, no error")
	}
}

// The OpenAI provider speaks the chat completions API: tools as
// functions, pictures as data URLs, the answer's JSON schema, tool calls
// and their results.
func TestOpenAILLM(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "wrong", 400)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(b, &req)
		got = append(got, req)
		if len(got) == 1 {
			io.WriteString(w, `{"model":"m","choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"t1","type":"function","function":{"name":"search","arguments":"{\"q\":\"x\"}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
			return
		}
		io.WriteString(w, `{"model":"m","choices":[{"finish_reason":"stop","message":{"content":"{\"a\":1}"}}],"usage":{"prompt_tokens":20,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":8}}}`)
	}))
	defer srv.Close()
	p, err := newOpenAILLM(LLMSettings{Model: "m", Key: "k", URL: srv.URL + "/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	ai := &AI{llm: p}
	text, model, err := ai.run(context.Background(), &LLMTask{
		System: "sys", User: []LLMPart{{Text: "frage"}, {JPEG: []byte{0xff, 0xd8}}},
		Schema: map[string]any{"type": "object"}, Tools: []LLMTool{{Name: "search", Params: map[string]any{"q": map[string]any{"type": "string"}}}},
		RunTool:   func(string, json.RawMessage) (string, bool) { return "treffer", false },
		MaxRounds: 4,
	})
	if err != nil || text != `{"a":1}` || model != "m" {
		t.Fatalf("answer %q %q %v", text, model, err)
	}
	first, _ := json.Marshal(got[0])
	for _, want := range []string{`"role":"system"`, `"data:image/jpeg;base64,/9g="`, `"type":"function"`, `"json_schema"`} {
		if !strings.Contains(string(first), want) {
			t.Errorf("request: %s missing in %s", want, first)
		}
	}
	second, _ := json.Marshal(got[1])
	for _, want := range []string{`"tool_calls"`, `"role":"tool"`, `"tool_call_id":"t1"`, `"content":"treffer"`} {
		if !strings.Contains(string(second), want) {
			t.Errorf("second request: %s missing in %s", want, second)
		}
	}
}

func TestLLMConfig(t *testing.T) {
	c := defaultConfig()
	if l, err := newLLM(c); l != nil || err != nil {
		t.Errorf("without llm: %v %v", l, err)
	}
	c.LLM = "erfunden"
	if _, err := newLLM(c); err == nil || !strings.Contains(err.Error(), "anthropic, openai") {
		t.Errorf("unknown provider: %v", err)
	}
	c.LLM, c.LLMModel, c.LLMURL, c.LLMKeyFile = "openai", "llama3", "http://localhost:11434/v1", "/nicht/da"
	if l, err := newLLM(c); l == nil || err != nil {
		t.Errorf("local model without key: %v %v", l, err)
	}
}

// The Anthropic provider streams; the fake server sends the events of a
// tool call, then of the answer.
func TestAnthropicLLM(t *testing.T) {
	var bodies []string
	sse := func(w http.ResponseWriter, events ...string) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			var m map[string]any
			json.Unmarshal([]byte(e), &m)
			io.WriteString(w, "event: "+m["type"].(string)+"\ndata: "+e+"\n\n")
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		start := `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0,"cache_read_input_tokens":3}}}`
		if len(bodies) == 1 {
			sse(w, start,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu1","name":"search","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"vax\"}"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
				`{"type":"message_stop"}`)
			return
		}
		sse(w, start,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"a\":2}"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
			`{"type":"message_stop"}`)
	}))
	defer srv.Close()
	p, err := newAnthropicLLM(LLMSettings{Key: "k", URL: srv.URL, Model: "claude-x"})
	if err != nil {
		t.Fatal(err)
	}
	var in string
	text, model, err := (&AI{llm: p}).run(context.Background(), &LLMTask{
		System: "sys", User: []LLMPart{{Text: "frage"}, {JPEG: []byte{1}}}, Schema: map[string]any{"type": "object"},
		Tools:     []LLMTool{{Name: "search", Params: map[string]any{"q": map[string]any{"type": "string"}}, Required: []string{"q"}}},
		RunTool:   func(_ string, i json.RawMessage) (string, bool) { in = string(i); return "treffer", false },
		MaxRounds: 3, MaxTokens: 1000, Effort: "high",
	})
	if err != nil || text != `{"a":2}` || model != "claude-x" {
		t.Fatalf("answer %q %q %v", text, model, err)
	}
	if in != `{"q":"vax"}` {
		t.Errorf("tool input %q", in)
	}
	for _, want := range []string{`"type":"tool_use"`, `"tool_use_id":"tu1"`, `"content":[{"text":"treffer"`, `"media_type":"image/jpeg"`, `"json_schema"`} {
		if !strings.Contains(bodies[1], want) {
			t.Errorf("second request: %s missing in %s", want, bodies[1])
		}
	}
}
