package qoder

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

type ToolCallDelta struct {
	Index             int
	ID                string
	Type              string
	FunctionName      string
	ArgumentsFragment string
}

type ToolCall struct {
	Index     int
	ID        string
	Type      string
	Name      string
	Arguments string
}

type ToolCallAccumulator struct {
	calls []ToolCall
}

func stringValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	default:
		return ""
	}
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func randomHex(length int) string {
	if length <= 0 {
		return ""
	}
	max := new(big.Int).Lsh(big.NewInt(1), uint(length*4))
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return fmt.Sprintf("%0*x", length, 0)
	}
	return fmt.Sprintf("%0*x", length, n)
}

func rawJSONIsEmpty(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null" || trimmed == "[]"
}

func ParseToolCallDeltas(raw json.RawMessage) []ToolCallDelta {
	if rawJSONIsEmpty(raw) {
		return nil
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	out := make([]ToolCallDelta, 0, len(decoded))
	for i, item := range decoded {
		fn, _ := item["function"].(map[string]any)
		out = append(out, ToolCallDelta{
			Index:             intValue(item["index"], i),
			ID:                stringValue(item["id"]),
			Type:              stringValue(item["type"]),
			FunctionName:      stringValue(fn["name"]),
			ArgumentsFragment: stringValue(fn["arguments"]),
		})
	}
	return out
}

func (a *ToolCallAccumulator) AddRaw(raw json.RawMessage) {
	for _, delta := range ParseToolCallDeltas(raw) {
		a.AddDelta(delta)
	}
}

func (a *ToolCallAccumulator) AddDelta(delta ToolCallDelta) {
	if delta.Index < 0 {
		delta.Index = 0
	}
	for len(a.calls) <= delta.Index {
		a.calls = append(a.calls, ToolCall{
			Index: len(a.calls),
			Type:  "function",
		})
	}
	call := &a.calls[delta.Index]
	call.Index = delta.Index
	if delta.ID != "" {
		call.ID = delta.ID
	}
	if delta.Type != "" {
		call.Type = delta.Type
	}
	if delta.FunctionName != "" {
		call.Name = delta.FunctionName
	}
	if delta.ArgumentsFragment != "" {
		call.Arguments += delta.ArgumentsFragment
	}
}

func (a *ToolCallAccumulator) Calls() []ToolCall {
	if len(a.calls) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(a.calls))
	for _, call := range a.calls {
		if strings.TrimSpace(call.ID) == "" && strings.TrimSpace(call.Name) == "" && strings.TrimSpace(call.Arguments) == "" {
			continue
		}
		if call.Type == "" {
			call.Type = "function"
		}
		out = append(out, call)
	}
	return out
}

func (a *ToolCallAccumulator) RawJSON() json.RawMessage {
	calls := a.Calls()
	if len(calls) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		out = append(out, map[string]any{
			"id":    call.ID,
			"index": call.Index,
			"type":  firstNonBlank(call.Type, "function"),
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	buf, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return buf
}

func intValue(v any, fallback int) int {
	switch x := v.(type) {
	case int:
		return x
	case int32:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n
		}
	}
	return fallback
}
