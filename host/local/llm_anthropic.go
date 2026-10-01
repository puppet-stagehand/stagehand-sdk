package local

// The Anthropic Messages API client over plain net/http.
//
// Recorded deviation: Anthropic publishes an official Go SDK and its own
// guidance prefers an SDK where one exists. This client uses net/http anyway,
// on a locked project decision: one SDK cannot serve the second protocol, and
// the repo's first external dependency needed an explicit human approval
// checkpoint in Phase 6. Two small stdlib clients behind one seam keep the
// dependency count at zero.
//
// Hard constraints (08-RESEARCH.md, do not rediscover by trial):
//   - no sampling parameters (temperature/top_p/top_k are 400s on current
//     model families) and no thinking or effort field; thinking is on by
//     default and spends max_tokens, so budgets are generous caps;
//   - no forced tool choice for structured output (it 400s on several current
//     models); the output_config.format json_schema form is used instead;
//   - only "text" content blocks of the reply are read.

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// anthropicVersion is the API version header every Messages call carries.
const anthropicVersion = "2023-06-01"

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicFormat struct {
	Type   string         `json:"type"`
	Schema map[string]any `json:"schema"`
}

type anthropicOutputConfig struct {
	Format anthropicFormat `json:"format"`
}

// anthropicRequest is the entire request body. It deliberately has no
// temperature, top_p, top_k, thinking or effort field.
type anthropicRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens"`
	System       string                 `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicReply struct {
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
}

// completeAnthropic performs one Messages call against base and returns the
// concatenated text blocks, verbatim.
func completeAnthropic(ctx context.Context, hc *http.Client, base *url.URL, p LLMProvider, req LLMRequest) (string, error) {
	if p.Model == "" {
		return "", status.Error(codes.FailedPrecondition, "llm provider has no model configured")
	}
	maxTokens := req.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = llmDefaultMaxOutputTokens
	}
	body := anthropicRequest{
		Model:     p.Model,
		MaxTokens: maxTokens,
		System:    req.System,
		Messages:  []anthropicMessage{{Role: "user", Content: req.User}},
	}
	if req.Schema != nil {
		body.OutputConfig = &anthropicOutputConfig{Format: anthropicFormat{Type: "json_schema", Schema: req.Schema}}
	}

	var reply anthropicReply
	err := postJSON(ctx, hc, base.JoinPath("v1", "messages").String(), map[string]string{
		"x-api-key":         p.APIKey,
		"anthropic-version": anthropicVersion,
	}, body, &reply)
	if err != nil {
		return "", err
	}

	switch reply.StopReason {
	case "max_tokens":
		return "", status.Error(codes.Internal, "llm provider ran out of output budget before finishing; raise the output budget or choose a non-reasoning model")
	case "refusal":
		return "", status.Error(codes.Internal, "llm provider declined to answer")
	}

	var text strings.Builder
	for _, block := range reply.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if text.Len() == 0 {
		return "", status.Error(codes.Internal, "llm provider reply held no text")
	}
	return text.String(), nil
}
