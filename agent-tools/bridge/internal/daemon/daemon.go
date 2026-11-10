package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/config"
	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/runner"
	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/store"
	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/threads"
)

type EventSource interface {
	Events(context.Context, string) (<-chan threads.Event, <-chan error)
}

type PresenceMaintainer interface {
	MaintainPresence(context.Context) error
}
type Sender interface {
	SendMessage(context.Context, string, threads.SendMessageRequest) error
}

type Reactor interface {
	AddReaction(context.Context, string, string) error
}

type ThreadTitler interface {
	SetThreadTitle(context.Context, string, threads.SetThreadTitleRequest) (threads.SetThreadTitleResponse, error)
}

type ProcessClient interface {
	CreateProcess(context.Context, threads.CreateProcessRequest) (threads.CreateProcessResponse, error)
	UpdateProcess(context.Context, string, threads.UpdateProcessRequest) error
	RecordProcessActivity(context.Context, string, threads.ProcessActivityRequest) error
	UpdateMessageProcessStatus(context.Context, string, threads.UpdateMessageProcessStatusRequest) error
}

type ClientFactory func(config.Scope, string) (EventSource, Sender, ProcessClient)

type Daemon struct {
	Config  config.Config
	Store   *store.Store
	Runner  runner.Runner
	Clients ClientFactory
	Logger  *slog.Logger

	activeMu sync.Mutex
	active   map[string]context.CancelFunc
	rootsMu  sync.Mutex
	roots    map[string]*rootGate
	workers  sync.WaitGroup
}

func (d *Daemon) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if d.Runner == nil {
		d.Runner = runner.LocalRunner{}
	}
	if d.Clients == nil {
		d.Clients = defaultClients
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	errc := make(chan error, len(d.Config.Scopes))
	var scopes sync.WaitGroup
	for _, scope := range d.Config.Scopes {
		scope := scope
		scopes.Add(1)
		go func() { defer scopes.Done(); errc <- d.runScope(ctx, scope) }()
	}
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errc:
	}
	cancel()
	drained := make(chan struct{})
	go func() { scopes.Wait(); d.workers.Wait(); close(drained) }()
	select {
	case <-drained:
		return err
	case <-time.After(45 * time.Second):
		return errors.Join(err, errors.New("bridge shutdown timed out while draining active runs"))
	}
}

func (d *Daemon) runScope(ctx context.Context, scope config.Scope) error {
	token, err := scope.Token()
	if err != nil {
		return err
	}
	eventsClient, sender, processes := d.Clients(scope, token)
	if presence, ok := eventsClient.(PresenceMaintainer); ok {
		go d.maintainPresence(ctx, scope, presence)
	}
	attempt := 0
	for ctx.Err() == nil {
		since := scope.Threads.Since
		if since == "" {
			since, err = d.Store.Cursor(ctx, scope.ID)
			if err != nil {
				return err
			}
		}
		err := d.consumeEvents(ctx, scope, eventsClient, sender, processes, since)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attempt++
		if d.Logger != nil {
			d.Logger.Warn("events websocket disconnected; reconnecting", "scope", scope.ID, "since", since, "attempt", attempt, "error", err)
		}
		select {
		case <-time.After(SleepBackoff(attempt)):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}

func (d *Daemon) consumeEvents(ctx context.Context, scope config.Scope, eventsClient EventSource, sender Sender, processes ProcessClient, since string) error {
	events, errs := eventsClient.Events(ctx, since)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return nil
			}
			if isProcessKillEvent(event) {
				d.HandleProcessKill(event)
				continue
			}
			d.workers.Add(1)
			go func(event threads.Event) {
				defer d.workers.Done()
				if err := d.HandleEvent(ctx, scope, sender, processes, event); err != nil {
					if d.Logger != nil {
						d.Logger.Error("handle event", "scope", scope.ID, "error", err)
					}
				}
			}(event)
		case err, ok := <-errs:
			if !ok {
				return nil
			}
			if err != nil {
				return err
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (d *Daemon) HandleEvent(ctx context.Context, scope config.Scope, sender Sender, processes ProcessClient, event threads.Event) error {
	if event.Cursor != "" {
		defer d.Store.SaveCursor(context.WithoutCancel(ctx), scope.ID, event.Cursor)
	}
	messageEvent := isMessageEvent(event)
	reactionEvent := isReactionEvent(event)
	if !messageEvent && !reactionEvent {
		return nil
	}
	if messageEvent && event.Message.Content == "" {
		return nil
	}
	threadID := rootThreadID(event)
	if messageEvent && !scope.Matches(event.ChannelID, threadID) {
		return nil
	}
	if reactionEvent {
		if scope.Threads.UserID != "" && event.ActorID == scope.Threads.UserID {
			return nil
		}
		if !reactionTargetsScope(scope, event.ChannelID, threadID, event.Message.SenderID) {
			return nil
		}
	}
	if messageEvent && scope.Threads.UserID != "" && event.Message.SenderID == scope.Threads.UserID {
		return nil
	}
	release, err := d.lockRoot(ctx, scope.ID+"\x00"+event.ChannelID+"\x00"+threadID)
	if err != nil {
		return err
	}
	defer release()
	eventID := event.ID
	if eventID == "" {
		eventID = event.Cursor
	}
	fresh, err := d.Store.MarkProcessed(ctx, scope.ID, eventID)
	if err != nil || !fresh {
		return err
	}
	runnerSessionID, err := d.Store.RunnerSessionID(ctx, scope.ID, event.ChannelID, threadID)
	if err != nil {
		return err
	}
	generateThreadTitle := scope.Runner.AutoTitle && runnerSessionID == "" && isNewRootMessage(event, threadID)
	processID := processID(scope.ID, event.Message.ID)
	if processes != nil {
		created, err := processes.CreateProcess(ctx, threads.CreateProcessRequest{ID: processID, ChannelID: event.ChannelID, MessageID: event.Message.ID, UserID: event.Message.SenderID, Status: "running", PID: os.Getpid()})
		if err != nil {
			d.warnNonFatalProcessError(scope, "create process", err)
			processes = nil
		} else {
			if created.ID != "" {
				processID = created.ID
			}
			if err := processes.UpdateMessageProcessStatus(ctx, event.Message.ID, threads.UpdateMessageProcessStatusRequest{ProcessID: processID, Status: "processing"}); err != nil {
				d.warnNonFatalProcessError(scope, "update initial message process status", err)
			}
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.trackProcess(processID, cancel)
	defer d.untrackProcess(processID)

	out, err := d.Runner.Run(runCtx, scope, runner.Input{ScopeID: scope.ID, Event: event, ThreadID: threadID, RunnerSessionID: runnerSessionID, NewSession: runnerSessionID == "", GenerateThreadTitle: generateThreadTitle, OnSessionAccepted: func(acceptedCtx context.Context, sessionID string) error {
		return d.Store.SaveRunnerSessionID(context.WithoutCancel(acceptedCtx), scope.ID, event.ChannelID, threadID, sessionID)
	}, OnToolEvent: d.toolEventHandler(scope, sender, processes, processID, event, threadID), OnLimitEvent: d.limitEventHandler(scope, sender, processes, processID, event, threadID)})
	if err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cleanupCancel()
		status := "error"
		var remoteCancellation *runner.RemoteCancellationError
		if (runCtx.Err() != nil || ctx.Err() != nil) && !errors.As(err, &remoteCancellation) {
			status = "killed"
		}
		if processes != nil {
			_ = processes.UpdateProcess(cleanupCtx, processID, threads.UpdateProcessRequest{Status: status})
			_ = processes.UpdateMessageProcessStatus(cleanupCtx, event.Message.ID, threads.UpdateMessageProcessStatusRequest{ProcessID: processID, Status: status, ErrorText: err.Error()})
		}
		if status == "killed" {
			return nil
		}
		if sendErr := sender.SendMessage(cleanupCtx, event.ChannelID, threads.SendMessageRequest{Content: runnerErrorMessage(err), ThreadID: threadID, MessageType: "progress", Metadata: map[string]any{"source": "threads-agent-bridge", "kind": "error", "severity": "error", "scope_id": scope.ID, "process_id": processID}}); sendErr != nil {
			if d.Logger != nil {
				d.Logger.Warn("send runner error", "scope", scope.ID, "error", sendErr)
			}
		}
		return err
	}
	if out.RunnerSessionID != "" && out.RunnerSessionID != runnerSessionID {
		if err := d.Store.SaveRunnerSessionID(ctx, scope.ID, event.ChannelID, threadID, out.RunnerSessionID); err != nil {
			return err
		}
	}
	if generateThreadTitle {
		d.setGeneratedThreadTitle(context.WithoutCancel(ctx), scope, sender, threadID, out.ThreadTitle)
	}
	if len(out.Reactions) > 0 {
		if reactor, ok := sender.(Reactor); ok {
			for _, reaction := range out.Reactions {
				if err := reactor.AddReaction(ctx, reaction.MessageID, reaction.Emoji); err != nil {
					if processes != nil {
						_ = processes.UpdateProcess(context.WithoutCancel(ctx), processID, threads.UpdateProcessRequest{Status: "error"})
						_ = processes.UpdateMessageProcessStatus(context.WithoutCancel(ctx), event.Message.ID, threads.UpdateMessageProcessStatusRequest{ProcessID: processID, Status: "error", ErrorText: err.Error()})
					}
					return err
				}
			}
		} else if d.Logger != nil {
			d.Logger.Warn("structured reactions requested but sender cannot react", "scope", scope.ID)
		}
	}
	if out.Text != "" {
		if processes != nil {
			_ = processes.RecordProcessActivity(context.WithoutCancel(ctx), processID, threads.ProcessActivityRequest{Type: "reply"})
		}
		if err := sender.SendMessage(ctx, event.ChannelID, threads.SendMessageRequest{Content: out.Text, ThreadID: threadID, MessageType: "response", Metadata: map[string]any{"source": "threads-agent-bridge", "kind": "final", "scope_id": scope.ID, "runner_session_id": out.RunnerSessionID, "process_id": processID}}); err != nil {
			if processes != nil {
				_ = processes.UpdateProcess(context.WithoutCancel(ctx), processID, threads.UpdateProcessRequest{Status: "error"})
				_ = processes.UpdateMessageProcessStatus(context.WithoutCancel(ctx), event.Message.ID, threads.UpdateMessageProcessStatusRequest{ProcessID: processID, Status: "error", ErrorText: err.Error()})
			}
			return err
		}
	}
	if processes != nil {
		_ = processes.UpdateProcess(context.WithoutCancel(ctx), processID, threads.UpdateProcessRequest{Status: "done"})
		_ = processes.UpdateMessageProcessStatus(context.WithoutCancel(ctx), event.Message.ID, threads.UpdateMessageProcessStatusRequest{ProcessID: processID, Status: "done"})
	}
	return nil
}

func (d *Daemon) warnNonFatalProcessError(scope config.Scope, operation string, err error) {
	if err != nil && d.Logger != nil {
		d.Logger.Warn("non-fatal process api error", "scope", scope.ID, "operation", operation, "error", err)
	}
}

func (d *Daemon) setGeneratedThreadTitle(ctx context.Context, scope config.Scope, sender Sender, threadID, value string) {
	title, ok := normalizeThreadTitle(value)
	if !ok {
		if strings.TrimSpace(value) != "" && d.Logger != nil {
			d.Logger.Warn("ignored invalid generated thread title", "scope", scope.ID, "thread_id", threadID)
		}
		return
	}
	titler, ok := sender.(ThreadTitler)
	if !ok {
		if d.Logger != nil {
			d.Logger.Warn("generated thread title requested but sender cannot set titles", "scope", scope.ID, "thread_id", threadID)
		}
		return
	}
	result, err := titler.SetThreadTitle(ctx, threadID, threads.SetThreadTitleRequest{Title: title, IfUnset: true})
	if err != nil {
		if d.Logger != nil {
			d.Logger.Warn("set generated thread title", "scope", scope.ID, "thread_id", threadID, "error", err)
		}
		return
	}
	if d.Logger != nil {
		d.Logger.Debug("generated thread title handled", "scope", scope.ID, "thread_id", threadID, "applied", result.Applied)
	}
}

func normalizeThreadTitle(value string) (string, bool) {
	title := strings.Join(strings.Fields(value), " ")
	return title, title != "" && utf8.RuneCountInString(title) <= 120
}

func runnerErrorMessage(err error) string {
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		msg = "unknown runner error"
	}
	const maxLen = 2000
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "…"
	}
	return fmt.Sprintf("⚠️ Agent error: %s", msg)
}

func (d *Daemon) limitEventHandler(scope config.Scope, sender Sender, processes ProcessClient, processID string, event threads.Event, threadID string) runner.LimitEventHandler {
	return func(ctx context.Context, limitEvent runner.LimitEvent) error {
		if processes != nil {
			_ = processes.RecordProcessActivity(context.WithoutCancel(ctx), processID, threads.ProcessActivityRequest{Type: "status"})
		}
		metadata := map[string]any{
			"source":     "threads-agent-bridge",
			"kind":       "runner_limit",
			"scope_id":   scope.ID,
			"trigger_id": event.Message.ID,
			"provider":   limitEvent.Source,
			"severity":   limitEvent.Severity,
			"process_id": processID,
		}
		for key, value := range limitEvent.Metadata {
			metadata[key] = value
		}
		err := sender.SendMessage(ctx, event.ChannelID, threads.SendMessageRequest{Content: formatLimitEvent(limitEvent), ThreadID: threadID, MessageType: "progress", Metadata: metadata})
		if err != nil && d.Logger != nil {
			d.Logger.Warn("send limit event", "scope", scope.ID, "provider", limitEvent.Source, "error", err)
		}
		return err
	}
}

func formatLimitEvent(event runner.LimitEvent) string {
	prefix := "ℹ️"
	switch event.Severity {
	case "warning":
		prefix = "⚠️"
	case "error":
		prefix = "🚫"
	}
	return strings.TrimSpace(prefix + " " + event.Message)
}

func (d *Daemon) toolEventHandler(scope config.Scope, sender Sender, processes ProcessClient, processID string, event threads.Event, threadID string) runner.ToolEventHandler {
	active := map[string]runner.ToolEvent{}
	return func(ctx context.Context, toolEvent runner.ToolEvent) error {
		if toolEvent.ID != "" {
			if toolEvent.Status == runner.ToolEventStarted {
				active[toolEvent.ID] = toolEvent
			} else if previous, ok := active[toolEvent.ID]; ok {
				if toolEvent.Name == "" {
					toolEvent.Name = previous.Name
				}
				if toolEvent.Input == nil {
					toolEvent.Input = previous.Input
				}
			}
		}
		if processes != nil && toolEvent.Status == runner.ToolEventStarted {
			if err := processes.RecordProcessActivity(context.WithoutCancel(ctx), processID, threads.ProcessActivityRequest{Type: "tool_call"}); err != nil && d.Logger != nil {
				d.Logger.Warn("record tool activity", "scope", scope.ID, "tool", toolEvent.Name, "error", err)
			}
		}
		content := formatToolEvent(toolEvent)
		messageType := "progress"
		if toolEvent.Status == runner.ToolEventCompleted {
			messageType = "tool_output"
		}
		err := sender.SendMessage(ctx, event.ChannelID, threads.SendMessageRequest{
			Content:     content,
			ThreadID:    threadID,
			MessageType: messageType,
			Metadata: map[string]any{
				"source":     "threads-agent-bridge",
				"kind":       "tool_call",
				"scope_id":   scope.ID,
				"trigger_id": event.Message.ID,
				"tool":       toolEvent.Name,
				"tool_id":    toolEvent.ID,
				"status":     string(toolEvent.Status),
				"error":      toolEvent.Error,
				"input":      toolEvent.Input,
				"output":     toolEvent.Output,
				"process_id": processID,
			},
		})
		if err != nil && d.Logger != nil {
			d.Logger.Warn("send tool event", "scope", scope.ID, "tool", toolEvent.Name, "status", toolEvent.Status, "error", err)
		}
		return err
	}
}

func formatToolEvent(event runner.ToolEvent) string {
	prefix := "↳"
	if event.Status == runner.ToolEventCompleted {
		if event.Error {
			prefix = "✗"
		} else {
			prefix = "✓"
		}
	}
	title := strings.TrimSpace(prefix + " " + event.Name)
	if command := toolCommand(event.Input); command != "" {
		title += ": `" + inlineCode(command) + "`"
	}
	if event.Status == runner.ToolEventCompleted {
		return title + formatToolOutput(event.Output)
	}
	return title + formatToolInput(event.Input)
}

func toolCommand(input any) string {
	switch v := input.(type) {
	case map[string]any:
		if command, ok := v["command"].(string); ok {
			return firstLine(command)
		}
	case string:
		return firstLine(v)
	}
	return ""
}

func firstLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func inlineCode(value string) string {
	return strings.ReplaceAll(value, "`", "\\`")
}

func formatToolInput(input any) string {
	return formatToolPayload("Input", input)
}

func formatToolOutput(output any) string {
	return formatToolPayload("Output", output)
}

func formatToolPayload(label string, payload any) string {
	if payload == nil {
		return ""
	}
	if text, ok := payload.(string); ok {
		text = strings.TrimSpace(text)
		if text == "" {
			return ""
		}
		return "\n\n" + label + ":\n\n" + indentCode(text)
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}
	return "\n\n" + label + ":\n\n" + indentCode(string(data))
}

func indentCode(value string) string {
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		lines[i] = "    " + line
	}
	return strings.Join(lines, "\n")
}

func rootThreadID(event threads.Event) string {
	if event.ThreadID != "" {
		return event.ThreadID
	}
	return event.Message.ID
}

func isMessageEvent(event threads.Event) bool {
	return event.Type == "message.created" || event.Type == "message.invoked" || event.Type == "message"
}

func isNewRootMessage(event threads.Event, threadID string) bool {
	if event.Emoji != "" || event.Message.ID == "" || event.Message.ID != threadID {
		return false
	}
	return event.Type == "message.created" || event.Type == "message"
}

func isReactionEvent(event threads.Event) bool {
	return event.Type == "reaction_added" || event.Type == "reaction.created" || event.Type == "reaction"
}

func reactionTargetsScope(scope config.Scope, channelID, threadID, messageSenderID string) bool {
	if scope.Threads.UserID == "" || messageSenderID != scope.Threads.UserID {
		return false
	}
	return scope.Matches(channelID, threadID)
}

func (d *Daemon) trackProcess(processID string, cancel context.CancelFunc) {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()
	if d.active == nil {
		d.active = map[string]context.CancelFunc{}
	}
	d.active[processID] = cancel
}

func (d *Daemon) untrackProcess(processID string) {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()
	delete(d.active, processID)
}

func (d *Daemon) HandleProcessKill(event threads.Event) {
	if event.ProcessID == "" {
		return
	}
	d.activeMu.Lock()
	cancel := d.active[event.ProcessID]
	d.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func isProcessKillEvent(event threads.Event) bool {
	return event.Type == "process_kill" || event.Type == "process.kill"
}

func processID(scopeID, messageID string) string {
	return sanitizeID(fmt.Sprintf("bridge-%s-%s-%d", scopeID, messageID, time.Now().UnixNano()))
}

func sanitizeID(value string) string {
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func (d *Daemon) maintainPresence(ctx context.Context, scope config.Scope, presence PresenceMaintainer) {
	attempt := 0
	for ctx.Err() == nil {
		if err := presence.MaintainPresence(ctx); err != nil && ctx.Err() == nil && d.Logger != nil {
			d.Logger.Warn("presence websocket disconnected", "scope", scope.ID, "error", err)
		}
		attempt++
		select {
		case <-time.After(SleepBackoff(attempt)):
		case <-ctx.Done():
			return
		}
	}
}

func defaultClients(scope config.Scope, token string) (EventSource, Sender, ProcessClient) {
	client := threads.Client{BaseURL: scope.Threads.BaseURL, Token: token}
	return client, client, client
}

func SleepBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

type rootGate struct {
	token chan struct{}
	users int
}

func (d *Daemon) lockRoot(ctx context.Context, key string) (func(), error) {
	d.rootsMu.Lock()
	if d.roots == nil {
		d.roots = map[string]*rootGate{}
	}
	gate := d.roots[key]
	if gate == nil {
		gate = &rootGate{token: make(chan struct{}, 1)}
		d.roots[key] = gate
	}
	gate.users++
	d.rootsMu.Unlock()
	releaseRef := func() {
		d.rootsMu.Lock()
		gate.users--
		if gate.users == 0 {
			delete(d.roots, key)
		}
		d.rootsMu.Unlock()
	}
	select {
	case gate.token <- struct{}{}:
		return func() { <-gate.token; releaseRef() }, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}
