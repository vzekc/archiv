package archiv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// anthropicLLM speaks to the Messages API of Anthropic. It caches the
// system prompt and the growing conversation, and streams, so that long
// answers are not cut off by a timeout.
type anthropicLLM struct {
	client *anthropic.Client
	model  string
}

func newAnthropicLLM(s LLMSettings) (LLMProvider, error) {
	if s.Key == "" {
		return nil, errors.New("llm = anthropic needs an API key (llm_key_file)")
	}
	opts := []option.RequestOption{option.WithAPIKey(s.Key), option.WithRequestTimeout(15 * time.Minute), option.WithMaxRetries(3)}
	if s.URL != "" {
		opts = append(opts, option.WithBaseURL(s.URL))
	}
	c := anthropic.NewClient(opts...)
	model := s.Model
	if model == "" {
		model = "claude-opus-5-5"
	}
	return &anthropicLLM{client: &c, model: model}, nil
}

func (l *anthropicLLM) Turn(ctx context.Context, req *LLMRequest) (*LLMTurn, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(l.model),
		MaxTokens: int64(req.MaxTokens),
		System:    []anthropic.TextBlockParam{{Text: req.System, CacheControl: anthropic.NewCacheControlEphemeralParam()}},
		// the conversation grows with every tool round; cache it as it goes
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
	}
	if req.Schema != nil || req.Effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{}
		if req.Schema != nil {
			params.OutputConfig.Format = anthropic.JSONOutputFormatParam{Schema: req.Schema}
		}
		switch req.Effort {
		case "high":
			params.OutputConfig.Effort = anthropic.OutputConfigEffortHigh
		case "medium":
			params.OutputConfig.Effort = anthropic.OutputConfigEffortMedium
		}
	}
	for _, t := range req.Tools {
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: t.Params, Required: t.Required},
		}})
	}
	if len(req.Tools) > 0 && req.NoTools {
		params.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
	}
	for _, m := range req.Messages {
		if raw, ok := m.Raw.(anthropic.MessageParam); ok {
			params.Messages = append(params.Messages, raw)
			continue
		}
		var blocks []anthropic.ContentBlockParamUnion
		for _, p := range m.Parts {
			if p.JPEG != nil {
				blocks = append(blocks, anthropic.NewImageBlockBase64("image/jpeg", base64.StdEncoding.EncodeToString(p.JPEG)))
			} else {
				blocks = append(blocks, anthropic.NewTextBlock(p.Text))
			}
		}
		for _, r := range m.Results {
			blocks = append(blocks, anthropic.NewToolResultBlock(r.ID, r.Content, r.IsError))
		}
		params.Messages = append(params.Messages, anthropic.NewUserMessage(blocks...))
	}

	stream := l.client.Messages.NewStreaming(ctx, params,
		option.WithHeaderAdd("anthropic-beta", "server-side-fallback-2026-07-01"),
		option.WithJSONSet("fallbacks", "default"))
	var msg anthropic.Message
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return nil, err
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}

	turn := &LLMTurn{Model: string(msg.Model), Stop: LLMDone, In: int(msg.Usage.InputTokens + msg.Usage.CacheReadInputTokens + msg.Usage.CacheCreationInputTokens),
		Out: int(msg.Usage.OutputTokens), Cached: int(msg.Usage.CacheReadInputTokens)}
	switch msg.StopReason {
	case anthropic.StopReasonRefusal:
		turn.Stop = LLMRefused
	case anthropic.StopReasonMaxTokens:
		turn.Stop = LLMMaxTokens
	case anthropic.StopReasonToolUse:
		turn.Stop = LLMToolUse
	}
	turn.Message = LLMMessage{Role: "assistant", Raw: msg.ToParam()}
	for _, b := range msg.Content {
		switch v := b.AsAny().(type) {
		case anthropic.TextBlock:
			turn.Text += v.Text
		case anthropic.ToolUseBlock:
			turn.Calls = append(turn.Calls, LLMToolCall{ID: v.ID, Name: v.Name, Input: json.RawMessage(v.JSON.Input.Raw())})
		}
	}
	return turn, nil
}
