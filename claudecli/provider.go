// Package claudecli adapts the Claude Code CLI (`claude -p`) to llm.Provider.
// Each completion is one stateless print-mode call that uses the CLI's own
// login (Claude subscription) instead of an API key. The CLI has no external
// tool protocol, so tool calls are requested through a JSON schema and returned
// as tool_use blocks; the CLI's built-in tools stay disabled.
package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

// DefaultBinary is resolved through PATH when Options.Binary is empty.
const DefaultBinary = "claude"

// replySchema is the structured output each call must return.
const replySchema = `{"type":"object","properties":{"text":{"type":"string"},"tool_calls":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"name":{"type":"string"},"input":{"type":"object"}},"required":["id","name","input"]}}},"required":["text","tool_calls"]}`

// Options configure the CLI provider.
type Options struct {
	Binary string // executable name or path; empty uses DefaultBinary
	Model  string // passed as --model; empty uses the CLI default
}

// Provider runs completions through the Claude Code CLI.
type Provider struct {
	bin   string
	model string
}

// New builds a provider. The binary is looked up at call time, so a missing
// CLI surfaces as a request error instead of blocking startup.
func New(opts Options) *Provider {
	bin := opts.Binary
	if bin == "" {
		bin = DefaultBinary
	}
	return &Provider{bin: bin, model: opts.Model}
}

// cliResult is the subset of `--output-format json` the provider reads.
type cliResult struct {
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	Usage            struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

type reply struct {
	Text      string `json:"text"`
	ToolCalls []struct {
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"tool_calls"`
}

// Complete implements llm.Provider with one CLI call per request.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	out, err := p.run(ctx, req)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	var r reply
	raw := out.StructuredOutput
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte(out.Result)
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return llm.Message{}, "", llm.Usage{}, fmt.Errorf("claude cli: structured reply not parsed: %w", err)
	}
	var content []llm.ContentBlock
	if r.Text != "" {
		content = append(content, llm.TextBlock(r.Text))
	}
	stop := "end_turn"
	for i, c := range r.ToolCalls {
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("cli_call_%d", i+1)
		}
		input := c.Input
		if len(input) == 0 || string(input) == "null" {
			input = json.RawMessage("{}")
		}
		content = append(content, llm.ContentBlock{Type: llm.BlockToolUse, ID: id, Name: c.Name, Input: input})
		stop = "tool_use"
	}
	usage := llm.Usage{
		InputTokens:      out.Usage.InputTokens,
		OutputTokens:     out.Usage.OutputTokens,
		CacheReadTokens:  out.Usage.CacheReadInputTokens,
		CacheWriteTokens: out.Usage.CacheCreationInputTokens,
	}
	return llm.Message{Role: llm.RoleAssistant, Content: content}, stop, usage, nil
}

// Stream replays the completed reply as normalized events. The CLI returns
// structured output only at the end, so this path gives no live text.
func (p *Provider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		msg, stop, usage, err := p.Complete(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		events := []llm.StreamEvent{{Type: llm.SEMessageStart}}
		for _, b := range msg.Content {
			switch b.Type {
			case llm.BlockText:
				events = append(events, llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text})
			case llm.BlockToolUse:
				events = append(events,
					llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name},
					llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)})
			}
		}
		events = append(events,
			llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: usage},
			llm.StreamEvent{Type: llm.SEMessageStop})
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (p *Provider) run(ctx context.Context, req llm.CompletionRequest) (cliResult, error) {
	dir := filepath.Join(os.TempDir(), "scopeweaver-claudecli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return cliResult{}, fmt.Errorf("claude cli: work dir: %w", err)
	}
	// System prompt goes through a file: it can exceed the per-argument limit.
	sysFile, err := os.CreateTemp(dir, "system-*.md")
	if err != nil {
		return cliResult{}, fmt.Errorf("claude cli: system prompt file: %w", err)
	}
	defer os.Remove(sysFile.Name())
	if _, err := sysFile.WriteString(buildSystem(req)); err != nil {
		sysFile.Close()
		return cliResult{}, fmt.Errorf("claude cli: system prompt file: %w", err)
	}
	sysFile.Close()

	args := []string{
		"-p",
		"--output-format", "json",
		"--json-schema", replySchema,
		"--system-prompt-file", sysFile.Name(),
		"--tools", "",
		// Account connectors (claude.ai MCP) would otherwise appear as native tools.
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--disable-slash-commands",
		"--no-session-persistence",
	}
	if p.model != "" {
		args = append(args, "--model", p.model)
	}
	cmd := exec.CommandContext(ctx, p.bin, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(renderConversation(req.Messages))
	cmd.Env = cleanEnv(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return cliResult{}, ctx.Err()
		}
		return cliResult{}, fmt.Errorf("claude cli: %w: %s", err, tail(stderr.String()+stdout.String()))
	}
	var out cliResult
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return cliResult{}, fmt.Errorf("claude cli: output not JSON: %w: %s", err, tail(stdout.String()))
	}
	if out.IsError || out.Subtype != "success" {
		return cliResult{}, fmt.Errorf("claude cli: %s: %s", out.Subtype, tail(out.Result))
	}
	return out, nil
}

// buildSystem joins host system segments and describes the tools. The CLI runs
// no tools itself, so the model requests calls here and the host executes them.
func buildSystem(req llm.CompletionRequest) string {
	var b strings.Builder
	for _, s := range req.System {
		if s != "" {
			b.WriteString(s)
			b.WriteString("\n\n")
		}
	}
	if len(req.Tools) == 0 {
		return b.String()
	}
	b.WriteString("## Tools\n")
	b.WriteString("You have no native tools in this session; never attempt a direct tool call. You cannot execute tools yourself. To use one, put a call in tool_calls with a unique id, the exact tool name, and an input object matching its input_schema. The host runs it and returns the result in the next turn. Use only the tools listed here. Put any visible reply in text. When no tool is needed, return an empty tool_calls array.\n\n")
	for _, t := range req.Tools {
		schema, _ := json.Marshal(t.InputSchema)
		fmt.Fprintf(&b, "### %s\n%s\ninput_schema: %s\n\n", t.Name, t.Description, schema)
	}
	return b.String()
}

// renderConversation flattens the transcript into one prompt. Each call is
// stateless, so the full history travels in stdin. Thinking blocks are not
// replayed.
func renderConversation(msgs []llm.Message) string {
	var b strings.Builder
	b.WriteString("<conversation>\n")
	for _, m := range msgs {
		for _, blk := range m.Content {
			switch blk.Type {
			case llm.BlockText:
				fmt.Fprintf(&b, "[%s]\n%s\n\n", m.Role, blk.Text)
			case llm.BlockToolUse:
				fmt.Fprintf(&b, "[assistant tool_call id=%s name=%s]\n%s\n\n", blk.ID, blk.Name, blk.Input)
			case llm.BlockToolResult:
				var text strings.Builder
				for _, c := range blk.Content {
					text.WriteString(c.Text)
				}
				fmt.Fprintf(&b, "[tool_result id=%s error=%t]\n%s\n\n", blk.ToolUseID, blk.IsError, text.String())
			}
		}
	}
	b.WriteString("</conversation>\n\nRespond as the assistant to the conversation above.\n")
	return b.String()
}

// cleanEnv drops ANTHROPIC_API_KEY so the CLI uses its subscription login.
// A key in the environment would otherwise switch billing to the API.
func cleanEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	const max = 600
	if len(s) > max {
		return "..." + s[len(s)-max:]
	}
	return s
}
