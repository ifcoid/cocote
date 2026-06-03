package hook

import (
	"strings"
	"testing"
)

func TestLastAssistantText(t *testing.T) {
	// A small transcript with: a user turn, an early assistant turn, a
	// non-JSON noise line, then the latest assistant turn (which should win).
	// The latest assistant message mixes a text block with a tool_use block;
	// only the text should be extracted.
	transcript := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first answer"}]}}`,
		`not json, should be skipped`,
		`{"type":"summary","summary":"ignore me"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"latest line one"},{"type":"tool_use","name":"Bash","input":{}},{"type":"text","text":"latest line two"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`,
	}, "\n")

	got, err := LastAssistantText(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("LastAssistantText: %v", err)
	}
	want := "latest line one\nlatest line two"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLastAssistantTextStringContent(t *testing.T) {
	// Some entries carry content as a plain string rather than blocks.
	transcript := `{"type":"assistant","message":{"role":"assistant","content":"plain string answer"}}`
	got, err := LastAssistantText(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("LastAssistantText: %v", err)
	}
	if got != "plain string answer" {
		t.Errorf("got %q, want %q", got, "plain string answer")
	}
}

func TestLastAssistantTextNone(t *testing.T) {
	transcript := `{"type":"user","message":{"role":"user","content":"only a user message"}}`
	got, err := LastAssistantText(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("LastAssistantText: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"abc-123_X":         "abc-123_X",
		"a/b\\c:d*e":        "a_b_c_d_e",
		"":                  "default",
		"550e8400-e29b-41d": "550e8400-e29b-41d",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
