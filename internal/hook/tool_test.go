package hook

import (
	"encoding/json"
	"testing"
)

func TestSummarizeTool(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"Bash", `{"command":"go test ./...","description":"run tests"}`, "go test ./..."},
		{"Edit", `{"file_path":"internal/server/server.go","old_string":"a","new_string":"b"}`, "internal/server/server.go"},
		{"Write", `{"file_path":"main.go","content":"package main"}`, "main.go"},
		{"Read", `{"file_path":"README.md"}`, "README.md"},
		{"Grep", `{"pattern":"func main","glob":"*.go"}`, "func main  (*.go)"},
		{"Glob", `{"pattern":"**/*.ts"}`, "**/*.ts"},
		{"Unknown", `{"foo":"bar"}`, `{"foo":"bar"}`},
	}
	for _, c := range cases {
		if got := summarizeTool(c.name, json.RawMessage(c.input)); got != c.want {
			t.Errorf("summarizeTool(%s, %s) = %q, want %q", c.name, c.input, got, c.want)
		}
	}
}

func TestToolStatus(t *testing.T) {
	cases := map[string]string{
		`{"is_error":true}`:  " ❌",
		`{"is_error":false}`: "",
		`{"error":"boom"}`:   " ❌",
		`{"stdout":"ok"}`:    "",
		``:                   "",
		`{"isError":true}`:   " ❌",
	}
	for in, want := range cases {
		if got := toolStatus(json.RawMessage(in)); got != want {
			t.Errorf("toolStatus(%q) = %q, want %q", in, got, want)
		}
	}
}
