package scheduler

import "encoding/json"

func redactResultJSON(data []byte) []byte {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return []byte(`{"status":"INFRA_ERROR","reason":"invalid_result"}`)
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return redact(x)
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
			return x
		case map[string]any:
			for k, v := range x {
				x[k] = walk(v)
			}
			return x
		default:
			return x
		}
	}
	out, err := json.Marshal(walk(value))
	if err != nil {
		return []byte(`{"status":"INFRA_ERROR","reason":"invalid_result"}`)
	}
	return out
}
