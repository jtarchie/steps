package llmclient

// Ported from adk-utils-go's genai/openai/completions adapter, Copyright 2026 Alby Hernández <hola@achetronic.com>, Apache-2.0: the request and response conversions are its, the wire is net/http instead of openai-go, which was ~6MB of the binary because its package init registers every API union type it defines. Retries, backoff and Retry-After stay requestRetryTransport's (requests.go) and the response-header bound agentBaseTransport's, as they were while openai-go's own retries were switched off.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"net/http"
	"strings"

	"github.com/achetronic/adk-utils-go/genai/common"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// errNoChoices is a 2xx answer with nothing in it.
var errNoChoices = errors.New("no choices in OpenAI response")

// OpenAI enforces a 40-character limit on tool_call_id fields.
const maxToolCallIDLength = 40

// APIError is a non-2xx answer from the provider: its status, and the fields of the body's "error" object when it had one. Error() is the status alone, as openai-go's was: a provider's message can echo request content.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Type       string
	Param      string
}

func (e *APIError) Error() string {
	if text := http.StatusText(e.StatusCode); text != "" {
		return fmt.Sprintf("OpenAI API error: %d %s", e.StatusCode, text)
	}

	return fmt.Sprintf("OpenAI API error: %d", e.StatusCode)
}

// chatModel is one model at one endpoint.
type chatModel struct {
	client   *http.Client
	endpoint string
	apiKey   string
	name     string
	// deepSeek sends the model's reasoning back as reasoning_content and reads it the same way; every other model is OpenAI-pure: reasoning is read from no field and folds into content as <think> tags on the way back.
	deepSeek bool
}

func newChatModel(client *http.Client, baseURL, apiKey, name string) *chatModel {
	return &chatModel{
		client:   client,
		endpoint: strings.TrimSuffix(baseURL, "/") + "/chat/completions",
		apiKey:   apiKey,
		name:     name,
		deepSeek: strings.Contains(strings.ToLower(name), "deepseek"),
	}
}

func (m *chatModel) Name() string { return m.name }

// ponytail: stream is ignored — every caller in steps asks for false, so a streamed call gets the whole answer as one final response. Real streaming needs an SSE reader and a tool-call delta accumulator here.
func (m *chatModel) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		resp, err := m.generate(ctx, req)
		yield(resp, err)
	}
}

func (m *chatModel) generate(ctx context.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	params, err := m.buildParams(req)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encoding the chat request: %w", err)
	}

	// A *bytes.Reader body is what gives the request a GetBody, which requestRetryTransport replays.
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building the chat request: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	if m.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+m.apiKey)
	}

	response, err := m.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("chat request: %w", err)
	}

	defer func() { _ = response.Body.Close() }()

	contents, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %w", err)
	}

	if response.StatusCode >= http.StatusBadRequest {
		return nil, decodeAPIError(response.StatusCode, contents)
	}

	var completion chatCompletion

	err = json.Unmarshal(contents, &completion)
	if err != nil {
		return nil, fmt.Errorf("decoding the chat response: %w", err)
	}

	return m.convertResponse(&completion)
}

func decodeAPIError(status int, contents []byte) *APIError {
	var body struct {
		Error struct {
			Code    lenientString `json:"code"`
			Message lenientString `json:"message"`
			Type    lenientString `json:"type"`
			Param   lenientString `json:"param"`
		} `json:"error"`
	}

	_ = json.Unmarshal(contents, &body)

	return &APIError{
		StatusCode: status,
		Code:       string(body.Error.Code),
		Message:    string(body.Error.Message),
		Type:       string(body.Error.Type),
		Param:      string(body.Error.Param),
	}
}

// lenientString reads a JSON string, and anything else — null, a number, an array — as empty, the way openai-go's decoder tolerated a provider's off-schema field instead of failing the whole response.
type lenientString string

func (s *lenientString) UnmarshalJSON(data []byte) error {
	var text string
	if json.Unmarshal(data, &text) == nil {
		*s = lenientString(text)
	}

	return nil
}

// lenientInt reads a JSON number, integral or not, and anything else as zero.
type lenientInt int64

func (n *lenientInt) UnmarshalJSON(data []byte) error {
	var number float64
	if json.Unmarshal(data, &number) == nil {
		*n = lenientInt(number)
	}

	return nil
}

type chatCompletion struct {
	Choices []struct {
		FinishReason lenientString   `json:"finish_reason"`
		Message      json.RawMessage `json:"message"`
	} `json:"choices"`
	Usage chatUsage `json:"usage"`
}

type chatMessage struct {
	Content   lenientString `json:"content"`
	ToolCalls []struct {
		ID       lenientString `json:"id"`
		Function struct {
			Name      lenientString `json:"name"`
			Arguments lenientString `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type chatUsage struct {
	PromptTokens        lenientInt `json:"prompt_tokens"`
	CompletionTokens    lenientInt `json:"completion_tokens"`
	TotalTokens         lenientInt `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens lenientInt `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens lenientInt `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// reasoningReadFields are where DeepSeek, Kimi and Mistral (reasoning_content) and OpenRouter and vLLM (reasoning) put the reasoning text; the first non-empty one wins.
var reasoningReadFields = []string{"reasoning_content", "reasoning"} //nolint:gochecknoglobals // a constant list

func (m *chatModel) convertResponse(completion *chatCompletion) (*model.LLMResponse, error) {
	if len(completion.Choices) == 0 {
		return nil, errNoChoices
	}

	choice := completion.Choices[0]

	var message chatMessage

	_ = json.Unmarshal(choice.Message, &message)

	content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{}}

	if m.deepSeek {
		if text := readReasoning(choice.Message); text != "" {
			content.Parts = append(content.Parts, &genai.Part{Text: text, Thought: true})
		}
	}

	if message.Content != "" {
		content.Parts = append(content.Parts, &genai.Part{Text: string(message.Content)})
	}

	for _, call := range message.ToolCalls {
		content.Parts = append(content.Parts, &genai.Part{
			FunctionCall: &genai.FunctionCall{
				ID:   string(call.ID),
				Name: string(call.Function.Name),
				Args: parseJSONArgs(string(call.Function.Arguments)),
			},
		})
	}

	return &model.LLMResponse{
		Content:       content,
		UsageMetadata: convertUsageMetadata(completion.Usage),
		FinishReason:  convertFinishReason(string(choice.FinishReason)),
		TurnComplete:  true,
	}, nil
}

func readReasoning(raw json.RawMessage) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return ""
	}

	for _, field := range reasoningReadFields {
		var text string
		if json.Unmarshal(fields[field], &text) == nil && text != "" {
			return text
		}
	}

	return ""
}

// buildParams is the request body, as a map: a few of its fields are a string in one case and an object in another.
func (m *chatModel) buildParams(req *model.LLMRequest) (map[string]any, error) {
	var messages []map[string]any

	if req.Config != nil && req.Config.SystemInstruction != nil {
		if text := extractText(req.Config.SystemInstruction); text != "" {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}

	for _, content := range req.Contents {
		converted, err := m.convertContent(content)
		if err != nil {
			return nil, err
		}

		messages = append(messages, converted...)
	}

	if m.deepSeek {
		requireReasoningKey(messages)
	}

	params := map[string]any{"model": m.name, "messages": messages}

	if req.Config != nil {
		applyGenerationConfig(params, req.Config)
	}

	return params, nil
}

// requireReasoningKey gives every assistant message a reasoning_content: steps injects context_paths: and upstream decisions as synthetic tool calls no model reasoned about, and DeepSeek in thinking mode rejects any tool-history assistant turn missing the key.
func requireReasoningKey(messages []map[string]any) {
	for _, message := range messages {
		if _, ok := message["reasoning_content"]; !ok && message["role"] == "assistant" {
			message["reasoning_content"] = ""
		}
	}
}

func applyGenerationConfig(params map[string]any, cfg *genai.GenerateContentConfig) {
	if cfg.Temperature != nil {
		params["temperature"] = float64(*cfg.Temperature)
	}

	if cfg.MaxOutputTokens > 0 {
		params["max_tokens"] = cfg.MaxOutputTokens
	}

	if cfg.TopP != nil {
		params["top_p"] = float64(*cfg.TopP)
	}

	if stop := stopParam(cfg.StopSequences); stop != nil {
		params["stop"] = stop
	}

	if cfg.ThinkingConfig != nil {
		params["reasoning_effort"] = convertThinkingLevel(cfg.ThinkingConfig.ThinkingLevel)
	}

	if format := responseFormat(cfg); format != nil {
		params["response_format"] = format
	}

	if tools := convertTools(cfg.Tools); len(tools) > 0 {
		params["tools"] = tools
	}

	if choice := toolChoice(cfg.ToolConfig); choice != nil {
		params["tool_choice"] = choice
	}
}

// stopParam is one stop sequence as a string, several as an array.
func stopParam(sequences []string) any {
	switch len(sequences) {
	case 0:
		return nil
	case 1:
		return sequences[0]
	default:
		return sequences
	}
}

func responseFormat(cfg *genai.GenerateContentConfig) map[string]any {
	switch {
	case cfg.ResponseSchema != nil:
		return map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":        "response",
				"description": cfg.ResponseSchema.Description,
				"schema":      convertSchema(cfg.ResponseSchema),
				"strict":      true,
			},
		}
	case cfg.ResponseMIMEType == "application/json":
		return map[string]any{"type": "json_object"}
	default:
		return nil
	}
}

// toolChoice maps genai's calling mode to tool_choice: Auto is "auto", None "none", and Any "required" — or a named function when exactly one is allowed, since OpenAI's tool_choice names at most one.
func toolChoice(cfg *genai.ToolConfig) any {
	if cfg == nil || cfg.FunctionCallingConfig == nil {
		return nil
	}

	fcc := cfg.FunctionCallingConfig

	switch fcc.Mode { //nolint:exhaustive // the unspecified and validated modes send no tool_choice
	case genai.FunctionCallingConfigModeAuto:
		return "auto"
	case genai.FunctionCallingConfigModeNone:
		return "none"
	case genai.FunctionCallingConfigModeAny:
		if len(fcc.AllowedFunctionNames) == 1 {
			return map[string]any{"type": "function", "function": map[string]any{"name": fcc.AllowedFunctionNames[0]}}
		}

		return "required"
	default:
		return nil
	}
}

// contentParts is one genai.Content sorted by what each part becomes on the wire.
type contentParts struct {
	// messages are the tool results, each its own message ahead of the turn's own.
	messages  []map[string]any
	texts     []string
	thoughts  []string
	toolCalls []map[string]any
	media     []map[string]any
}

func collectParts(content *genai.Content) (contentParts, error) {
	var parts contentParts

	for _, part := range content.Parts {
		err := parts.add(part)
		if err != nil {
			return contentParts{}, err
		}
	}

	return parts, nil
}

func (c *contentParts) add(part *genai.Part) error {
	switch {
	// Ahead of the Text case: a thought carries its text in Text, and merged into the reply it hides the reasoning inside the answer.
	case part.Thought:
		if part.Text != "" {
			c.thoughts = append(c.thoughts, part.Text)
		}
	case part.FunctionResponse != nil || part.FunctionCall != nil:
		return c.addTool(part)
	case part.Text != "":
		c.texts = append(c.texts, part.Text)
	case part.InlineData != nil || part.FileData != nil:
		return c.addMedia(part)
	}

	return nil
}

func (c *contentParts) addTool(part *genai.Part) error {
	if response := part.FunctionResponse; response != nil {
		// ADK's own confirmation protocol: providers reject it as an undeclared tool.
		if common.IsADKInternalCall(response.Name) {
			return nil
		}

		encoded, err := common.MarshalToolPayload(response.Response)
		if err != nil {
			return fmt.Errorf("failed to marshal function response: %w", err)
		}

		c.messages = append(c.messages, map[string]any{
			"role": "tool", "content": string(encoded), "tool_call_id": normalizeToolCallID(response.ID),
		})

		return nil
	}

	call := part.FunctionCall
	if common.IsADKInternalCall(call.Name) {
		return nil
	}

	args, err := common.MarshalToolPayload(call.Args)
	if err != nil {
		return fmt.Errorf("failed to marshal function args: %w", err)
	}

	c.toolCalls = append(c.toolCalls, map[string]any{
		"id":       normalizeToolCallID(call.ID),
		"type":     "function",
		"function": map[string]any{"name": call.Name, "arguments": string(args)},
	})

	return nil
}

func (c *contentParts) addMedia(part *genai.Part) error {
	var (
		converted map[string]any
		err       error
	)

	if part.InlineData != nil {
		converted, err = convertInlineData(part.InlineData)
	} else {
		converted, err = convertFileData(part.FileData)
	}

	if err != nil {
		return err
	}

	c.media = append(c.media, converted)

	return nil
}

// convertContent turns one genai.Content into zero or more chat messages.
func (m *chatModel) convertContent(content *genai.Content) ([]map[string]any, error) {
	parts, err := collectParts(content)
	if err != nil {
		return nil, err
	}

	role := convertRole(content.Role)
	texts, reasoning := m.foldReasoning(role, parts)

	// Reasoning alone is not a message: an assistant message with neither content nor tool_calls is invalid.
	if len(texts) == 0 && len(parts.media) == 0 && len(parts.toolCalls) == 0 {
		return parts.messages, nil
	}

	switch role {
	case "user":
		return append(parts.messages, userMessage(texts, parts.media)), nil
	case "assistant":
		return append(parts.messages, assistantMessage(texts, parts.toolCalls, reasoning)), nil
	case "system":
		return append(parts.messages, map[string]any{"role": "system", "content": joinTexts(texts)}), nil
	default:
		return parts.messages, nil
	}
}

// foldReasoning places a turn's thoughts: DeepSeek takes them back as reasoning_content, every other model as <think> tags ahead of the reply. Reasoning belongs to an assistant turn only: ADK rewrites another agent's events as user-role "For context:" content, thoughts included, and no provider accepts reasoning on a user message.
func (m *chatModel) foldReasoning(role string, parts contentParts) ([]string, string) {
	if role != "assistant" || len(parts.thoughts) == 0 {
		return parts.texts, ""
	}

	if m.deepSeek {
		return parts.texts, joinTexts(parts.thoughts)
	}

	return append([]string{"<think>\n" + joinTexts(parts.thoughts) + "\n</think>"}, parts.texts...), ""
}

func assistantMessage(texts []string, toolCalls []map[string]any, reasoning string) map[string]any {
	message := map[string]any{"role": "assistant"}
	if len(texts) > 0 {
		message["content"] = joinTexts(texts)
	}

	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}

	return message
}

func userMessage(texts []string, media []map[string]any) map[string]any {
	if len(media) == 0 {
		return map[string]any{"role": "user", "content": joinTexts(texts)}
	}

	parts := make([]map[string]any, 0, len(texts)+len(media))
	for _, text := range texts {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}

	return map[string]any{"role": "user", "content": append(parts, media...)}
}

func convertTools(genaiTools []*genai.Tool) []map[string]any {
	var tools []map[string]any

	for _, genaiTool := range genaiTools {
		if genaiTool == nil {
			continue
		}

		for _, decl := range genaiTool.FunctionDeclarations {
			params := decl.ParametersJsonSchema
			if params == nil {
				params = decl.Parameters
			}

			function := map[string]any{"name": decl.Name, "description": decl.Description}
			if converted := convertToFunctionParams(params); converted != nil {
				function["parameters"] = converted
			}

			tools = append(tools, map[string]any{"type": "function", "function": function})
		}
	}

	return tools
}

// convertToFunctionParams is a tool's parameter schema as OpenAI accepts it: lowercase types, and a properties field on every object.
func convertToFunctionParams(params any) map[string]any {
	if params == nil {
		return nil
	}

	schema, ok := params.(map[string]any)
	if !ok {
		// Any other shape — a *jsonschema.Schema, a *genai.Schema — through its JSON.
		encoded, err := json.Marshal(params)
		if err != nil || json.Unmarshal(encoded, &schema) != nil {
			return nil
		}
	}

	if schema == nil {
		return nil
	}

	lowercaseTypes(schema)
	ensureObjectProperties(schema)

	return schema
}

func ensureObjectProperties(schema map[string]any) {
	if kind, ok := schema["type"].(string); ok && kind == "object" {
		if _, has := schema["properties"]; !has {
			schema["properties"] = map[string]any{}
		}
	}

	if props, ok := schema["properties"].(map[string]any); ok {
		for _, prop := range props {
			if nested, ok := prop.(map[string]any); ok {
				ensureObjectProperties(nested)
			}
		}
	}

	if items, ok := schema["items"].(map[string]any); ok {
		ensureObjectProperties(items)
	}
}

func lowercaseTypes(schema map[string]any) {
	for key, value := range schema {
		switch typed := value.(type) {
		case string:
			if key == "type" {
				schema[key] = strings.ToLower(typed)
			}
		case map[string]any:
			lowercaseTypes(typed)
		case []any:
			for _, item := range typed {
				if nested, ok := item.(map[string]any); ok {
					lowercaseTypes(nested)
				}
			}
		}
	}
}

func convertSchema(schema *genai.Schema) map[string]any {
	result := map[string]any{}

	if schema.Type != genai.TypeUnspecified {
		result["type"] = schemaTypeToString(schema.Type)
	}

	if schema.Description != "" {
		result["description"] = schema.Description
	}

	if len(schema.Required) > 0 {
		result["required"] = schema.Required
	}

	if len(schema.Enum) > 0 {
		result["enum"] = schema.Enum
	}

	if len(schema.Properties) > 0 {
		props := make(map[string]any, len(schema.Properties))
		for name, prop := range schema.Properties {
			props[name] = convertSchema(prop)
		}

		result["properties"] = props
	}

	if schema.Items != nil {
		result["items"] = convertSchema(schema.Items)
	}

	return result
}

// normalizeToolCallID keeps an ID within OpenAI's 40 characters: a longer one is hashed shorter, the same way on every send, so a call and its response still pair.
func normalizeToolCallID(id string) string {
	if len(id) <= maxToolCallIDLength {
		return id
	}

	hash := sha256.Sum256([]byte(id))

	return "tc_" + hex.EncodeToString(hash[:])[:maxToolCallIDLength-3]
}

// convertInlineData is an image as a data URI, audio as input_audio, a PDF or text as a file; anything else is refused rather than silently dropped.
func convertInlineData(data *genai.Blob) (map[string]any, error) {
	mediaType := normalizeMIMEType(data.MIMEType)
	encoded := base64.StdEncoding.EncodeToString(data.Data)

	switch {
	case isImage(mediaType):
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mediaType + ";base64," + encoded, "detail": "auto"}}, nil
	case audioFormats[mediaType] != "":
		return map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": encoded, "format": audioFormats[mediaType]}}, nil
	case mediaType == "application/pdf" || strings.HasPrefix(mediaType, "text/"):
		return map[string]any{"type": "file", "file": map[string]any{"file_data": "data:" + mediaType + ";base64," + encoded}}, nil
	default:
		return nil, fmt.Errorf("unsupported inline data MIME type for OpenAI: %s", mediaType)
	}
}

// audioFormats is input_audio's format for each audio type it takes.
var audioFormats = map[string]string{"audio/wav": "wav", "audio/webm": "wav", "audio/mp3": "mp3", "audio/mpeg": "mp3"} //nolint:gochecknoglobals // a constant table

// convertFileData passes an http(s) image URL through verbatim; nothing is downloaded.
func convertFileData(data *genai.FileData) (map[string]any, error) {
	if strings.HasPrefix(data.FileURI, "gs://") {
		return nil, fmt.Errorf("file data URI %q is a Google Cloud Storage URI, which OpenAI cannot fetch: download the bytes and use InlineData instead", data.FileURI)
	}

	if !strings.HasPrefix(data.FileURI, "https://") && !strings.HasPrefix(data.FileURI, "http://") {
		return nil, fmt.Errorf("file data URI must be an http(s) URL for OpenAI, got %q", data.FileURI)
	}

	mediaType := normalizeMIMEType(data.MIMEType)
	if !isImage(mediaType) {
		return nil, fmt.Errorf("unsupported file data MIME type for OpenAI: %s", mediaType)
	}

	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": data.FileURI, "detail": "auto"}}, nil
}

func isImage(mediaType string) bool {
	switch mediaType {
	case "image/jpeg", "image/jpg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func normalizeMIMEType(mimeType string) string {
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return mimeType
	}

	return mediaType
}

// convertUsageMetadata is nil when the provider reported no tokens, so a call it did not price never reads as one that cost nothing.
func convertUsageMetadata(usage chatUsage) *genai.GenerateContentResponseUsageMetadata {
	if usage.TotalTokens == 0 {
		return nil
	}

	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        int32(usage.PromptTokens),                            //nolint:gosec // token counts fit
		CandidatesTokenCount:    int32(usage.CompletionTokens),                        //nolint:gosec // token counts fit
		TotalTokenCount:         int32(usage.TotalTokens),                             //nolint:gosec // token counts fit
		ThoughtsTokenCount:      int32(usage.CompletionTokensDetails.ReasoningTokens), //nolint:gosec // token counts fit
		CachedContentTokenCount: int32(usage.PromptTokensDetails.CachedTokens),        //nolint:gosec // token counts fit
	}
}

func convertRole(role string) string {
	if role == genai.RoleModel {
		return "assistant"
	}

	return role
}

func convertFinishReason(reason string) genai.FinishReason {
	switch reason {
	case "stop", "tool_calls", "function_call":
		return genai.FinishReasonStop
	case "length":
		return genai.FinishReasonMaxTokens
	case "content_filter":
		return genai.FinishReasonSafety
	default:
		return genai.FinishReasonUnspecified
	}
}

func convertThinkingLevel(level genai.ThinkingLevel) string {
	switch level { //nolint:exhaustive // everything but low and high is medium
	case genai.ThinkingLevelLow:
		return "low"
	case genai.ThinkingLevelHigh:
		return "high"
	default:
		return "medium"
	}
}

func schemaTypeToString(kind genai.Type) string {
	switch kind { //nolint:exhaustive // an unknown type is a string, as it was
	case genai.TypeNumber:
		return "number"
	case genai.TypeInteger:
		return "integer"
	case genai.TypeBoolean:
		return "boolean"
	case genai.TypeArray:
		return "array"
	case genai.TypeObject:
		return "object"
	default:
		return "string"
	}
}

func extractText(content *genai.Content) string {
	var texts []string

	for _, part := range content.Parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}

	return joinTexts(texts)
}

func joinTexts(texts []string) string {
	return strings.Join(texts, "\n")
}

// parseJSONArgs is a tool call's arguments as a map, empty when they are missing or not JSON (repair.go fixes what it can before this sees it).
func parseJSONArgs(argsJSON string) map[string]any {
	args := map[string]any{}
	if argsJSON != "" && json.Unmarshal([]byte(argsJSON), &args) != nil {
		return map[string]any{}
	}

	return args
}
