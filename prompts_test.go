package main

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPromptsListAndGet(t *testing.T) {
	ts := setupMCPServerAndClient(t)
	defer ts.cleanup()

	list, err := ts.session.ListPrompts(ts.ctx, &mcp.ListPromptsParams{})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]*mcp.Prompt)
	for _, prompt := range list.Prompts {
		got[prompt.Name] = prompt
	}
	for _, name := range []string{"debug-source", "debug-attach", "debug-core-dump", "debug-binary"} {
		if got[name] == nil {
			t.Errorf("prompts/list omitted %q", name)
		}
	}

	tests := []struct {
		name string
		args map[string]string
		want []string
	}{
		{"debug-source", map[string]string{"path": "/workspace/main.go"}, []string{"debug", "/workspace/main.go", "delve"}},
		{"debug-source", map[string]string{"path": "/workspace/main.c", "language": "c"}, []string{"debug", "/workspace/main.c", "gdb"}},
		{"debug-attach", map[string]string{"pid": "42"}, []string{"attach", "42"}},
		{"debug-core-dump", map[string]string{"binary_path": "/tmp/app", "core_path": "/tmp/core"}, []string{"/tmp/app", "/tmp/core"}},
		{"debug-binary", map[string]string{"path": "/tmp/app"}, []string{"debug", "/tmp/app"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := ts.session.GetPrompt(ts.ctx, &mcp.GetPromptParams{Name: test.name, Arguments: test.args})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Messages) == 0 {
				t.Fatal("prompts/get returned no messages")
			}
			var text strings.Builder
			for _, message := range result.Messages {
				if content, ok := message.Content.(*mcp.TextContent); ok {
					text.WriteString(content.Text)
				}
			}
			for _, want := range test.want {
				if !strings.Contains(text.String(), want) {
					t.Errorf("prompt text does not contain %q:\n%s", want, text.String())
				}
			}
		})
	}
}
