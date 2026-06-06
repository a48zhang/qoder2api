package bridge

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
)

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

func normalizeToolDefinitions(raw json.RawMessage) json.RawMessage {
	if rawJSONIsEmpty(raw) {
		return nil
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return raw
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		itemType := stringValue(item["type"])
		switch itemType {
		case "", "function":
			if fn, ok := item["function"].(map[string]any); ok {
				out = append(out, map[string]any{
					"type":     "function",
					"function": fn,
				})
				continue
			}
			if name := stringValue(item["name"]); name != "" {
				out = append(out, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        name,
						"description": stringValue(item["description"]),
						"parameters":  firstNonNil(item["parameters"], item["input_schema"], map[string]any{"type": "object", "properties": map[string]any{}}),
					},
				})
				continue
			}
		}
		out = append(out, item)
	}
	buf, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return buf
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
