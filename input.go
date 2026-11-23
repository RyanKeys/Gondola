package main

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// inputPreview (SPEC section 21) extracts a short, human-recognizable
// preview of what a generation request asked for, from the same body
// the router already parsed for its model field. Chat-shaped bodies
// yield the last user message's text; generate-shaped bodies yield
// the prompt. Anything else yields "". The preview is
// whitespace-collapsed and capped at inputPreviewMax runes; it lives
// only in the in-memory activity ring.
const inputPreviewMax = 160

func inputPreview(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var probe struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Prompt json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	text := ""
	for i := len(probe.Messages) - 1; i >= 0; i-- {
		if probe.Messages[i].Role != "user" {
			continue
		}
		if t := messageText(probe.Messages[i].Content); t != "" {
			text = t
			break
		}
	}
	if text == "" && len(probe.Prompt) > 0 {
		var p string
		if err := json.Unmarshal(probe.Prompt, &p); err == nil {
			text = p
		}
	}
	return truncatePreview(text)
}

// messageText flattens a chat message's content: a plain string, or
// an array of content parts whose text fields are concatenated.
func messageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

func truncatePreview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= inputPreviewMax {
		return s
	}
	return string([]rune(s)[:inputPreviewMax]) + "…"
}
