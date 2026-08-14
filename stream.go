package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type bridgeStreamEvent struct {
	Kind       string
	ResponseID string
	Model      string
	Text       string
	ToolKey    string
	ToolID     string
	ToolName   string
	Stop       string
	Usage      *bridgeUsage
}

type bridgeStreamParser struct {
	protocol     Protocol
	started      bool
	tools        map[string]bool
	responseArgs map[string]bool
}

func transcodeStream(w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("response writer does not support streaming")
	}
	parser := &bridgeStreamParser{
		protocol:     from,
		tools:        map[string]bool{},
		responseArgs: map[string]bool{},
	}
	emitter := newBridgeStreamEmitter(w, flusher, to, model)
	if err := readSSE(reader, func(eventName, data string) error {
		events, err := parser.Parse(eventName, data)
		if err != nil {
			return err
		}
		for _, event := range events {
			if err := emitter.Emit(event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return emitter.Finish()
}

func readSSE(reader io.Reader, handler func(eventName, data string) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var eventName string
	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}
		err := handler(eventName, strings.Join(dataLines, "\n"))
		eventName = ""
		dataLines = dataLines[:0]
		return err
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

func (parser *bridgeStreamParser) Parse(eventName, data string) ([]bridgeStreamEvent, error) {
	if data == "[DONE]" {
		return []bridgeStreamEvent{{Kind: "done"}}, nil
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		return nil, fmt.Errorf("invalid upstream SSE JSON: %w", err)
	}
	switch parser.protocol {
	case ProtocolChat:
		return parser.parseChat(value), nil
	case ProtocolAnthropic:
		return parser.parseAnthropic(value)
	case ProtocolResponses:
		return parser.parseResponses(eventName, value), nil
	default:
		return nil, fmt.Errorf("unsupported stream protocol %q", parser.protocol)
	}
}

func (parser *bridgeStreamParser) parseChat(value map[string]any) []bridgeStreamEvent {
	events := make([]bridgeStreamEvent, 0, 4)
	if !parser.started {
		if id := stringAt(value, "id"); id != "" {
			parser.started = true
			events = append(events, bridgeStreamEvent{Kind: "start", ResponseID: id, Model: stringAt(value, "model")})
		}
	}
	if usageMap := mapAt(value, "usage"); len(usageMap) > 0 {
		usage := decodeOpenAIUsage(usageMap)
		events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
	}
	for _, raw := range sliceAt(value, "choices") {
		choice, _ := raw.(map[string]any)
		delta := mapAt(choice, "delta")
		if text := stringAt(delta, "content"); text != "" {
			events = append(events, bridgeStreamEvent{Kind: "text", Text: text})
		}
		for _, rawCall := range sliceAt(delta, "tool_calls") {
			call, _ := rawCall.(map[string]any)
			key := fmt.Sprint(firstAny(call["index"], stringAt(call, "id")))
			function := mapAt(call, "function")
			id := stringAt(call, "id")
			name := stringAt(function, "name")
			if !parser.tools[key] {
				parser.tools[key] = true
				events = append(events, bridgeStreamEvent{Kind: "tool_start", ToolKey: key, ToolID: id, ToolName: name})
			}
			if arguments := stringAt(function, "arguments"); arguments != "" {
				events = append(events, bridgeStreamEvent{Kind: "tool_delta", ToolKey: key, ToolID: id, ToolName: name, Text: arguments})
			}
		}
		if stop := stringAt(choice, "finish_reason"); stop != "" {
			events = append(events, bridgeStreamEvent{Kind: "finish", Stop: stop})
		}
	}
	return events
}

func (parser *bridgeStreamParser) parseAnthropic(value map[string]any) ([]bridgeStreamEvent, error) {
	switch stringAt(value, "type") {
	case "message_start":
		message := mapAt(value, "message")
		events := []bridgeStreamEvent{{Kind: "start", ResponseID: stringAt(message, "id"), Model: stringAt(message, "model")}}
		if usageMap := mapAt(message, "usage"); len(usageMap) > 0 {
			usage := decodeAnthropicUsage(usageMap)
			events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
		}
		return events, nil
	case "content_block_start":
		block := mapAt(value, "content_block")
		key := fmt.Sprint(value["index"])
		switch stringAt(block, "type") {
		case "tool_use":
			parser.tools[key] = true
			return []bridgeStreamEvent{{Kind: "tool_start", ToolKey: key, ToolID: stringAt(block, "id"), ToolName: stringAt(block, "name")}}, nil
		case "text":
			if text := stringAt(block, "text"); text != "" {
				return []bridgeStreamEvent{{Kind: "text", Text: text}}, nil
			}
		}
	case "content_block_delta":
		delta := mapAt(value, "delta")
		key := fmt.Sprint(value["index"])
		switch stringAt(delta, "type") {
		case "text_delta":
			return []bridgeStreamEvent{{Kind: "text", Text: stringAt(delta, "text")}}, nil
		case "input_json_delta":
			return []bridgeStreamEvent{{Kind: "tool_delta", ToolKey: key, Text: stringAt(delta, "partial_json")}}, nil
		}
	case "message_delta":
		events := make([]bridgeStreamEvent, 0, 2)
		if usageMap := mapAt(value, "usage"); len(usageMap) > 0 {
			usage := decodeAnthropicUsage(usageMap)
			events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
		}
		if stop := stringAt(value, "delta", "stop_reason"); stop != "" {
			events = append(events, bridgeStreamEvent{Kind: "finish", Stop: stop})
		}
		return events, nil
	case "message_stop":
		return []bridgeStreamEvent{{Kind: "done"}}, nil
	case "error":
		message := firstString(stringAt(value, "error", "message"), "upstream Anthropic stream error")
		return nil, fmt.Errorf("%s", message)
	}
	return nil, nil
}

func (parser *bridgeStreamParser) parseResponses(eventName string, value map[string]any) []bridgeStreamEvent {
	typeName := stringAt(value, "type")
	if typeName == "" {
		typeName = eventName
	}
	switch typeName {
	case "response.created":
		response := mapAt(value, "response")
		return []bridgeStreamEvent{{Kind: "start", ResponseID: stringAt(response, "id"), Model: stringAt(response, "model")}}
	case "response.output_text.delta":
		return []bridgeStreamEvent{{Kind: "text", Text: stringAt(value, "delta")}}
	case "response.output_item.added":
		item := mapAt(value, "item")
		if stringAt(item, "type") == "function_call" {
			key := responseToolKey(value, item)
			parser.tools[key] = true
			return []bridgeStreamEvent{{Kind: "tool_start", ToolKey: key, ToolID: stringAt(item, "call_id"), ToolName: stringAt(item, "name")}}
		}
	case "response.function_call_arguments.delta":
		key := responseToolKey(value, nil)
		parser.responseArgs[key] = true
		return []bridgeStreamEvent{{Kind: "tool_delta", ToolKey: key, Text: stringAt(value, "delta")}}
	case "response.output_item.done":
		item := mapAt(value, "item")
		if stringAt(item, "type") == "function_call" {
			key := responseToolKey(value, item)
			events := make([]bridgeStreamEvent, 0, 2)
			if !parser.tools[key] {
				parser.tools[key] = true
				events = append(events, bridgeStreamEvent{Kind: "tool_start", ToolKey: key, ToolID: stringAt(item, "call_id"), ToolName: stringAt(item, "name")})
			}
			if !parser.responseArgs[key] {
				if arguments := stringAt(item, "arguments"); arguments != "" {
					events = append(events, bridgeStreamEvent{Kind: "tool_delta", ToolKey: key, Text: arguments})
				}
			}
			return events
		}
	case "response.completed", "response.incomplete", "response.failed":
		response := mapAt(value, "response")
		usage := decodeOpenAIUsage(mapAt(response, "usage"))
		stop := "stop"
		if typeName == "response.incomplete" {
			stop = "length"
		} else if typeName == "response.failed" {
			stop = "error"
		}
		return []bridgeStreamEvent{{Kind: "usage", Usage: &usage}, {Kind: "finish", Stop: stop}, {Kind: "done"}}
	}
	return nil
}

func responseToolKey(event map[string]any, item map[string]any) string {
	if item != nil {
		if id := stringAt(item, "id"); id != "" {
			return id
		}
	}
	if id := stringAt(event, "item_id"); id != "" {
		return id
	}
	return fmt.Sprint(event["output_index"])
}

type bridgeStreamTool struct {
	Key       string
	ID        string
	ItemID    string
	Name      string
	Arguments strings.Builder
	Index     int
	Started   bool
}

type bridgeStreamEmitter struct {
	w       io.Writer
	flush   http.Flusher
	target  Protocol
	model   string
	id      string
	created int64
	started bool
	done    bool
	stop    string
	usage   bridgeUsage
	text    strings.Builder

	tools map[string]*bridgeStreamTool
	order []string

	sequence       int
	nextOutput     int
	textOpen       bool
	textItemID     string
	textOutput     int
	responseOutput []any
}

func newBridgeStreamEmitter(writer io.Writer, flusher http.Flusher, target Protocol, model string) *bridgeStreamEmitter {
	return &bridgeStreamEmitter{
		w:       writer,
		flush:   flusher,
		target:  target,
		model:   model,
		created: time.Now().Unix(),
		tools:   map[string]*bridgeStreamTool{},
	}
}

func (emitter *bridgeStreamEmitter) Emit(event bridgeStreamEvent) error {
	if event.ResponseID != "" && emitter.id == "" {
		emitter.id = event.ResponseID
	}
	if event.Model != "" {
		emitter.model = event.Model
	}
	if !emitter.started && (event.Kind == "start" || event.Kind == "text" || event.Kind == "tool_start" || event.Kind == "tool_delta") {
		if err := emitter.start(); err != nil {
			return err
		}
	}
	switch event.Kind {
	case "text":
		if event.Text == "" {
			return nil
		}
		emitter.text.WriteString(event.Text)
		return emitter.emitText(event.Text)
	case "tool_start":
		tool := emitter.tool(event.ToolKey)
		if event.ToolID != "" {
			tool.ID = event.ToolID
		}
		if event.ToolName != "" {
			tool.Name = event.ToolName
		}
		return emitter.startTool(tool)
	case "tool_delta":
		tool := emitter.tool(event.ToolKey)
		if event.ToolID != "" {
			tool.ID = event.ToolID
		}
		if event.ToolName != "" {
			tool.Name = event.ToolName
		}
		if err := emitter.startTool(tool); err != nil {
			return err
		}
		tool.Arguments.WriteString(event.Text)
		return emitter.emitToolDelta(tool, event.Text)
	case "usage":
		if event.Usage != nil {
			mergeBridgeUsage(&emitter.usage, *event.Usage)
		}
	case "finish":
		emitter.stop = event.Stop
	case "done":
		return emitter.Finish()
	}
	return nil
}

func (emitter *bridgeStreamEmitter) tool(key string) *bridgeStreamTool {
	if key == "" {
		key = fmt.Sprintf("tool-%d", len(emitter.order))
	}
	if current := emitter.tools[key]; current != nil {
		return current
	}
	tool := &bridgeStreamTool{
		Key:    key,
		ID:     randomID("call", 12),
		ItemID: randomID("fc", 12),
		Index:  len(emitter.order),
	}
	emitter.tools[key] = tool
	emitter.order = append(emitter.order, key)
	return tool
}

func (emitter *bridgeStreamEmitter) start() error {
	if emitter.started {
		return nil
	}
	emitter.started = true
	if emitter.id == "" {
		emitter.id = randomID("resp", 12)
	}
	switch emitter.target {
	case ProtocolChat:
		return emitter.sse("", map[string]any{
			"id":      asPrefix(emitter.id, "chatcmpl"),
			"object":  "chat.completion.chunk",
			"created": emitter.created,
			"model":   emitter.model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"role": "assistant", "content": ""},
				"finish_reason": nil,
			}},
		})
	case ProtocolAnthropic:
		return emitter.sse("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            asPrefix(emitter.id, "msg"),
				"type":          "message",
				"role":          "assistant",
				"model":         emitter.model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
	case ProtocolResponses:
		response := emitter.responsesBase("in_progress", []any{})
		if err := emitter.sse("response.created", map[string]any{"type": "response.created", "response": response, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
		return emitter.sse("response.in_progress", map[string]any{"type": "response.in_progress", "response": response, "sequence_number": emitter.nextSequence()})
	default:
		return fmt.Errorf("unsupported stream target %q", emitter.target)
	}
}

func (emitter *bridgeStreamEmitter) emitText(delta string) error {
	switch emitter.target {
	case ProtocolChat:
		return emitter.chatChunk(map[string]any{"content": delta}, nil)
	case ProtocolAnthropic:
		if !emitter.textOpen {
			emitter.textOpen = true
			if err := emitter.sse("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         0,
				"content_block": map[string]any{"type": "text", "text": ""},
			}); err != nil {
				return err
			}
		}
		return emitter.sse("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": delta},
		})
	case ProtocolResponses:
		if !emitter.textOpen {
			emitter.textOpen = true
			emitter.textItemID = randomID("msg", 12)
			emitter.textOutput = emitter.nextOutput
			emitter.nextOutput++
			item := map[string]any{"id": emitter.textItemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}
			if err := emitter.sse("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": emitter.textOutput, "item": item, "sequence_number": emitter.nextSequence()}); err != nil {
				return err
			}
			part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
			if err := emitter.sse("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": emitter.textItemID, "output_index": emitter.textOutput, "content_index": 0, "part": part, "sequence_number": emitter.nextSequence()}); err != nil {
				return err
			}
		}
		return emitter.sse("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": delta, "item_id": emitter.textItemID, "output_index": emitter.textOutput, "content_index": 0, "sequence_number": emitter.nextSequence()})
	}
	return nil
}

func (emitter *bridgeStreamEmitter) startTool(tool *bridgeStreamTool) error {
	if tool.Started || emitter.target == ProtocolAnthropic {
		return nil
	}
	tool.Started = true
	switch emitter.target {
	case ProtocolChat:
		return emitter.chatChunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": tool.Index,
			"id":    tool.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      tool.Name,
				"arguments": "",
			},
		}}}, nil)
	case ProtocolResponses:
		tool.Index = emitter.nextOutput
		emitter.nextOutput++
		item := map[string]any{"id": tool.ItemID, "type": "function_call", "status": "in_progress", "arguments": "", "call_id": tool.ID, "name": tool.Name}
		return emitter.sse("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": tool.Index, "item": item, "sequence_number": emitter.nextSequence()})
	}
	return nil
}

func (emitter *bridgeStreamEmitter) emitToolDelta(tool *bridgeStreamTool, delta string) error {
	if delta == "" || emitter.target == ProtocolAnthropic {
		return nil
	}
	switch emitter.target {
	case ProtocolChat:
		return emitter.chatChunk(map[string]any{"tool_calls": []any{map[string]any{
			"index":    tool.Index,
			"function": map[string]any{"arguments": delta},
		}}}, nil)
	case ProtocolResponses:
		return emitter.sse("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "delta": delta, "item_id": tool.ItemID, "output_index": tool.Index, "sequence_number": emitter.nextSequence()})
	}
	return nil
}

func (emitter *bridgeStreamEmitter) Finish() error {
	if emitter.done {
		return nil
	}
	emitter.done = true
	if !emitter.started {
		if err := emitter.start(); err != nil {
			return err
		}
	}
	if emitter.stop == "" {
		if len(emitter.order) > 0 {
			emitter.stop = "tool_calls"
		} else {
			emitter.stop = "stop"
		}
	}
	switch emitter.target {
	case ProtocolChat:
		if err := emitter.chatChunk(map[string]any{}, chatStop(emitter.stop)); err != nil {
			return err
		}
		if err := emitter.sse("", map[string]any{
			"id":      asPrefix(emitter.id, "chatcmpl"),
			"object":  "chat.completion.chunk",
			"created": emitter.created,
			"model":   emitter.model,
			"choices": []any{},
			"usage":   openAIUsage(emitter.usage),
		}); err != nil {
			return err
		}
		return emitter.rawSSE("", "[DONE]")

	case ProtocolAnthropic:
		index := 0
		if emitter.textOpen {
			if err := emitter.sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
				return err
			}
			index++
		}
		for _, key := range emitter.order {
			tool := emitter.tools[key]
			if err := emitter.sse("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         index,
				"content_block": map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": map[string]any{}},
			}); err != nil {
				return err
			}
			if arguments := tool.Arguments.String(); arguments != "" {
				if err := emitter.sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": arguments}}); err != nil {
					return err
				}
			}
			if err := emitter.sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
				return err
			}
			index++
		}
		if err := emitter.sse("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": anthropicStop(emitter.stop), "stop_sequence": nil},
			"usage": map[string]any{"input_tokens": emitter.usage.Input, "output_tokens": emitter.usage.Output},
		}); err != nil {
			return err
		}
		return emitter.sse("message_stop", map[string]any{"type": "message_stop"})

	case ProtocolResponses:
		if err := emitter.finishResponsesItems(); err != nil {
			return err
		}
		response := bridgeResponse{
			ID:      emitter.id,
			Model:   emitter.model,
			Text:    emitter.text.String(),
			Stop:    emitter.stop,
			Usage:   emitter.usage,
			Created: emitter.created,
		}
		for _, key := range emitter.order {
			tool := emitter.tools[key]
			response.Tools = append(response.Tools, bridgeBlock{Kind: "tool_call", ID: tool.ID, Name: tool.Name, ArgumentsJSON: tool.Arguments.String()})
		}
		completed := encodeBridgeResponse(ProtocolResponses, response)
		return emitter.sse("response.completed", map[string]any{"type": "response.completed", "response": completed, "sequence_number": emitter.nextSequence()})
	}
	return nil
}

func (emitter *bridgeStreamEmitter) finishResponsesItems() error {
	if emitter.textOpen {
		text := emitter.text.String()
		if err := emitter.sse("response.output_text.done", map[string]any{"type": "response.output_text.done", "text": text, "item_id": emitter.textItemID, "output_index": emitter.textOutput, "content_index": 0, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		if err := emitter.sse("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": emitter.textItemID, "output_index": emitter.textOutput, "content_index": 0, "part": part, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
		item := map[string]any{"id": emitter.textItemID, "type": "message", "status": "completed", "role": "assistant", "content": []any{part}}
		if err := emitter.sse("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": emitter.textOutput, "item": item, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
	}
	for _, key := range emitter.order {
		tool := emitter.tools[key]
		arguments := tool.Arguments.String()
		if err := emitter.sse("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "arguments": arguments, "item_id": tool.ItemID, "output_index": tool.Index, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
		item := map[string]any{"id": tool.ItemID, "type": "function_call", "status": "completed", "arguments": arguments, "call_id": tool.ID, "name": tool.Name}
		if err := emitter.sse("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": tool.Index, "item": item, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
	}
	return nil
}

func (emitter *bridgeStreamEmitter) chatChunk(delta map[string]any, finish any) error {
	return emitter.sse("", map[string]any{
		"id":      asPrefix(emitter.id, "chatcmpl"),
		"object":  "chat.completion.chunk",
		"created": emitter.created,
		"model":   emitter.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	})
}

func (emitter *bridgeStreamEmitter) responsesBase(status string, output []any) map[string]any {
	return map[string]any{
		"id":         asPrefix(emitter.id, "resp"),
		"object":     "response",
		"created_at": emitter.created,
		"status":     status,
		"model":      emitter.model,
		"output":     output,
		"error":      nil,
	}
}

func (emitter *bridgeStreamEmitter) nextSequence() int {
	sequence := emitter.sequence
	emitter.sequence++
	return sequence
}

func (emitter *bridgeStreamEmitter) sse(eventName string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return emitter.rawSSE(eventName, string(data))
}

func (emitter *bridgeStreamEmitter) rawSSE(eventName, data string) error {
	if eventName != "" {
		if _, err := fmt.Fprintf(emitter.w, "event: %s\n", eventName); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(emitter.w, "data: %s\n\n", data); err != nil {
		return err
	}
	emitter.flush.Flush()
	return nil
}

func mergeBridgeUsage(destination *bridgeUsage, source bridgeUsage) {
	if source.Input != 0 {
		destination.Input = source.Input
	}
	if source.Output != 0 {
		destination.Output = source.Output
	}
	if source.Total != 0 {
		destination.Total = source.Total
	}
	if source.Cached != 0 {
		destination.Cached = source.Cached
	}
	if source.Reasoning != 0 {
		destination.Reasoning = source.Reasoning
	}
	if destination.Total == 0 {
		destination.Total = destination.Input + destination.Output
	}
}
