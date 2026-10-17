package claudecli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// fakeCLI writes a shell stand-in for `claude`. It copies stdin to stdin.txt
// and the arguments to args.txt, then prints body as the CLI JSON result.
func fakeCLI(t *testing.T, body string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := "#!/bin/sh\n" +
		"cat > " + filepath.Join(dir, "stdin.txt") + "\n" +
		"printf '%s\\n' \"$@\" > " + filepath.Join(dir, "args.txt") + "\n" +
		"cat <<'EOF'\n" + body + "\nEOF\n"
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

const okBody = `{"type":"result","subtype":"success","is_error":false,"result":"{}",
"structured_output":{"text":"looking","tool_calls":[{"id":"t1","name":"echo","input":{"x":1}}]},
"usage":{"input_tokens":5,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":11}}`

func request() llm.CompletionRequest {
	return llm.CompletionRequest{
		System: []string{"be terse"},
		Messages: []llm.Message{
			llm.UserText("hi"),
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Type: llm.BlockThinking, Thinking: "secret"},
				{Type: llm.BlockToolUse, ID: "p1", Name: "echo", Input: json.RawMessage(`{"x":0}`)},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("p1", "pong", false)}},
		},
		Tools: []llm.ToolSchema{{Name: "echo", Description: "echoes", InputSchema: map[string]any{"type": "object"}}},
	}
}

func TestCompleteMapsToolCallsAndUsage(t *testing.T) {
	bin, dir := fakeCLI(t, okBody)
	p := New(Options{Binary: bin, Model: "claude-haiku-5-5"})

	msg, stop, usage, err := p.Complete(context.Background(), request())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if stop != "tool_use" {
		t.Fatalf("stop = %q, want tool_use", stop)
	}
	if msg.Text() != "looking" {
		t.Fatalf("text = %q", msg.Text())
	}
	calls := msg.ToolUses()
	if len(calls) != 1 || calls[0].ID != "t1" || calls[0].Name != "echo" || string(calls[0].Input) != `{"x":1}` {
		t.Fatalf("tool calls = %+v", calls)
	}
	want := llm.Usage{InputTokens: 5, OutputTokens: 7, CacheReadTokens: 3, CacheWriteTokens: 11}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}

	args, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	for _, need := range []string{"--tools", "--json-schema", "--system-prompt-file", "--model", "claude-haiku-5-5", "--no-session-persistence", "--strict-mcp-config"} {
		if !strings.Contains(string(args), need) {
			t.Errorf("args missing %s:\n%s", need, args)
		}
	}
}

func TestPromptCarriesHistoryAndToolsWithoutThinking(t *testing.T) {
	bin, dir := fakeCLI(t, okBody)
	if _, _, _, err := New(Options{Binary: bin}).Complete(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin.txt"))
	s := string(stdin)
	for _, need := range []string{"[user]\nhi", "tool_call id=p1 name=echo", `{"x":0}`, "[tool_result id=p1 error=false]\npong"} {
		if !strings.Contains(s, need) {
			t.Errorf("stdin missing %q:\n%s", need, s)
		}
	}
	if strings.Contains(s, "secret") {
		t.Error("thinking block replayed into prompt")
	}
}

func TestEmptyToolCallsEndTurn(t *testing.T) {
	bin, _ := fakeCLI(t, `{"subtype":"success","is_error":false,"result":"","structured_output":{"text":"done","tool_calls":[]}}`)
	msg, stop, _, err := New(Options{Binary: bin}).Complete(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if stop != "end_turn" || msg.Text() != "done" || len(msg.ToolUses()) != 0 {
		t.Fatalf("stop=%q msg=%+v", stop, msg)
	}
}

func TestCLIErrorSurfaces(t *testing.T) {
	bin, _ := fakeCLI(t, `{"subtype":"error_during_execution","is_error":true,"result":"usage limit reached"}`)
	_, _, _, err := New(Options{Binary: bin}).Complete(context.Background(), request())
	if err == nil || !strings.Contains(err.Error(), "usage limit reached") {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingBinaryIsRequestError(t *testing.T) {
	_, _, _, err := New(Options{Binary: filepath.Join(t.TempDir(), "nope")}).Complete(context.Background(), request())
	if err == nil {
		t.Fatal("want error for missing binary")
	}
}

func TestStreamReplaysCompletion(t *testing.T) {
	bin, _ := fakeCLI(t, okBody)
	var types []llm.StreamEventType
	var toolInput string
	for ev, err := range New(Options{Binary: bin}).Stream(context.Background(), request()) {
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, ev.Type)
		if ev.Type == llm.SEToolInputJSON {
			toolInput = ev.Text
		}
	}
	want := []llm.StreamEventType{llm.SEMessageStart, llm.SETextDelta, llm.SEToolUseStart, llm.SEToolInputJSON, llm.SEMessageDelta, llm.SEMessageStop}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event %d = %s, want %s", i, types[i], want[i])
		}
	}
	if toolInput != `{"x":1}` {
		t.Fatalf("tool input = %s", toolInput)
	}
}

func TestCleanEnvDropsAPIKey(t *testing.T) {
	got := cleanEnv([]string{"PATH=/bin", "ANTHROPIC_API_KEY=sk-x", "HOME=/root"})
	if len(got) != 2 || got[0] != "PATH=/bin" || got[1] != "HOME=/root" {
		t.Fatalf("cleanEnv = %v", got)
	}
}
