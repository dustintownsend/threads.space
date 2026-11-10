package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/config"
	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/threads"
)

type Input struct {
	ScopeID             string
	Event               threads.Event
	ThreadID            string
	RunnerSessionID     string
	NewSession          bool
	GenerateThreadTitle bool
	OnSessionAccepted   func(context.Context, string) error
	OnToolEvent         ToolEventHandler
	OnLimitEvent        LimitEventHandler
}

type Output struct {
	Text            string
	RunnerSessionID string
	ThreadTitle     string
	Reactions       []Reaction
}

type Reaction struct {
	MessageID string `json:"message_id"`
	Emoji     string `json:"emoji"`
}

type ToolEventStatus string

const (
	ToolEventStarted   ToolEventStatus = "started"
	ToolEventCompleted ToolEventStatus = "completed"
)

type ToolEvent struct {
	ID     string
	Name   string
	Input  any
	Output any
	Status ToolEventStatus
	Error  bool
}

type ToolEventHandler func(context.Context, ToolEvent) error

type LimitEvent struct {
	Source   string
	Message  string
	Severity string
	Metadata map[string]any
}

type LimitEventHandler func(context.Context, LimitEvent) error

type Runner interface {
	Run(context.Context, config.Scope, Input) (Output, error)
}

type LocalRunner struct{}

func (LocalRunner) Run(ctx context.Context, scope config.Scope, input Input) (Output, error) {
	if scope.Runner.Type == "t3" {
		return runT3(ctx, scope, input)
	}
	adapter := adapterFor(scope.Runner.Type)
	invocation := adapter.BuildInvocation(scope, input)
	if invocation.UnsupportedMessage != "" {
		return Output{Text: invocation.UnsupportedMessage, RunnerSessionID: input.RunnerSessionID}, nil
	}
	cmd := exec.CommandContext(ctx, scope.Runner.Command, invocation.Args...)
	if scope.Runner.WorkingDir != "" {
		cmd.Dir = scope.Runner.WorkingDir
	}
	cmd.Env = agentEnv(scope, input)
	cmd.Stdin = strings.NewReader(invocation.Prompt)
	var stdout, stderr bytes.Buffer
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Output{}, err
	}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Output{}, err
	}
	scanErr := scanAdapterOutput(ctx, adapter, stdoutPipe, &stdout, input.OnToolEvent, input.OnLimitEvent)
	if err := cmd.Wait(); err != nil {
		if stderr.Len() > 0 {
			return Output{}, errWithStderr{err: err, stderr: stderr.String()}
		}
		return Output{}, err
	}
	if scanErr != nil {
		return Output{}, scanErr
	}
	parsed := adapter.ParseOutput(stdout.Bytes())
	if parsed.Text == "" && parsed.Error != "" {
		return Output{}, errors.New(parsed.Error)
	}
	if parsed.Text == "" && !parsed.SawJSON {
		parsed.Text = strings.TrimSpace(stdout.String())
	}
	out := Output{Text: parsed.Text, RunnerSessionID: parsed.SessionID}
	if scope.Runner.Structured {
		out = parseStructuredOutput(out, input.Event.Message.ID)
	}
	return out, nil
}

type promptHandling struct {
	UnsupportedMessage string
}

func buildRunnerPrompt(scope config.Scope, input Input) (string, promptHandling) {
	invocation := adapterFor(scope.Runner.Type).BuildInvocation(scope, input)
	return invocation.Prompt, promptHandling{UnsupportedMessage: invocation.UnsupportedMessage}
}

func buildPrompt(scope config.Scope, input Input) string {
	return buildBridgeInstructions(scope, input) + "\n" + buildUserPrompt(input)
}

func nativeSlashCommand(input Input) (string, bool) {
	if input.Event.Emoji != "" {
		return "", false
	}
	content := strings.TrimSpace(input.Event.Message.Content)
	if content == "" || !strings.HasPrefix(content, "/") || strings.ContainsAny(content, "\r\n") {
		return "", false
	}
	name := commandName(content)
	if len(name) < 2 {
		return "", false
	}
	for _, r := range name[1:] {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return "", false
	}
	return content, true
}

func commandName(command string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(command), " ")
	return name
}

func buildBridgeInstructions(scope config.Scope, input Input) string {
	var b strings.Builder
	b.WriteString("You are running inside a Threads agent bridge session.\n")
	if input.NewSession {
		b.WriteString("This is the first turn for this Threads root message; treat it as a new agent conversation.\n")
	} else {
		b.WriteString("This is a reply in an existing Threads thread; continue the existing agent conversation/session.\n")
	}
	b.WriteString("The current Threads context is available in environment variables: THREADS_API, THREADS_TOKEN, THREADS_CHANNEL_ID, THREADS_THREAD_ID, THREADS_MESSAGE_ID, THREADS_SCOPE_ID, THREADS_RUNNER_SESSION_ID, and THREADS_REACTION_EMOJI when applicable.\n")
	b.WriteString("If useful, send interim progress before your final answer with: threads messages send --channel-id \"$THREADS_CHANNEL_ID\" --thread-id \"$THREADS_THREAD_ID\" --content \"...\" --message-type progress --metadata '{\"source\":\"threads-agent-bridge\",\"kind\":\"interim\"}'.\n")
	b.WriteString("Attach a local artifact by adding a repeatable --file or --image path to `threads messages send`.\n")
	b.WriteString("You can react to any Threads message by id with: threads reactions add --message-id \"$THREADS_MESSAGE_ID\" --emoji \"👍\". Replace $THREADS_MESSAGE_ID with another message id when you need to react elsewhere.\n")
	b.WriteString("You can explicitly title a thread with: threads messages title --message-id \"$THREADS_THREAD_ID\" --title \"...\" --if-unset true.\n")
	b.WriteString("Treat `threads messages send`, `threads reactions add`, and `threads messages title` as side effects only; still write your final answer to stdout when finished so the bridge can post the final response.\n")
	if input.Event.Emoji != "" {
		b.WriteString("This run was invoked by an emoji reaction. THREADS_REACTION_EMOJI contains the triggering emoji, and THREADS_MESSAGE_ID is the reacted-to message.\n")
	}
	if scope.Runner.Structured {
		if input.GenerateThreadTitle {
			b.WriteString("Structured final-output mode is enabled. This is a newly created Threads root message, so the final answer must be a single JSON object like {\"content\":\"message to post\",\"thread_title\":\"Concise descriptive title\",\"reactions\":[]}. Set thread_title to 3-8 words that describe the user's intent, preferably under 60 characters. Do not quote it, end it with punctuation, or use a generic title such as \"User Request\". The bridge will set the title; do not call a tool to do so. Use reactions only when they add value.\n")
		} else {
			b.WriteString("Structured final-output mode is enabled. For the final answer, write either plain text or a single JSON object like {\"content\":\"message to post\",\"reactions\":[{\"message_id\":\"$THREADS_MESSAGE_ID\",\"emoji\":\"👍\"}]}. Use reactions only when they add value.\n")
		}
	}
	return b.String()
}

func buildUserPrompt(input Input) string {
	var b strings.Builder
	b.WriteString("User message:\n")
	if input.Event.Message.Content != "" {
		b.WriteString(strings.TrimSpace(input.Event.Message.Content))
	} else if input.Event.Emoji != "" {
		b.WriteString("[Emoji reaction trigger: ")
		b.WriteString(input.Event.Emoji)
		b.WriteString("]")
	}
	b.WriteString("\n")
	return b.String()
}

func agentEnv(scope config.Scope, input Input) []string {
	env := append([]string{}, environ()...)
	if scope.Runner.WorkingDir != "" {
		env = prependPath(env, scope.Runner.WorkingDir+"/bin")
	}
	add := func(key, value string) {
		if value != "" {
			env = append(env, key+"="+value)
		}
	}
	token, _ := scope.Token()
	add("THREADS_BASE_URL", scope.Threads.BaseURL)
	add("THREADS_API_TOKEN", token)
	add("THREADS_API", scope.Threads.BaseURL)
	add("THREADS_TOKEN", token)
	add("THREADS_CHANNEL_ID", input.Event.ChannelID)
	add("THREADS_THREAD_ID", input.ThreadID)
	add("THREADS_MESSAGE_ID", input.Event.Message.ID)
	add("THREADS_REACTION_EMOJI", input.Event.Emoji)
	add("THREADS_SCOPE_ID", input.ScopeID)
	add("THREADS_RUNNER_SESSION_ID", input.RunnerSessionID)
	return env
}

var environ = os.Environ

type errWithStderr struct {
	err    error
	stderr string
}

func (e errWithStderr) Error() string { return e.err.Error() + ": " + e.stderr }
func (e errWithStderr) Unwrap() error { return e.err }

type parsedJSONLOutput struct {
	Text      string
	SessionID string
	SawJSON   bool
	Error     string
}

func parseJSONLText(data []byte) string { return parseJSONLOutput(data).Text }

func parseStructuredOutput(out Output, defaultMessageID string) Output {
	text := strings.TrimSpace(out.Text)
	if text == "" || !strings.HasPrefix(text, "{") {
		return out
	}
	var payload struct {
		Content     string            `json:"content"`
		Text        string            `json:"text"`
		ThreadTitle string            `json:"thread_title"`
		Reactions   []json.RawMessage `json:"reactions"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return out
	}
	if strings.TrimSpace(payload.Content) != "" {
		out.Text = strings.TrimSpace(payload.Content)
	} else {
		out.Text = strings.TrimSpace(payload.Text)
	}
	out.ThreadTitle = strings.TrimSpace(payload.ThreadTitle)
	for _, raw := range payload.Reactions {
		var reaction Reaction
		if err := json.Unmarshal(raw, &reaction); err != nil {
			if err := json.Unmarshal(raw, &reaction.Emoji); err != nil {
				continue
			}
		}
		if strings.TrimSpace(reaction.MessageID) == "" {
			reaction.MessageID = defaultMessageID
		}
		if strings.TrimSpace(reaction.MessageID) != "" && strings.TrimSpace(reaction.Emoji) != "" {
			out.Reactions = append(out.Reactions, Reaction{MessageID: strings.TrimSpace(reaction.MessageID), Emoji: strings.TrimSpace(reaction.Emoji)})
		}
	}
	return out
}

const maxRunnerLineBytes = 16 * 1024 * 1024

func scanRunnerOutput(ctx context.Context, r io.Reader, dst *bytes.Buffer, onToolEvent ToolEventHandler, onLimitEvent ...LimitEventHandler) error {
	return scanAdapterOutput(ctx, autoDetectAdapter{}, r, dst, onToolEvent, onLimitEvent...)
}

func scanAdapterOutput(ctx context.Context, adapter runnerAdapter, r io.Reader, dst *bytes.Buffer, onToolEvent ToolEventHandler, onLimitEvent ...LimitEventHandler) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), maxRunnerLineBytes)
	activeNames := map[string]string{}
	seenLimitEvents := map[string]bool{}
	var limitHandler LimitEventHandler
	if len(onLimitEvent) > 0 {
		limitHandler = onLimitEvent[0]
	}
	for s.Scan() {
		line := append([]byte(nil), s.Bytes()...)
		dst.Write(line)
		dst.WriteByte('\n')
		if limitHandler != nil {
			if event, ok := parseLimitEventWith(adapter, line); ok {
				signature := event.Source + "\x00" + event.Message
				if !seenLimitEvents[signature] {
					seenLimitEvents[signature] = true
					_ = limitHandler(ctx, event)
				}
			}
		}
		if onToolEvent == nil {
			continue
		}
		if event, ok := parseToolEventWith(adapter, line); ok {
			if event.Status == ToolEventStarted && event.ID != "" && event.Name != "" {
				activeNames[event.ID] = event.Name
			}
			if event.Name == "" && event.ID != "" {
				event.Name = activeNames[event.ID]
			}
			if event.Name == "" {
				event.Name = "tool"
			}
			if event.Status == ToolEventCompleted && event.ID != "" {
				delete(activeNames, event.ID)
			}
			// Tool-call UI is telemetry/progress. Do not fail the agent run if Threads
			// rejects an intermediate step row; the final response should still land.
			_ = onToolEvent(ctx, event)
		}
	}
	return s.Err()
}

func parseLimitEvent(line []byte) (LimitEvent, bool) {
	return parseLimitEventWith(autoDetectAdapter{}, line)
}

func parseToolEvent(line []byte) (ToolEvent, bool) {
	return parseToolEventWith(autoDetectAdapter{}, line)
}

func firstPresent(obj map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := obj[key]; ok {
			return value
		}
	}
	return nil
}

func truthy(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return v != "" && v != "false"
	default:
		return value != nil
	}
}

func parseJSONLOutput(data []byte) parsedJSONLOutput {
	var parts, results []string
	var lastAgentMessage string
	var sessionID string
	var sawJSON bool
	var lastError string
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 64*1024), maxRunnerLineBytes)
	for s.Scan() {
		line := bytes.TrimSpace(s.Bytes())
		if len(line) == 0 {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(line, &obj); err != nil {
			continue
		}
		sawJSON = true
		typeName := firstString(obj, "type")
		if sessionID == "" {
			if typeName == "session" {
				sessionID = firstString(obj, "sessionFile", "session_file", "sessionId", "session_id", "id")
			} else {
				sessionID = firstString(obj, "session_id", "thread_id", "sessionId", "sessionFile")
			}
		}
		if typeName == "agent_end" {
			if text := lastAssistantTextFromMessages(obj["messages"]); text != "" {
				parts = append(parts, text)
			}
			if errText := lastAssistantErrorFromMessages(obj["messages"]); errText != "" {
				lastError = errText
			}
			continue
		} else if typeName == "message_end" {
			if text := assistantTextFromMessage(obj["message"]); text != "" {
				lastAgentMessage = text
			}
			if errText := assistantErrorFromMessage(obj["message"]); errText != "" {
				lastError = errText
			}
			continue
		}
		if value, ok := obj["result"].(string); ok && strings.TrimSpace(value) != "" {
			results = append(results, strings.TrimSpace(value))
			continue
		}
		if item, ok := obj["item"].(map[string]any); ok && item["type"] == "agent_message" {
			if value, ok := item["text"].(string); ok && strings.TrimSpace(value) != "" {
				lastAgentMessage = strings.TrimSpace(value)
				continue
			}
		}
		if message, ok := obj["message"].(map[string]any); ok {
			if text := assistantTextFromOutputMessage(typeName, message); text != "" {
				parts = append(parts, text)
				continue
			}
			if errText := assistantErrorFromOutputMessage(typeName, message); errText != "" {
				lastError = errText
			}
		}
		for _, key := range []string{"text", "content", "message", "output"} {
			if value, ok := obj[key].(string); ok && strings.TrimSpace(value) != "" {
				parts = append(parts, strings.TrimSpace(value))
				break
			}
		}
	}
	text := strings.Join(parts, "\n")
	if lastAgentMessage != "" {
		text = lastAgentMessage
	}
	if len(results) > 0 {
		text = strings.Join(results, "\n")
	}
	if text != "" {
		lastError = ""
	}
	return parsedJSONLOutput{Text: text, SessionID: sessionID, SawJSON: sawJSON, Error: lastError}
}

func lastAssistantTextFromMessages(value any) string {
	messages, ok := value.([]any)
	if !ok {
		return ""
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if text := assistantTextFromMessage(messages[i]); text != "" {
			return text
		}
	}
	return ""
}

func lastAssistantErrorFromMessages(value any) string {
	messages, ok := value.([]any)
	if !ok {
		return ""
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if text := assistantErrorFromMessage(messages[i]); text != "" {
			return text
		}
	}
	return ""
}

func assistantTextFromMessage(value any) string {
	message, ok := value.(map[string]any)
	if !ok || firstString(message, "role") != "assistant" {
		return ""
	}
	return textFromMessageContent(message)
}

func assistantTextFromOutputMessage(eventType string, message map[string]any) string {
	role := firstString(message, "role")
	if role != "assistant" && !(role == "" && eventType == "assistant") {
		return ""
	}
	return textFromMessageContent(message)
}

func assistantErrorFromMessage(value any) string {
	message, ok := value.(map[string]any)
	if !ok || firstString(message, "role") != "assistant" {
		return ""
	}
	return errorFromMessage(message)
}

func assistantErrorFromOutputMessage(eventType string, message map[string]any) string {
	role := firstString(message, "role")
	if role != "assistant" && !(role == "" && eventType == "assistant") {
		return ""
	}
	return errorFromMessage(message)
}

func errorFromMessage(message map[string]any) string {
	if text := firstString(message, "errorMessage", "error_message", "error"); text != "" {
		return text
	}
	return ""
}

func textFromMessageContent(message map[string]any) string {
	if text := firstString(message, "content"); text != "" {
		return text
	}
	content, ok := message["content"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, part := range content {
		partObj, ok := part.(map[string]any)
		if !ok || firstString(partObj, "type") != "text" {
			continue
		}
		if text := firstString(partObj, "text"); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func numberFromAny(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case json.Number:
		n, err := v.Float64()
		if err == nil {
			return n
		}
	}
	return -1
}

func intFromNumber(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return int(n)
		}
	}
	return -1
}

func firstString(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := obj[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func prependPath(env []string, dir string) []string {
	if dir == "" {
		return env
	}
	for i, value := range env {
		if strings.HasPrefix(value, "PATH=") {
			env[i] = "PATH=" + dir + string(os.PathListSeparator) + strings.TrimPrefix(value, "PATH=")
			return env
		}
	}
	return append(env, "PATH="+dir)
}
