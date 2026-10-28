package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// checkArguments holds a tool's arguments to its inputSchema as strictly as a money route holds its body (B50.4): a
// model fills them, so they are untrusted. They must be one JSON object naming no property the schema does not, each
// of the type it names, every required one present, an enum's value one of its values, within its bounds. A tool
// this server does not offer is left to the dispatcher to refuse.
func (s *Server) checkArguments(name string, raw json.RawMessage) error {
	tools := s.tools()
	i := slices.IndexFunc(tools, func(t map[string]any) bool { return t["name"] == name })
	if i < 0 {
		return nil
	}
	schema, _ := tools[i]["inputSchema"].(map[string]any)
	var v any = map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return &toolRefusal{"invalid arguments: not JSON"}
		}
		if dec.More() {
			return &toolRefusal{"invalid arguments: more than one JSON value"}
		}
	}
	if err := checkValue("arguments", schema, v); err != nil {
		return &toolRefusal{"invalid arguments: " + err.Error()}
	}
	return nil
}

func checkValue(path string, schema map[string]any, v any) error {
	switch schema["type"] {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", path)
		}
		props, named := schema["properties"].(map[string]any)
		extra, _ := schema["additionalProperties"].(map[string]any)
		if !named && extra == nil {
			return nil // a free-form object (a contribution's artifact): the tool validates it as its HTTP route does
		}
		for k, pv := range obj {
			ps, known := props[k].(map[string]any)
			switch {
			case known:
			case extra != nil:
				ps = extra
			default:
				return fmt.Errorf("%s has no property %q", path, k)
			}
			if pv == nil {
				continue // null is absent, as encoding/json reads it
			}
			if err := checkValue(path+"."+k, ps, pv); err != nil {
				return err
			}
		}
		for _, k := range required(schema["required"]) {
			if obj[k] == nil {
				return fmt.Errorf("%s needs %q", path, k)
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", path)
		}
		if n, ok := bound(schema["minItems"]); ok && int64(len(arr)) < n {
			return fmt.Errorf("%s needs at least %d items", path, n)
		}
		items, _ := schema["items"].(map[string]any)
		for i, e := range arr {
			if items == nil {
				break
			}
			if err := checkValue(fmt.Sprintf("%s[%d]", path, i), items, e); err != nil {
				return err
			}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", path)
		}
		if n, ok := bound(schema["maxLength"]); ok && int64(utf8.RuneCountInString(str)) > n {
			return fmt.Errorf("%s must be at most %d characters", path, n)
		}
		if enum := required(schema["enum"]); len(enum) > 0 && !slices.Contains(enum, str) {
			return fmt.Errorf("%s must be one of %s", path, strings.Join(enum, ", "))
		}
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return fmt.Errorf("%s must be an integer", path)
		}
		i, err := n.Int64()
		if err != nil {
			return fmt.Errorf("%s must be a whole number that fits in 64 bits", path)
		}
		if min, ok := bound(schema["minimum"]); ok && i < min {
			return fmt.Errorf("%s must be at least %d", path, min)
		}
	case "number":
		if _, ok := v.(json.Number); !ok {
			return fmt.Errorf("%s must be a number", path)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s must be true or false", path)
		}
	}
	return nil
}

func required(v any) []string {
	switch r := v.(type) {
	case []string:
		return r
	case []any:
		var out []string
		for _, s := range r {
			if s, ok := s.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func bound(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}
