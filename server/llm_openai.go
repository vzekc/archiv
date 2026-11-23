package archiv

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openaiLLM speaks the chat completions API of OpenAI, which many services
// offer as well: Mistral, Groq, and local models through Ollama
// (llm_url = http://localhost:11434/v1), LM Studio or vLLM.
type openaiLLM struct {
	url, key, model string
	client          *http.Client
}

func newOpenAILLM(s LLMSettings) (LLMProvider, error) {
	if s.Model == "" {
		return nil, errors.New("llm = openai needs a model (llm_model)")
	}
	url := strings.TrimRight(s.URL, "/")
	if url == "" {
		url = "https://api.openai.com/v1"
	}
	return &openaiLLM{url: url, key: s.Key, model: s.Model, client: &http.Client{Timeout: 15 * time.Minute}}, nil
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content,omitempty"` // string or []oaPart
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaPart struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ImageURL *oaImageURL `json:"image_url,omitempty"`
}

type oaImageURL struct {
	URL string `json:"url"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (l *openaiLLM) Turn(ctx context.Context, req *LLMRequest) (*LLMTurn, error) {
	msgs := []oaMessage{{Role: "system", Content: req.System}}
	for _, m := range req.Messages {
		if raw, ok := m.Raw.(oaMessage); ok {
			msgs = append(msgs, raw)
			continue
		}
		for _, r := range m.Results {
			content := r.Content
			if r.IsError {
				content = "Fehler: " + content
			}
			msgs = append(msgs, oaMessage{Role: "tool", ToolCallID: r.ID, Content: content})
		}
		if len(m.Parts) == 0 {
			continue
		}
		var parts []oaPart
		for _, p := range m.Parts {
			if p.JPEG != nil {
				parts = append(parts, oaPart{Type: "image_url", ImageURL: &oaImageURL{URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(p.JPEG)}})
			} else {
				parts = append(parts, oaPart{Type: "text", Text: p.Text})
			}
		}
		msgs = append(msgs, oaMessage{Role: m.Role, Content: parts})
	}
	body := map[string]any{"model": l.model, "messages": msgs}
	if req.MaxTokens > 0 {
		body["max_completion_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		var tools []any
		for _, t := range req.Tools {
			params := map[string]any{"type": "object", "properties": t.Params}
			if len(t.Required) > 0 {
				params["required"] = t.Required
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": params}})
		}
		body["tools"] = tools
		if req.NoTools {
			body["tool_choice"] = "none"
		}
	}
	if req.Schema != nil {
		body["response_format"] = map[string]any{"type": "json_schema",
			"json_schema": map[string]any{"name": "antwort", "schema": req.Schema}}
	}
	if req.Effort != "" && strings.HasPrefix(l.url, "https://api.openai.com/") {
		body["reasoning_effort"] = req.Effort
	}
	js, _ := json.Marshal(body)
	hr, _ := http.NewRequestWithContext(ctx, "POST", l.url+"/chat/completions", bytes.NewReader(js))
	hr.Header.Set("Content-Type", "application/json")
	if l.key != "" {
		hr.Header.Set("Authorization", "Bearer "+l.key)
	}
	var resp *http.Response
	var err error
	for try := 0; ; try++ {
		hr.Body = io.NopCloser(bytes.NewReader(js))
		resp, err = l.client.Do(hr)
		if err == nil && resp.StatusCode != 429 && resp.StatusCode < 500 {
			break
		}
		if try == 3 {
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(time.Duration(5<<try) * time.Second)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("llm %s: %s: %s", l.url, resp.Status, limitBytes(string(raw), 500))
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string       `json:"content"`
				Refusal   string       `json:"refusal"`
				ToolCalls []oaToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Details    struct {
				Cached int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return nil, fmt.Errorf("llm %s: bad answer %q", l.url, limitBytes(string(raw), 300))
	}
	ch := out.Choices[0]
	turn := &LLMTurn{Text: ch.Message.Content, Model: out.Model, Stop: LLMDone,
		In: out.Usage.Prompt, Out: out.Usage.Completion, Cached: out.Usage.Details.Cached}
	switch {
	case ch.Message.Refusal != "":
		turn.Stop = LLMRefused
	case ch.FinishReason == "length":
		turn.Stop = LLMMaxTokens
	case len(ch.Message.ToolCalls) > 0:
		turn.Stop = LLMToolUse
	}
	am := oaMessage{Role: "assistant", ToolCalls: ch.Message.ToolCalls}
	if ch.Message.Content != "" {
		am.Content = ch.Message.Content
	}
	turn.Message = LLMMessage{Role: "assistant", Raw: am}
	for _, c := range ch.Message.ToolCalls {
		turn.Calls = append(turn.Calls, LLMToolCall{ID: c.ID, Name: c.Function.Name, Input: json.RawMessage(c.Function.Arguments)})
	}
	return turn, nil
}
