package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type canonicalBlock struct {
	Type    string
	Text    string
	URL     string
	Media   string
	Data    string
	ID      string
	Name    string
	Input   any
	ToolID  string
	Content any
	IsError bool
}

type canonicalMessage struct {
	Role   string
	Blocks []canonicalBlock
}

type canonicalTool struct {
	Name        string
	Description string
	Parameters  any
	Strict      bool
}

type canonicalRequest struct {
	Model       string
	System      []canonicalBlock
	Messages    []canonicalMessage
	Tools       []canonicalTool
	ToolChoice  any
	Stream      bool
	Temperature any
	TopP        any
	MaxTokens   any
	Stop        any
	Reasoning   any
	Metadata    any
}

type canonicalUsage struct {
	Input     int
	Output    int
	Total     int
	Cached    int
	Reasoning int
}

type canonicalResponse struct {
	ID      string
	Model   string
	Text    string
	Tools   []canonicalBlock
	Stop    string
	Usage   canonicalUsage
	Created int64
}

func convertRequest(from, to Protocol, input map[string]any) (map[string]any, error) {
	if from == to {
		return cloneMap(input), nil
	}
	canonical, err := decodeRequest(from, input)
	if err != nil {
		return nil, err
	}
	return encodeRequest(to, canonical), nil
}

func decodeRequest(protocol Protocol, input map[string]any) (canonicalRequest, error) {
	request := canonicalRequest{
		Model:       stringAt(input, "model"),
		Stream:      boolAt(input, "stream"),
		Temperature: input["temperature"],
		TopP:        input["top_p"],
		Metadata:    input["metadata"],
	}
	switch protocol {
	case ProtocolChat:
		request.MaxTokens = firstAny(input["max_completion_tokens"], input["max_tokens"])
		request.Stop = input["stop"]
		request.Reasoning = firstAny(input["reasoning_effort"], input["reasoning"])
		for _, raw := range sliceAt(input, "messages") {
			message, ok := raw.(map[string]any)
			if !ok {
				return request, fmt.Errorf("messages must contain objects")
			}
			role := stringAt(message, "role")
			blocks := decodeOpenAIContent(message["content"])
			if role == "system" || role == "developer" {
				request.System = append(request.System, blocks...)
				continue
			}
			if role == "tool" {
				blocks = []canonicalBlock{{Type: "tool_result", ToolID: stringAt(message, "tool_call_id"), Content: message["content"]}}
				role = "user"
			}
			for _, rawCall := range sliceAt(message, "tool_calls") {
				call, _ := rawCall.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				blocks = append(blocks, canonicalBlock{Type: "tool_use", ID: stringAt(call, "id"), Name: stringAt(fn, "name"), Input: parseJSONOrString(stringAt(fn, "arguments"))})
			}
			request.Messages = append(request.Messages, canonicalMessage{Role: role, Blocks: blocks})
		}
		for _, raw := range sliceAt(input, "tools") {
			tool, _ := raw.(map[string]any)
			fn, _ := tool["function"].(map[string]any)
			request.Tools = append(request.Tools, canonicalTool{Name: stringAt(fn, "name"), Description: stringAt(fn, "description"), Parameters: fn["parameters"], Strict: boolAt(fn, "strict")})
		}
		request.ToolChoice = decodeChatToolChoice(input["tool_choice"])
	case ProtocolResponses:
		request.MaxTokens = input["max_output_tokens"]
		request.Stop = input["stop"]
		request.Reasoning = input["reasoning"]
		request.System = append(request.System, decodeOpenAIContent(input["instructions"])...)
		switch raw := input["input"].(type) {
		case string:
			request.Messages = append(request.Messages, canonicalMessage{Role: "user", Blocks: []canonicalBlock{{Type: "text", Text: raw}}})
		case []any:
			for _, itemRaw := range raw {
				item, ok := itemRaw.(map[string]any)
				if !ok {
					return request, fmt.Errorf("input must contain objects")
				}
				switch stringAt(item, "type") {
				case "function_call":
					request.Messages = append(request.Messages, canonicalMessage{Role: "assistant", Blocks: []canonicalBlock{{Type: "tool_use", ID: stringAt(item, "call_id"), Name: stringAt(item, "name"), Input: parseJSONOrString(stringAt(item, "arguments"))}}})
				case "function_call_output":
					request.Messages = append(request.Messages, canonicalMessage{Role: "user", Blocks: []canonicalBlock{{Type: "tool_result", ToolID: stringAt(item, "call_id"), Content: item["output"]}}})
				default:
					role := stringAt(item, "role")
					blocks := decodeOpenAIContent(item["content"])
					if role == "system" || role == "developer" {
						request.System = append(request.System, blocks...)
					} else {
						request.Messages = append(request.Messages, canonicalMessage{Role: role, Blocks: blocks})
					}
				}
			}
		default:
			return request, fmt.Errorf("input must be a string or array")
		}
		for _, raw := range sliceAt(input, "tools") {
			tool, _ := raw.(map[string]any)
			if stringAt(tool, "type") != "function" {
				return request, fmt.Errorf("only function tools can be converted between protocols")
			}
			request.Tools = append(request.Tools, canonicalTool{Name: stringAt(tool, "name"), Description: stringAt(tool, "description"), Parameters: tool["parameters"], Strict: boolAt(tool, "strict")})
		}
		request.ToolChoice = decodeResponsesToolChoice(input["tool_choice"])
	case ProtocolAnthropic:
		request.MaxTokens = input["max_tokens"]
		request.Stop = input["stop_sequences"]
		request.Reasoning = firstAny(input["thinking"], input["effort"])
		request.System = decodeAnthropicContent(input["system"])
		for _, raw := range sliceAt(input, "messages") {
			message, ok := raw.(map[string]any)
			if !ok {
				return request, fmt.Errorf("messages must contain objects")
			}
			request.Messages = append(request.Messages, canonicalMessage{Role: stringAt(message, "role"), Blocks: decodeAnthropicContent(message["content"])})
		}
		for _, raw := range sliceAt(input, "tools") {
			tool, _ := raw.(map[string]any)
			request.Tools = append(request.Tools, canonicalTool{Name: stringAt(tool, "name"), Description: stringAt(tool, "description"), Parameters: tool["input_schema"]})
		}
		request.ToolChoice = decodeAnthropicToolChoice(input["tool_choice"])
	default:
		return request, fmt.Errorf("unsupported input protocol %q", protocol)
	}
	return request, nil
}

func encodeRequest(protocol Protocol, request canonicalRequest) map[string]any {
	output := map[string]any{"model": request.Model, "stream": request.Stream}
	put(output, "temperature", request.Temperature)
	put(output, "top_p", request.TopP)
	put(output, "metadata", request.Metadata)
	switch protocol {
	case ProtocolChat:
		messages := make([]any, 0, len(request.Messages)+1)
		if len(request.System) > 0 {
			messages = append(messages, map[string]any{"role": "system", "content": encodeChatContent(request.System)})
		}
		for _, message := range request.Messages {
			messages = append(messages, encodeChatMessages(message)...)
		}
		output["messages"] = messages
		put(output, "max_tokens", request.MaxTokens)
		put(output, "stop", request.Stop)
		if effort := reasoningEffort(request.Reasoning); effort != nil {
			output["reasoning_effort"] = effort
		}
		if request.Stream {
			output["stream_options"] = map[string]any{"include_usage": true}
		}
		if len(request.Tools) > 0 {
			tools := make([]any, 0, len(request.Tools))
			for _, tool := range request.Tools {
				tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": schemaOrDefault(tool.Parameters), "strict": tool.Strict}})
			}
			output["tools"] = tools
			put(output, "tool_choice", encodeChatToolChoice(request.ToolChoice))
		}
	case ProtocolResponses:
		if len(request.System) > 0 {
			output["instructions"] = blocksText(request.System)
		}
		items := make([]any, 0, len(request.Messages))
		for _, message := range request.Messages {
			items = append(items, encodeResponsesMessage(message)...)
		}
		output["input"] = items
		put(output, "max_output_tokens", request.MaxTokens)
		put(output, "stop", request.Stop)
		if request.Reasoning != nil {
			switch v := request.Reasoning.(type) {
			case string:
				output["reasoning"] = map[string]any{"effort": v}
			default:
				output["reasoning"] = v
			}
		}
		if len(request.Tools) > 0 {
			tools := make([]any, 0, len(request.Tools))
			for _, tool := range request.Tools {
				tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": schemaOrDefault(tool.Parameters), "strict": tool.Strict})
			}
			output["tools"] = tools
			put(output, "tool_choice", encodeResponsesToolChoice(request.ToolChoice))
		}
	case ProtocolAnthropic:
		if len(request.System) > 0 {
			output["system"] = encodeAnthropicContent(request.System)
		}
		messages := make([]any, 0, len(request.Messages))
		for _, message := range request.Messages {
			messages = append(messages, map[string]any{"role": normalizeAnthropicRole(message.Role), "content": encodeAnthropicContent(message.Blocks)})
		}
		output["messages"] = messages
		if request.MaxTokens == nil {
			output["max_tokens"] = 4096
		} else {
			output["max_tokens"] = request.MaxTokens
		}
		put(output, "stop_sequences", request.Stop)
		if effort := reasoningEffort(request.Reasoning); effort != nil {
			output["effort"] = effort
		}
		if len(request.Tools) > 0 {
			tools := make([]any, 0, len(request.Tools))
			for _, tool := range request.Tools {
				tools = append(tools, map[string]any{"name": tool.Name, "description": tool.Description, "input_schema": schemaOrDefault(tool.Parameters)})
			}
			output["tools"] = tools
			put(output, "tool_choice", encodeAnthropicToolChoice(request.ToolChoice))
		}
	}
	return output
}

func decodeOpenAIContent(value any) []canonicalBlock {
	switch value := value.(type) {
	case string:
		if value == "" {
			return nil
		}
		return []canonicalBlock{{Type: "text", Text: value}}
	case []any:
		var blocks []canonicalBlock
		for _, raw := range value {
			part, _ := raw.(map[string]any)
			switch stringAt(part, "type") {
			case "text", "input_text", "output_text":
				blocks = append(blocks, canonicalBlock{Type: "text", Text: stringAt(part, "text")})
			case "image_url":
				blocks = append(blocks, canonicalBlock{Type: "image", URL: stringAt(part, "image_url", "url")})
			case "input_image":
				blocks = append(blocks, canonicalBlock{Type: "image", URL: stringAt(part, "image_url")})
			}
		}
		return blocks
	case map[string]any:
		return decodeOpenAIContent([]any{value})
	default:
		return nil
	}
}

func decodeAnthropicContent(value any) []canonicalBlock {
	if text, ok := value.(string); ok {
		return []canonicalBlock{{Type: "text", Text: text}}
	}
	var blocks []canonicalBlock
	for _, raw := range asSlice(value) {
		part, _ := raw.(map[string]any)
		switch stringAt(part, "type") {
		case "text":
			blocks = append(blocks, canonicalBlock{Type: "text", Text: stringAt(part, "text")})
		case "image":
			source, _ := part["source"].(map[string]any)
			if stringAt(source, "type") == "url" {
				blocks = append(blocks, canonicalBlock{Type: "image", URL: stringAt(source, "url")})
			} else {
				blocks = append(blocks, canonicalBlock{Type: "image", Media: stringAt(source, "media_type"), Data: stringAt(source, "data")})
			}
		case "tool_use":
			blocks = append(blocks, canonicalBlock{Type: "tool_use", ID: stringAt(part, "id"), Name: stringAt(part, "name"), Input: part["input"]})
		case "tool_result":
			blocks = append(blocks, canonicalBlock{Type: "tool_result", ToolID: stringAt(part, "tool_use_id"), Content: part["content"], IsError: boolAt(part, "is_error")})
		}
	}
	return blocks
}

func encodeChatMessages(message canonicalMessage) []any {
	out := map[string]any{"role": normalizeChatRole(message.Role)}
	var content []canonicalBlock
	var toolCalls []any
	var results []any
	for _, block := range message.Blocks {
		switch block.Type {
		case "tool_use":
			args, _ := json.Marshal(block.Input)
			toolCalls = append(toolCalls, map[string]any{"id": block.ID, "type": "function", "function": map[string]any{"name": block.Name, "arguments": string(args)}})
		case "tool_result":
			results = append(results, map[string]any{"role": "tool", "tool_call_id": block.ToolID, "content": contentString(block.Content)})
		default:
			content = append(content, block)
		}
	}
	if len(content) > 0 {
		out["content"] = encodeChatContent(content)
	} else {
		out["content"] = nil
	}
	if len(toolCalls) > 0 {
		out["tool_calls"] = toolCalls
	}
	messages := make([]any, 0, len(results)+1)
	if len(content) > 0 || len(toolCalls) > 0 {
		messages = append(messages, out)
	}
	messages = append(messages, results...)
	return messages
}

func encodeChatContent(blocks []canonicalBlock) any {
	if len(blocks) == 1 && blocks[0].Type == "text" {
		return blocks[0].Text
	}
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": block.Text})
		case "image":
			url := block.URL
			if url == "" && block.Data != "" {
				url = "data:" + block.Media + ";base64," + block.Data
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		}
	}
	return parts
}

func encodeResponsesMessage(message canonicalMessage) []any {
	var output []any
	var content []any
	for _, block := range message.Blocks {
		switch block.Type {
		case "text":
			kind := "input_text"
			if message.Role == "assistant" {
				kind = "output_text"
			}
			content = append(content, map[string]any{"type": kind, "text": block.Text})
		case "image":
			url := block.URL
			if url == "" {
				url = "data:" + block.Media + ";base64," + block.Data
			}
			content = append(content, map[string]any{"type": "input_image", "image_url": url})
		case "tool_use":
			args, _ := json.Marshal(block.Input)
			output = append(output, map[string]any{"type": "function_call", "call_id": block.ID, "name": block.Name, "arguments": string(args)})
		case "tool_result":
			output = append(output, map[string]any{"type": "function_call_output", "call_id": block.ToolID, "output": contentString(block.Content)})
		}
	}
	if len(content) > 0 {
		output = append([]any{map[string]any{"role": normalizeResponsesRole(message.Role), "content": content}}, output...)
	}
	return output
}

func encodeAnthropicContent(blocks []canonicalBlock) []any {
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": block.Text})
		case "image":
			if block.URL != "" {
				parts = append(parts, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": block.URL}})
			} else {
				parts = append(parts, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": block.Media, "data": block.Data}})
			}
		case "tool_use":
			parts = append(parts, map[string]any{"type": "tool_use", "id": block.ID, "name": block.Name, "input": block.Input})
		case "tool_result":
			parts = append(parts, map[string]any{"type": "tool_result", "tool_use_id": block.ToolID, "content": block.Content, "is_error": block.IsError})
		}
	}
	return parts
}

func convertResponse(from, to Protocol, body []byte) ([]byte, error) {
	var input map[string]any
	if err := json.Unmarshal(body, &input); err != nil {
		return nil, err
	}
	response, err := decodeResponse(from, input)
	if err != nil {
		return nil, err
	}
	return json.Marshal(encodeResponse(to, response))
}

func decodeResponse(protocol Protocol, input map[string]any) (canonicalResponse, error) {
	response := canonicalResponse{ID: stringAt(input, "id"), Model: stringAt(input, "model"), Created: int64At(input, "created")}
	if response.Created == 0 {
		response.Created = int64At(input, "created_at")
	}
	if response.Created == 0 {
		response.Created = time.Now().Unix()
	}
	switch protocol {
	case ProtocolChat:
		choices := sliceAt(input, "choices")
		if len(choices) == 0 {
			return response, fmt.Errorf("chat response has no choices")
		}
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		response.Text = blocksText(decodeOpenAIContent(message["content"]))
		for _, raw := range sliceAt(message, "tool_calls") {
			call, _ := raw.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			response.Tools = append(response.Tools, canonicalBlock{Type: "tool_use", ID: stringAt(call, "id"), Name: stringAt(fn, "name"), Input: parseJSONOrString(stringAt(fn, "arguments"))})
		}
		response.Stop = stringAt(choice, "finish_reason")
		response.Usage = decodeOpenAIUsage(mapAt(input, "usage"))
	case ProtocolResponses:
		for _, raw := range sliceAt(input, "output") {
			item, _ := raw.(map[string]any)
			switch stringAt(item, "type") {
			case "message":
				response.Text += blocksText(decodeOpenAIContent(item["content"]))
			case "function_call":
				response.Tools = append(response.Tools, canonicalBlock{Type: "tool_use", ID: stringAt(item, "call_id"), Name: stringAt(item, "name"), Input: parseJSONOrString(stringAt(item, "arguments"))})
			}
		}
		response.Stop = "stop"
		if len(response.Tools) > 0 {
			response.Stop = "tool_calls"
		}
		response.Usage = decodeOpenAIUsage(mapAt(input, "usage"))
	case ProtocolAnthropic:
		for _, block := range decodeAnthropicContent(input["content"]) {
			if block.Type == "text" {
				response.Text += block.Text
			} else if block.Type == "tool_use" {
				response.Tools = append(response.Tools, block)
			}
		}
		response.Stop = stringAt(input, "stop_reason")
		response.Usage = decodeAnthropicUsage(mapAt(input, "usage"))
	default:
		return response, fmt.Errorf("unsupported response protocol")
	}
	return response, nil
}

func encodeResponse(protocol Protocol, response canonicalResponse) map[string]any {
	if response.ID == "" {
		response.ID = randomID("resp", 12)
	}
	switch protocol {
	case ProtocolChat:
		message := map[string]any{"role": "assistant", "content": response.Text}
		if len(response.Tools) > 0 {
			calls := make([]any, 0, len(response.Tools))
			for _, tool := range response.Tools {
				args, _ := json.Marshal(tool.Input)
				calls = append(calls, map[string]any{"id": tool.ID, "type": "function", "function": map[string]any{"name": tool.Name, "arguments": string(args)}})
			}
			message["tool_calls"] = calls
		}
		return map[string]any{"id": asPrefix(response.ID, "chatcmpl"), "object": "chat.completion", "created": response.Created, "model": response.Model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": chatStop(response.Stop)}}, "usage": openAIUsage(response.Usage)}
	case ProtocolResponses:
		output := make([]any, 0, len(response.Tools)+1)
		if response.Text != "" {
			output = append(output, map[string]any{"id": randomID("msg", 12), "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": response.Text, "annotations": []any{}}}})
		}
		for _, tool := range response.Tools {
			args, _ := json.Marshal(tool.Input)
			output = append(output, map[string]any{"id": randomID("fc", 12), "type": "function_call", "status": "completed", "call_id": tool.ID, "name": tool.Name, "arguments": string(args)})
		}
		return map[string]any{"id": asPrefix(response.ID, "resp"), "object": "response", "created_at": response.Created, "status": "completed", "model": response.Model, "output": output, "usage": map[string]any{"input_tokens": response.Usage.Input, "output_tokens": response.Usage.Output, "total_tokens": response.Usage.Total, "input_tokens_details": map[string]any{"cached_tokens": response.Usage.Cached}, "output_tokens_details": map[string]any{"reasoning_tokens": response.Usage.Reasoning}}, "error": nil, "incomplete_details": nil}
	case ProtocolAnthropic:
		content := []any{}
		if response.Text != "" {
			content = append(content, map[string]any{"type": "text", "text": response.Text})
		}
		for _, tool := range response.Tools {
			content = append(content, map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": tool.Input})
		}
		return map[string]any{"id": asPrefix(response.ID, "msg"), "type": "message", "role": "assistant", "model": response.Model, "content": content, "stop_reason": anthropicStop(response.Stop), "stop_sequence": nil, "usage": map[string]any{"input_tokens": response.Usage.Input, "output_tokens": response.Usage.Output, "cache_read_input_tokens": response.Usage.Cached}}
	}
	return map[string]any{}
}

func decodeOpenAIUsage(u map[string]any) canonicalUsage {
	input := intAt(u, "prompt_tokens")
	if input == 0 {
		input = intAt(u, "input_tokens")
	}
	output := intAt(u, "completion_tokens")
	if output == 0 {
		output = intAt(u, "output_tokens")
	}
	total := intAt(u, "total_tokens")
	if total == 0 {
		total = input + output
	}
	cached := intAt(u, "prompt_tokens_details", "cached_tokens")
	if cached == 0 {
		cached = intAt(u, "input_tokens_details", "cached_tokens")
	}
	reasoning := intAt(u, "completion_tokens_details", "reasoning_tokens")
	if reasoning == 0 {
		reasoning = intAt(u, "output_tokens_details", "reasoning_tokens")
	}
	return canonicalUsage{Input: input, Output: output, Total: total, Cached: cached, Reasoning: reasoning}
}
func decodeAnthropicUsage(u map[string]any) canonicalUsage {
	input := intAt(u, "input_tokens")
	output := intAt(u, "output_tokens")
	cached := intAt(u, "cache_read_input_tokens")
	return canonicalUsage{Input: input, Output: output, Total: input + output, Cached: cached}
}
func openAIUsage(u canonicalUsage) map[string]any {
	return map[string]any{"prompt_tokens": u.Input, "completion_tokens": u.Output, "total_tokens": u.Total, "prompt_tokens_details": map[string]any{"cached_tokens": u.Cached}, "completion_tokens_details": map[string]any{"reasoning_tokens": u.Reasoning}}
}

func decodeChatToolChoice(v any) any {
	m, _ := v.(map[string]any)
	if stringAt(m, "type") == "function" {
		return map[string]any{"name": stringAt(m, "function", "name")}
	}
	return v
}
func decodeResponsesToolChoice(v any) any {
	m, _ := v.(map[string]any)
	if stringAt(m, "type") == "function" {
		return map[string]any{"name": stringAt(m, "name")}
	}
	return v
}
func decodeAnthropicToolChoice(v any) any {
	m, _ := v.(map[string]any)
	if stringAt(m, "type") == "tool" {
		return map[string]any{"name": stringAt(m, "name")}
	}
	if s := stringAt(m, "type"); s != "" {
		return s
	}
	return v
}
func encodeChatToolChoice(v any) any {
	if m, ok := v.(map[string]any); ok {
		return map[string]any{"type": "function", "function": map[string]any{"name": stringAt(m, "name")}}
	}
	return v
}
func encodeResponsesToolChoice(v any) any {
	if m, ok := v.(map[string]any); ok {
		return map[string]any{"type": "function", "name": stringAt(m, "name")}
	}
	return v
}
func encodeAnthropicToolChoice(v any) any {
	if m, ok := v.(map[string]any); ok {
		return map[string]any{"type": "tool", "name": stringAt(m, "name")}
	}
	if v == "required" {
		return map[string]any{"type": "any"}
	}
	if v == "none" {
		return map[string]any{"type": "none"}
	}
	if v == "auto" {
		return map[string]any{"type": "auto"}
	}
	return v
}

func normalizeChatRole(role string) string {
	if role == "developer" {
		return "system"
	}
	return role
}
func normalizeResponsesRole(role string) string {
	if role == "tool" {
		return "user"
	}
	return role
}
func normalizeAnthropicRole(role string) string {
	if role == "assistant" {
		return role
	}
	return "user"
}
func chatStop(stop string) string {
	switch stop {
	case "tool_use", "tool_calls":
		return "tool_calls"
	case "max_tokens", "length":
		return "length"
	default:
		return "stop"
	}
}
func anthropicStop(stop string) string {
	switch stop {
	case "tool_use", "tool_calls":
		return "tool_use"
	case "max_tokens", "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}
func schemaOrDefault(v any) any {
	if v == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return v
}
func reasoningEffort(v any) any {
	if m, ok := v.(map[string]any); ok {
		return firstAny(m["effort"], m["type"])
	}
	return v
}
func blocksText(blocks []canonicalBlock) string {
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}
func contentString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	data, _ := json.Marshal(v)
	return string(data)
}
func parseJSONOrString(v string) any {
	if v == "" {
		return map[string]any{}
	}
	var out any
	if json.Unmarshal([]byte(v), &out) == nil {
		return out
	}
	return v
}
func asPrefix(id, prefix string) string {
	if strings.HasPrefix(id, prefix+"_") {
		return id
	}
	return prefix + "_" + strings.TrimPrefix(id, "resp_")
}
func put(m map[string]any, k string, v any) {
	if v != nil {
		m[k] = v
	}
}
func firstAny(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}
func cloneMap(in map[string]any) map[string]any {
	data, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}
func asSlice(v any) []any {
	if values, ok := v.([]any); ok {
		return values
	}
	if v == nil {
		return nil
	}
	return []any{v}
}
func sliceAt(m map[string]any, path ...string) []any {
	v := anyAt(m, path...)
	values, _ := v.([]any)
	return values
}
func mapAt(m map[string]any, path ...string) map[string]any {
	v := anyAt(m, path...)
	value, _ := v.(map[string]any)
	return value
}
func stringAt(m map[string]any, path ...string) string {
	v := anyAt(m, path...)
	value, _ := v.(string)
	return value
}
func boolAt(m map[string]any, path ...string) bool {
	v := anyAt(m, path...)
	value, _ := v.(bool)
	return value
}
func intAt(m map[string]any, path ...string) int {
	v := anyAt(m, path...)
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}
func int64At(m map[string]any, path ...string) int64 { return int64(intAt(m, path...)) }
func anyAt(m map[string]any, path ...string) any {
	var current any = m
	for _, key := range path {
		next, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = next[key]
	}
	return current
}
