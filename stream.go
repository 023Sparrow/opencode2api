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

type streamEvent struct {
	Kind   string
	ID     string
	Model  string
	Text   string
	Key    string
	ToolID string
	Name   string
	Stop   string
	Usage  *canonicalUsage
}

type streamParser struct {
	protocol Protocol
	toolSeen map[string]bool
}

func transcodeStream(w http.ResponseWriter, r io.Reader, from, to Protocol, model string) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("response writer does not support streaming")
	}
	parser := &streamParser{protocol: from, toolSeen: map[string]bool{}}
	emitter := newStreamEmitter(w, flusher, to, model)
	err := readSSE(r, func(eventName, data string) error {
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
	})
	if err != nil {
		return err
	}
	return emitter.Finish()
}

func readSSE(r io.Reader, fn func(eventName, data string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var eventName string
	var data []string
	flush := func() error {
		if len(data) == 0 {
			eventName = ""
			return nil
		}
		err := fn(eventName, strings.Join(data, "\n"))
		eventName = ""
		data = data[:0]
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
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

func (p *streamParser) Parse(eventName, data string) ([]streamEvent, error) {
	if data == "[DONE]" {
		return []streamEvent{{Kind: "done"}}, nil
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		return nil, fmt.Errorf("invalid upstream SSE JSON: %w", err)
	}
	switch p.protocol {
	case ProtocolChat:
		return p.parseChat(value), nil
	case ProtocolAnthropic:
		return p.parseAnthropic(value), nil
	case ProtocolResponses:
		return p.parseResponses(eventName, value), nil
	default:
		return nil, fmt.Errorf("unsupported stream protocol %q", p.protocol)
	}
}

func (p *streamParser) parseChat(v map[string]any) []streamEvent {
	var events []streamEvent
	if id := stringAt(v, "id"); id != "" {
		events = append(events, streamEvent{Kind: "start", ID: id, Model: stringAt(v, "model")})
	}
	if usageMap := mapAt(v, "usage"); len(usageMap) > 0 {
		usage := decodeOpenAIUsage(usageMap)
		events = append(events, streamEvent{Kind: "usage", Usage: &usage})
	}
	for _, raw := range sliceAt(v, "choices") {
		choice, _ := raw.(map[string]any)
		delta := mapAt(choice, "delta")
		if text := stringAt(delta, "content"); text != "" {
			events = append(events, streamEvent{Kind: "text", Text: text})
		}
		for _, rawCall := range sliceAt(delta, "tool_calls") {
			call, _ := rawCall.(map[string]any)
			key := fmt.Sprint(firstAny(call["index"], stringAt(call, "id")))
			fn := mapAt(call, "function")
			if !p.toolSeen[key] {
				p.toolSeen[key] = true
				events = append(events, streamEvent{Kind: "tool_start", Key: key, ToolID: stringAt(call, "id"), Name: stringAt(fn, "name")})
			}
			if args := stringAt(fn, "arguments"); args != "" {
				events = append(events, streamEvent{Kind: "tool_delta", Key: key, Text: args})
			}
		}
		if stop := stringAt(choice, "finish_reason"); stop != "" {
			events = append(events, streamEvent{Kind: "finish", Stop: stop})
		}
	}
	return events
}

func (p *streamParser) parseAnthropic(v map[string]any) []streamEvent {
	typeName := stringAt(v, "type")
	switch typeName {
	case "message_start":
		message := mapAt(v, "message")
		events := []streamEvent{{Kind: "start", ID: stringAt(message, "id"), Model: stringAt(message, "model")}}
		if usageMap := mapAt(message, "usage"); len(usageMap) > 0 {
			usage := decodeAnthropicUsage(usageMap)
			events = append(events, streamEvent{Kind: "usage", Usage: &usage})
		}
		return events
	case "content_block_start":
		block := mapAt(v, "content_block")
		key := fmt.Sprint(v["index"])
		if stringAt(block, "type") == "tool_use" {
			return []streamEvent{{Kind: "tool_start", Key: key, ToolID: stringAt(block, "id"), Name: stringAt(block, "name")}}
		}
		return nil
	case "content_block_delta":
		delta := mapAt(v, "delta")
		key := fmt.Sprint(v["index"])
		if stringAt(delta, "type") == "input_json_delta" {
			return []streamEvent{{Kind: "tool_delta", Key: key, Text: stringAt(delta, "partial_json")}}
		}
		if stringAt(delta, "type") == "text_delta" {
			return []streamEvent{{Kind: "text", Text: stringAt(delta, "text")}}
		}
	case "message_delta":
		var events []streamEvent
		if usageMap := mapAt(v, "usage"); len(usageMap) > 0 {
			usage := decodeAnthropicUsage(usageMap)
			events = append(events, streamEvent{Kind: "usage", Usage: &usage})
		}
		if stop := stringAt(v, "delta", "stop_reason"); stop != "" {
			events = append(events, streamEvent{Kind: "finish", Stop: stop})
		}
		return events
	case "message_stop":
		return []streamEvent{{Kind: "done"}}
	case "error":
		return []streamEvent{{Kind: "finish", Stop: "error"}, {Kind: "done"}}
	}
	return nil
}

func (p *streamParser) parseResponses(eventName string, v map[string]any) []streamEvent {
	typeName := stringAt(v, "type")
	if typeName == "" {
		typeName = eventName
	}
	switch typeName {
	case "response.created":
		response := mapAt(v, "response")
		return []streamEvent{{Kind: "start", ID: stringAt(response, "id"), Model: stringAt(response, "model")}}
	case "response.output_text.delta":
		return []streamEvent{{Kind: "text", Text: stringAt(v, "delta")}}
	case "response.output_item.added":
		item := mapAt(v, "item")
		if stringAt(item, "type") == "function_call" {
			return []streamEvent{{Kind: "tool_start", Key: fmt.Sprint(v["output_index"]), ToolID: stringAt(item, "call_id"), Name: stringAt(item, "name")}}
		}
	case "response.function_call_arguments.delta":
		return []streamEvent{{Kind: "tool_delta", Key: fmt.Sprint(v["output_index"]), Text: stringAt(v, "delta")}}
	case "response.completed", "response.incomplete", "response.failed":
		response := mapAt(v, "response")
		usage := decodeOpenAIUsage(mapAt(response, "usage"))
		stop := "stop"
		if typeName == "response.incomplete" {
			stop = "length"
		}
		if typeName == "response.failed" {
			stop = "error"
		}
		return []streamEvent{{Kind: "usage", Usage: &usage}, {Kind: "finish", Stop: stop}, {Kind: "done"}}
	}
	return nil
}

type bufferedTool struct {
	ID        string
	ItemID    string
	Name      string
	Arguments strings.Builder
}

type streamEmitter struct {
	w        io.Writer
	flush    http.Flusher
	target   Protocol
	response canonicalResponse
	started  bool
	finished bool
	textOpen bool
	textItem string
	sequence int
	tools    map[string]*bufferedTool
	order    []string
}

func newStreamEmitter(w io.Writer, flush http.Flusher, target Protocol, model string) *streamEmitter {
	return &streamEmitter{w: w, flush: flush, target: target, response: canonicalResponse{Model: model, Created: time.Now().Unix()}, tools: map[string]*bufferedTool{}}
}

func (e *streamEmitter) Emit(event streamEvent) error {
	if event.ID != "" && e.response.ID == "" {
		e.response.ID = event.ID
	}
	if event.Model != "" {
		e.response.Model = event.Model
	}
	if !e.started && (event.Kind == "start" || event.Kind == "text" || event.Kind == "tool_start") {
		if err := e.start(); err != nil {
			return err
		}
	}
	switch event.Kind {
	case "text":
		if event.Text == "" {
			return nil
		}
		e.response.Text += event.Text
		return e.emitText(event.Text)
	case "tool_start":
		if _, exists := e.tools[event.Key]; !exists {
			id := event.ToolID
			if id == "" {
				id = randomID("call", 12)
			}
			e.tools[event.Key] = &bufferedTool{ID: id, ItemID: randomID("fc", 12), Name: event.Name}
			e.order = append(e.order, event.Key)
		}
	case "tool_delta":
		tool := e.tools[event.Key]
		if tool == nil {
			tool = &bufferedTool{ID: randomID("call", 12), ItemID: randomID("fc", 12)}
			e.tools[event.Key] = tool
			e.order = append(e.order, event.Key)
		}
		tool.Arguments.WriteString(event.Text)
	case "usage":
		if event.Usage != nil {
			mergeUsage(&e.response.Usage, *event.Usage)
		}
	case "finish":
		e.response.Stop = event.Stop
	case "done":
		return e.Finish()
	}
	return nil
}

func (e *streamEmitter) start() error {
	if e.started {
		return nil
	}
	e.started = true
	if e.response.ID == "" {
		e.response.ID = randomID("resp", 12)
	}
	switch e.target {
	case ProtocolChat:
		return e.sse("", map[string]any{"id": asPrefix(e.response.ID, "chatcmpl"), "object": "chat.completion.chunk", "created": e.response.Created, "model": e.response.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}}})
	case ProtocolAnthropic:
		return e.sse("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": asPrefix(e.response.ID, "msg"), "type": "message", "role": "assistant", "model": e.response.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
	case ProtocolResponses:
		base := map[string]any{"id": asPrefix(e.response.ID, "resp"), "object": "response", "created_at": e.response.Created, "status": "in_progress", "model": e.response.Model, "output": []any{}, "error": nil}
		if err := e.sse("response.created", map[string]any{"type": "response.created", "response": base, "sequence_number": e.nextSequence()}); err != nil {
			return err
		}
		return e.sse("response.in_progress", map[string]any{"type": "response.in_progress", "response": base, "sequence_number": e.nextSequence()})
	}
	return nil
}

func (e *streamEmitter) emitText(delta string) error {
	switch e.target {
	case ProtocolChat:
		return e.sse("", map[string]any{"id": asPrefix(e.response.ID, "chatcmpl"), "object": "chat.completion.chunk", "created": e.response.Created, "model": e.response.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": nil}}})
	case ProtocolAnthropic:
		if !e.textOpen {
			e.textOpen = true
			if err := e.sse("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}); err != nil {
				return err
			}
		}
		return e.sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": delta}})
	case ProtocolResponses:
		if !e.textOpen {
			e.textOpen = true
			e.textItem = randomID("msg", 12)
			item := map[string]any{"id": e.textItem, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}
			if err := e.sse("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item, "sequence_number": e.nextSequence()}); err != nil {
				return err
			}
			if err := e.sse("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": item["id"], "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}, "sequence_number": e.nextSequence()}); err != nil {
				return err
			}
		}
		return e.sse("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": delta, "item_id": e.textItem, "output_index": 0, "content_index": 0, "sequence_number": e.nextSequence()})
	}
	return nil
}

func (e *streamEmitter) Finish() error {
	if e.finished {
		return nil
	}
	e.finished = true
	if !e.started {
		if err := e.start(); err != nil {
			return err
		}
	}
	for _, key := range e.order {
		tool := e.tools[key]
		if tool == nil {
			continue
		}
		input := parseJSONOrString(tool.Arguments.String())
		e.response.Tools = append(e.response.Tools, canonicalBlock{Type: "tool_use", ID: tool.ID, Name: tool.Name, Input: input})
	}
	if e.response.Stop == "" {
		e.response.Stop = "stop"
	}
	switch e.target {
	case ProtocolChat:
		if len(e.response.Tools) > 0 {
			calls := make([]any, 0, len(e.response.Tools))
			for i, tool := range e.response.Tools {
				args, _ := json.Marshal(tool.Input)
				calls = append(calls, map[string]any{"index": i, "id": tool.ID, "type": "function", "function": map[string]any{"name": tool.Name, "arguments": string(args)}})
			}
			if err := e.sse("", map[string]any{"id": asPrefix(e.response.ID, "chatcmpl"), "object": "chat.completion.chunk", "created": e.response.Created, "model": e.response.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": calls}, "finish_reason": nil}}}); err != nil {
				return err
			}
		}
		if err := e.sse("", map[string]any{"id": asPrefix(e.response.ID, "chatcmpl"), "object": "chat.completion.chunk", "created": e.response.Created, "model": e.response.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": chatStop(e.response.Stop)}}}); err != nil {
			return err
		}
		if err := e.sse("", map[string]any{"id": asPrefix(e.response.ID, "chatcmpl"), "object": "chat.completion.chunk", "created": e.response.Created, "model": e.response.Model, "choices": []any{}, "usage": openAIUsage(e.response.Usage)}); err != nil {
			return err
		}
		return e.rawSSE("", "[DONE]")
	case ProtocolAnthropic:
		index := 0
		if e.textOpen {
			if err := e.sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
				return err
			}
			index++
		}
		for _, tool := range e.response.Tools {
			if err := e.sse("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": map[string]any{}}}); err != nil {
				return err
			}
			args, _ := json.Marshal(tool.Input)
			if err := e.sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(args)}}); err != nil {
				return err
			}
			if err := e.sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
				return err
			}
			index++
		}
		if err := e.sse("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": anthropicStop(e.response.Stop), "stop_sequence": nil}, "usage": map[string]any{"input_tokens": e.response.Usage.Input, "output_tokens": e.response.Usage.Output}}); err != nil {
			return err
		}
		return e.sse("message_stop", map[string]any{"type": "message_stop"})
	case ProtocolResponses:
		if e.textOpen {
			if err := e.sse("response.output_text.done", map[string]any{"type": "response.output_text.done", "text": e.response.Text, "item_id": e.textItem, "output_index": 0, "content_index": 0, "sequence_number": e.nextSequence()}); err != nil {
				return err
			}
			part := map[string]any{"type": "output_text", "text": e.response.Text, "annotations": []any{}}
			if err := e.sse("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": e.textItem, "output_index": 0, "content_index": 0, "part": part, "sequence_number": e.nextSequence()}); err != nil {
				return err
			}
			item := map[string]any{"id": e.textItem, "type": "message", "status": "completed", "role": "assistant", "content": []any{part}}
			if err := e.sse("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item, "sequence_number": e.nextSequence()}); err != nil {
				return err
			}
		}
		baseIndex := 0
		if e.textOpen {
			baseIndex = 1
		}
		for i, key := range e.order {
			tool := e.tools[key]
			if tool == nil {
				continue
			}
			index := baseIndex + i
			item := map[string]any{"id": tool.ItemID, "type": "function_call", "status": "in_progress", "arguments": "", "call_id": tool.ID, "name": tool.Name}
			if err := e.sse("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": index, "item": item, "sequence_number": e.nextSequence()}); err != nil {
				return err
			}
			args := tool.Arguments.String()
			if err := e.sse("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "delta": args, "item_id": tool.ItemID, "output_index": index, "sequence_number": e.nextSequence()}); err != nil {
				return err
			}
			if err := e.sse("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "arguments": args, "item_id": tool.ItemID, "output_index": index, "sequence_number": e.nextSequence()}); err != nil {
				return err
			}
			item["status"] = "completed"
			item["arguments"] = args
			if err := e.sse("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": index, "item": item, "sequence_number": e.nextSequence()}); err != nil {
				return err
			}
		}
		output := encodeResponse(ProtocolResponses, e.response)["output"]
		response := encodeResponse(ProtocolResponses, e.response)
		response["output"] = output
		return e.sse("response.completed", map[string]any{"type": "response.completed", "response": response, "sequence_number": e.nextSequence()})
	}
	return nil
}

func (e *streamEmitter) nextSequence() int {
	value := e.sequence
	e.sequence++
	return value
}

func (e *streamEmitter) sse(event string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return e.rawSSE(event, string(data))
}

func (e *streamEmitter) rawSSE(event, data string) error {
	if event != "" {
		if _, err := fmt.Fprintf(e.w, "event: %s\n", event); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(e.w, "data: %s\n\n", data); err != nil {
		return err
	}
	e.flush.Flush()
	return nil
}

func mergeUsage(dst *canonicalUsage, src canonicalUsage) {
	if src.Input != 0 {
		dst.Input = src.Input
	}
	if src.Output != 0 {
		dst.Output = src.Output
	}
	if src.Total != 0 {
		dst.Total = src.Total
	}
	if src.Cached != 0 {
		dst.Cached = src.Cached
	}
	if src.Reasoning != 0 {
		dst.Reasoning = src.Reasoning
	}
	if dst.Total == 0 {
		dst.Total = dst.Input + dst.Output
	}
}
