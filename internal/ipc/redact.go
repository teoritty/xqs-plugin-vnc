// This file (redact.go) sanitizes JSON-RPC params before they reach
// log.write or any other plugin-side logging sink, so that secret
// connection-field values (most notably a VNC password carried through
// session.connect's fields map, see docs/plugin-api.md "Connect fields")
// never leak into logs.
//
// Per docs/security-model.md: "Sensitive field keys (password, secret,
// token, key, …) are stripped at the IPC boundary." This mirrors that
// host-side behavior on the plugin side, defensively, for anything this
// plugin itself chooses to log.
package ipc

import "encoding/json"

// RedactedPlaceholder replaces the value of any sensitive key when
// redacting a params structure for logging.
const RedactedPlaceholder = "[REDACTED]"

// sensitiveKeys lists the field-name substrings/exact names that must
// never appear unredacted in a log. Matching is case-insensitive and
// exact against each JSON object key. Extend this list, don't scatter
// new sensitive-key literals elsewhere.
var sensitiveKeys = map[string]bool{
	"password":    true,
	"secret":      true,
	"token":       true,
	"key":         true,
	"passphrase":  true,
	"credential":  true,
	"credentials": true,
}

// isSensitiveKey reports whether key names a field that must be redacted
// before logging. Comparison is case-insensitive.
func isSensitiveKey(key string) bool {
	return sensitiveKeys[lower(key)]
}

// lower is a tiny ASCII lowercaser, avoiding a strings.ToLower import for
// such a small, hot-ish helper; params maps are small so this is not a
// performance concern, but keeping it local avoids widening this file's
// dependency surface.
func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// RedactValue returns a deep copy of v with the values of any map keys
// matching sensitiveKeys replaced by RedactedPlaceholder, recursively
// through nested maps and slices. v is not mutated. Supported input
// shapes: map[string]any, []any, and JSON scalar types (string, float64,
// bool, nil) as produced by encoding/json unmarshaling into `any`.
// Any other concrete type is returned unchanged (best-effort; callers
// that need JSON-tree redaction should decode into `any` first, e.g. via
// RedactJSON).
func RedactValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, elem := range val {
			if isSensitiveKey(k) {
				out[k] = RedactedPlaceholder
				continue
			}
			out[k] = RedactValue(elem)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, elem := range val {
			out[i] = RedactValue(elem)
		}
		return out
	default:
		return val
	}
}

// RedactJSON redacts raw JSON bytes (typically a JSON-RPC params value)
// and returns the redacted form re-marshaled to JSON. The input is not
// mutated. If payload is empty or not valid JSON, it is returned
// unchanged (redaction is best-effort logging hygiene, not a parser: a
// malformed payload should surface as a parse error elsewhere, not here).
func RedactJSON(payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 {
		return payload
	}
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return payload
	}
	redacted := RedactValue(v)
	out, err := json.Marshal(redacted)
	if err != nil {
		return payload
	}
	return out
}
