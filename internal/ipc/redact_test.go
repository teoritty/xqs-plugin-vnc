package ipc

import (
	"encoding/json"
	"testing"
)

func TestRedactJSONMasksPasswordLeavesOthersUntouched(t *testing.T) {
	in := json.RawMessage(`{"username":"admin","password":"hunter2","host":"1.2.3.4"}`)
	out := RedactJSON(in)

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("Unmarshal redacted output: %v", err)
	}
	if m["password"] != RedactedPlaceholder {
		t.Errorf("password = %v, want %v", m["password"], RedactedPlaceholder)
	}
	if m["username"] != "admin" {
		t.Errorf("username = %v, want admin", m["username"])
	}
	if m["host"] != "1.2.3.4" {
		t.Errorf("host = %v, want 1.2.3.4", m["host"])
	}
}

func TestRedactJSONDoesNotMutateInput(t *testing.T) {
	in := json.RawMessage(`{"password":"hunter2"}`)
	inCopy := make(json.RawMessage, len(in))
	copy(inCopy, in)

	_ = RedactJSON(in)

	if string(in) != string(inCopy) {
		t.Errorf("input payload was mutated: got %s, want %s", in, inCopy)
	}
}

func TestRedactValueDoesNotMutateInputMap(t *testing.T) {
	in := map[string]any{"password": "hunter2", "nested": map[string]any{"token": "abc"}}
	out := RedactValue(in).(map[string]any)

	if in["password"] != "hunter2" {
		t.Errorf("original map mutated: password = %v", in["password"])
	}
	nestedIn := in["nested"].(map[string]any)
	if nestedIn["token"] != "abc" {
		t.Errorf("original nested map mutated: token = %v", nestedIn["token"])
	}

	if out["password"] != RedactedPlaceholder {
		t.Errorf("out password = %v, want redacted", out["password"])
	}
	nestedOut := out["nested"].(map[string]any)
	if nestedOut["token"] != RedactedPlaceholder {
		t.Errorf("out nested token = %v, want redacted", nestedOut["token"])
	}
}

func TestRedactValueCaseInsensitiveAndMultipleKeys(t *testing.T) {
	in := map[string]any{
		"Password":   "a",
		"SECRET":     "b",
		"token":      "c",
		"apiKey":     "d", // not an exact match, should NOT be redacted (key list is exact-name based)
		"passphrase": "e",
	}
	out := RedactValue(in).(map[string]any)
	if out["Password"] != RedactedPlaceholder {
		t.Errorf("Password not redacted: %v", out["Password"])
	}
	if out["SECRET"] != RedactedPlaceholder {
		t.Errorf("SECRET not redacted: %v", out["SECRET"])
	}
	if out["token"] != RedactedPlaceholder {
		t.Errorf("token not redacted: %v", out["token"])
	}
	if out["passphrase"] != RedactedPlaceholder {
		t.Errorf("passphrase not redacted: %v", out["passphrase"])
	}
}

func TestRedactValueHandlesArraysOfObjects(t *testing.T) {
	in := map[string]any{
		"fields": []any{
			map[string]any{"key": "id1", "password": "p1"},
			map[string]any{"key": "id2", "password": "p2"},
		},
	}
	out := RedactValue(in).(map[string]any)
	fields := out["fields"].([]any)
	for _, elem := range fields {
		m := elem.(map[string]any)
		if m["password"] != RedactedPlaceholder {
			t.Errorf("password not redacted in array element: %v", m["password"])
		}
	}
}

func TestRedactJSONEmptyPayloadUnchanged(t *testing.T) {
	var empty json.RawMessage
	out := RedactJSON(empty)
	if len(out) != 0 {
		t.Errorf("expected empty output for empty input, got %s", out)
	}
}
