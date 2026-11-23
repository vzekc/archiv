package archiv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// The language model behind the suggestions: sorting submissions, proposing
// tags for untagged items, titles. The archive speaks to it through
// LLMProvider, one conversation turn at a time; a provider translates the
// turn into its vendor's API. The providers "anthropic" and "openai" (any
// service with the OpenAI chat completions API: OpenAI itself, Mistral, a
// local model through Ollama or LM Studio, ...) come with the archive;
// RegisterLLM adds more.

// LLMPart is a piece of a message: text or a JPEG picture.
type LLMPart struct {
	Text string
	JPEG []byte
}

// LLMToolCall is the model asking to use a tool.
type LLMToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// LLMToolResult answers a tool call.
type LLMToolResult struct {
	ID      string
	Content string
	IsError bool
}

// LLMMessage is one message of the conversation. The model's messages
// carry Raw, the provider's own form of it, so that the provider can send
// it back unchanged.
type LLMMessage struct {
	Role    string // "user" or "assistant"
	Parts   []LLMPart
	Calls   []LLMToolCall
	Results []LLMToolResult
	Raw     any
}

// LLMTool is a tool the model may use; Params are the JSON schema
// properties of its input.
type LLMTool struct {
	Name        string
	Description string
	Params      map[string]any
	Required    []string
}

// LLMRequest is one turn: the conversation so far and what the answer must
// look like.
type LLMRequest struct {
	System    string
	Messages  []LLMMessage
	Tools     []LLMTool
	NoTools   bool           // the model must answer now
	Schema    map[string]any // JSON schema of the answer
	MaxTokens int
	Effort    string // "high" or "medium": how hard the model should think, where it can be told
}

// Stop reasons of a turn.
const (
	LLMDone      = "done"
	LLMToolUse   = "tool_use"
	LLMRefused   = "refused"
	LLMMaxTokens = "max_tokens"
)

// LLMTurn is the model's answer to one turn.
type LLMTurn struct {
	Text    string
	Calls   []LLMToolCall
	Stop    string
	Model   string
	Message LLMMessage // to add to the conversation
	In, Out int        // tokens
	Cached  int        // input tokens read from the provider's cache
}

// LLMProvider sends one turn to a language model.
type LLMProvider interface {
	Turn(ctx context.Context, req *LLMRequest) (*LLMTurn, error)
}

// LLMSettings are what a provider is made from: the llm_* settings.
type LLMSettings struct {
	Model string
	Key   string // the API key, "" when the service needs none
	URL   string // the service's address, for providers that take one
}

var llmProviders = map[string]func(LLMSettings) (LLMProvider, error){
	"anthropic": newAnthropicLLM,
	"openai":    newOpenAILLM,
}

// RegisterLLM adds a provider for the setting llm = name.
func RegisterLLM(name string, make func(LLMSettings) (LLMProvider, error)) { llmProviders[name] = make }

// newLLM makes the configured provider, or nil when there is none.
func newLLM(c *Config) (LLMProvider, error) {
	if c.LLM == "" {
		return nil, nil
	}
	make, ok := llmProviders[c.LLM]
	if !ok {
		var names []string
		for n := range llmProviders {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("llm: unknown provider %q (known: %s)", c.LLM, strings.Join(names, ", "))
	}
	key, err := os.ReadFile(c.LLMKeyFile)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return make(LLMSettings{Model: c.LLMModel, Key: strings.TrimSpace(string(key)), URL: c.LLMURL})
}

// LLMTask is one job for the model: a question, the tools it may use, and
// a check of its answer.
type LLMTask struct {
	Label     string // for the log
	System    string
	User      []LLMPart
	Schema    map[string]any
	Tools     []LLMTool
	RunTool   func(name string, input json.RawMessage) (out string, isErr bool)
	MaxTokens int
	Effort    string
	MaxRounds int // turns with tools; the last one must answer (default 1)
	// Check looks at an answer; with a problem, the model tries again, at
	// most twice, told what Feedback makes of the problem (default: the
	// problem itself).
	Check    func(text string) (problem string)
	Feedback func(problem string) string
}

// run carries out a task and answers the model's final text and the
// model's name.
func (ai *AI) run(ctx context.Context, t *LLMTask) (string, string, error) {
	if ai.llm == nil {
		return "", "", errors.New("kein Sprachmodell eingerichtet")
	}
	rounds := max(t.MaxRounds, 1)
	req := &LLMRequest{System: t.System, Tools: t.Tools, Schema: t.Schema, MaxTokens: t.MaxTokens, Effort: t.Effort,
		Messages: []LLMMessage{{Role: "user", Parts: t.User}}}
	tools, retries := 0, 0
	for round := 0; ; round++ {
		req.NoTools = len(t.Tools) == 0 || round >= rounds-1
		turn, err := ai.llm.Turn(ctx, req)
		if err != nil {
			return "", "", err
		}
		logf("llm %s, round %d: in %d (cached %d), out %d", t.Label, round, turn.In, turn.Cached, turn.Out)
		switch turn.Stop {
		case LLMRefused:
			return "", "", errors.New("das Sprachmodell hat die Anfrage abgelehnt")
		case LLMMaxTokens:
			return "", "", errors.New("die Antwort des Sprachmodells war zu lang")
		}
		req.Messages = append(req.Messages, turn.Message)
		if turn.Stop == LLMToolUse && len(turn.Calls) > 0 && t.RunTool != nil {
			var results []LLMToolResult
			for _, c := range turn.Calls {
				out, isErr := t.RunTool(c.Name, c.Input)
				results = append(results, LLMToolResult{ID: c.ID, Content: out, IsError: isErr})
				tools++
			}
			req.Messages = append(req.Messages, LLMMessage{Role: "user", Results: results})
			continue
		}
		problem := ""
		if t.Check != nil {
			problem = t.Check(turn.Text)
		}
		if problem == "" {
			if tools > 0 {
				logf("llm %s: answered after %d tool calls", t.Label, tools)
			}
			return turn.Text, turn.Model, nil
		}
		if retries++; retries > 2 || round >= rounds-1 {
			return "", "", errors.New(problem)
		}
		// tell the model what was wrong and let it try again
		say := problem
		if t.Feedback != nil {
			say = t.Feedback(problem)
		}
		req.Messages = append(req.Messages, LLMMessage{Role: "user", Parts: []LLMPart{{Text: say}}})
	}
}
