package local

// The OpenAI-compatible chat-completions client over plain net/http. It
// reaches OpenAI itself and, through a base URL, Ollama, vLLM and most
// gateways (D-01). Transport and status classification are shared with the
// Anthropic client (postJSON in llm_client.go).
//
// Hard constraints (08-RESEARCH.md):
//   - OpenAI reasoning models reject max_tokens and require
//     max_completion_tokens, so that is the default; MaxTokensField is the
//     documented legacy escape hatch for servers that only know max_tokens;
//   - no response_format by default: arbitrary gateways do not all support
//     it, and the prompt asks for a bare JSON object that the host parses
//     strictly either way;
//   - Authorization is omitted entirely when the key is empty so a local
//     model server with no auth works.

import (
	"context"
	"net/http"
	"net/url"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// openAIDefaultTokensField is the output-budget parameter name sent unless the
// provider's sealed config overrides it.
const openAIDefaultTokensField = "max_completion_tokens"

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChoice struct {
	Message struct {
		Content string  `json:"content"`
		Refusal *string `json:"refusal"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

type openAIWireReply struct {
	Choices []openAIChoice `json:"choices"`
}

// completeOpenAICompatible performs one chat-completions call against base and
// returns the first choice's message content, verbatim.
func completeOpenAICompatible(ctx context.Context, hc *http.Client, base *url.URL, p LLMProvider, req LLMRequest) (string, error) {
	if p.Model == "" {
		return "", status.Error(codes.FailedPrecondition, "llm provider has no model configured")
	}
	tokensField := p.MaxTokensField
	if tokensField == "" {
		tokensField = openAIDefaultTokensField
	}
	maxTokens := req.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = llmDefaultMaxOutputTokens
	}
	messages := make([]openAIMessage, 0, 2)
	if req.System != "" {
		messages = append(messages, openAIMessage{Role: "system", Content: req.System})
	}
	messages = append(messages, openAIMessage{Role: "user", Content: req.User})

	// A map so the token field's name is configurable. The fixed keys are set
	// after it, so a sealed override can never displace the model or messages.
	body := map[string]any{tokensField: maxTokens}
	body["model"] = p.Model
	body["messages"] = messages

	headers := map[string]string{}
	if p.APIKey != "" {
		headers["Authorization"] = "Bearer " + p.APIKey
	}

	var reply openAIWireReply
	if err := postJSON(ctx, hc, base.JoinPath("chat", "completions").String(), headers, body, &reply); err != nil {
		return "", err
	}
	if len(reply.Choices) == 0 {
		return "", status.Error(codes.Internal, "llm provider reply held no choices")
	}
	choice := reply.Choices[0]
	if choice.Message.Refusal != nil && *choice.Message.Refusal != "" {
		return "", status.Error(codes.Internal, "llm provider declined to answer")
	}
	switch choice.FinishReason {
	case "length":
		return "", status.Error(codes.Internal, "llm provider ran out of output budget before finishing; raise the output budget or choose a non-reasoning model")
	case "content_filter":
		return "", status.Error(codes.Internal, "llm provider filtered the answer")
	}
	if choice.Message.Content == "" {
		return "", status.Error(codes.Internal, "llm provider reply held no text")
	}
	return choice.Message.Content, nil
}
