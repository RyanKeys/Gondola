package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestInputPreview(t *testing.T) {
	long := strings.Repeat("word ", 60) // 300 chars
	cases := []struct {
		name string
		body string
		want string
	}{
		{"v1 chat last user wins", `{"model":"heavy","messages":[{"role":"system","content":"SYSSECRET"},{"role":"user","content":"first ask"},{"role":"assistant","content":"reply"},{"role":"user","content":"second ask"}]}`, "second ask"},
		{"system prompt never included", `{"model":"brain","messages":[{"role":"system","content":"SYSSECRET"},{"role":"user","content":"hello there"}]}`, "hello there"},
		{"content parts array", `{"model":"heavy","messages":[{"role":"user","content":[{"type":"text","text":"part one "},{"type":"text","text":"part two"}]}]}`, "part one part two"},
		{"native chat string content", `{"model":"brain","messages":[{"role":"user","content":"native ask"}]}`, "native ask"},
		{"generate prompt", `{"model":"utility","prompt":"once upon a time"}`, "once upon a time"},
		{"whitespace collapsed", `{"model":"brain","messages":[{"role":"user","content":"line one\n\n  line\ttwo"}]}`, "line one line two"},
		{"no user message", `{"model":"brain","messages":[{"role":"system","content":"only system"}]}`, ""},
		{"embeddings body has no preview", `{"model":"utility","input":"embed me"}`, ""},
		{"non json", `not json at all`, ""},
		{"empty body", ``, ""},
		{"long input truncated", `{"model":"brain","prompt":"` + long + `"}`, string([]rune(strings.TrimSpace(long))[:inputPreviewMax]) + "…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := inputPreview([]byte(c.body))
			if got != c.want {
				t.Errorf("inputPreview = %q, want %q", got, c.want)
			}
			if utf8.RuneCountInString(got) > inputPreviewMax+1 {
				t.Errorf("preview too long: %d runes", utf8.RuneCountInString(got))
			}
			if strings.Contains(got, "SYSSECRET") {
				t.Errorf("system prompt leaked into preview: %q", got)
			}
		})
	}
}
