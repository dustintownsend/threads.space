package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/config"
	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/t3"
)

type RemoteCancellationError struct{ Err error }

func (e *RemoteCancellationError) Error() string {
	return "remote T3 interrupt failed: " + e.Err.Error()
}
func (e *RemoteCancellationError) Unwrap() error { return e.Err }

type t3Caller interface {
	Call(context.Context, string, any, any) error
}

func runT3(ctx context.Context, scope config.Scope, input Input) (Output, error) {
	client, err := t3.New(ctx, scope.Runner.T3.BaseURL, scope.Runner.T3.CredentialsFile)
	if err != nil {
		return Output{}, err
	}
	defer client.Close()
	return runT3Client(ctx, client, scope, input)
}

func t3RequestID(input Input, operation string) string {
	eventID := input.Event.ID
	if eventID == "" {
		eventID = input.Event.Cursor
	}
	if eventID == "" {
		eventID = input.Event.Message.ID + ":" + input.Event.Type + ":" + input.Event.Emoji
	}
	hash := sha256.Sum256([]byte(input.ScopeID + "\x00" + input.Event.ChannelID + "\x00" + input.ThreadID + "\x00" + eventID + "\x00" + operation))
	return "threads-" + hex.EncodeToString(hash[:])
}

func runT3Client(ctx context.Context, client t3Caller, scope config.Scope, input Input) (out Output, err error) {
	settings := scope.Runner.T3
	threadID := input.RunnerSessionID
	if threadID != "" {
		var current t3.ReadResult
		if err := client.Call(ctx, "t3_thread_read", map[string]any{"threadId": threadID, "limit": 1}, &current); err != nil {
			return out, err
		}
		if err := validateT3Thread(current, threadID, settings.ProjectID); err != nil {
			return out, err
		}
		if current.Thread.ActiveRunID != "" || current.Thread.PendingRequestCount > 0 {
			return out, fmt.Errorf("T3 thread already has active or queued work; finish it in T3 before sending a reply")
		}
	}
	var accepted t3.Acceptance
	args := map[string]any{"message": buildT3Prompt(scope, input), "clientRequestId": t3RequestID(input, "turn")}
	tool := "t3_thread_send"
	if threadID == "" {
		tool = "t3_thread_launch"
		title := strings.Join(strings.Fields(input.Event.Message.Content), " ")
		if len([]rune(title)) > 100 {
			title = string([]rune(title)[:100])
		}
		if title == "" {
			title = "Threads conversation"
		}
		args["title"] = title
		args["projectId"] = settings.ProjectID
		args["modelSelection"] = settings.ModelSelection
		args["runtimeMode"] = settings.RuntimeMode
		args["interactionMode"] = settings.InteractionMode
		args["workspaceStrategy"] = map[string]string{"type": "root"}
	} else {
		args["threadId"] = threadID
		args["mode"] = "queue"
	}
	if err := client.Call(ctx, tool, args, &accepted); err != nil {
		return out, err
	}
	if accepted.ThreadID == "" || (threadID != "" && accepted.ThreadID != threadID) {
		return out, fmt.Errorf("T3 acceptance did not match requested thread")
	}
	threadID = accepted.ThreadID
	out.RunnerSessionID = threadID
	finished := false
	defer func() {
		if finished || accepted.RunID == "" {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var stopped t3.Acceptance
		stopErr := client.Call(stopCtx, "t3_thread_interrupt", map[string]any{"threadId": threadID, "runId": accepted.RunID, "clientRequestId": t3RequestID(input, "interrupt")}, &stopped)
		if stopErr == nil && (stopped.ThreadID != threadID || (stopped.RunID != "" && stopped.RunID != accepted.RunID)) {
			stopErr = fmt.Errorf("T3 interruption acknowledgement did not match the targeted run")
		}
		if stopErr != nil {
			err = errors.Join(err, &RemoteCancellationError{Err: stopErr})
		}
	}()
	if tool == "t3_thread_launch" && accepted.ProjectID != settings.ProjectID {
		return out, fmt.Errorf("T3 acceptance did not identify the configured project")
	}
	if input.OnSessionAccepted != nil {
		if err := input.OnSessionAccepted(ctx, threadID); err != nil {
			return out, err
		}
	}
	if accepted.RunID == "" {
		return out, fmt.Errorf("T3 acceptance did not identify the configured project and exact run")
	}
	seenTools := map[string]ToolEventStatus{}
	resultRunID := ""
	notifiedPending := false
	for {
		page, items, readErr := readT3Activity(ctx, client, threadID, settings.ProjectID)
		if readErr != nil {
			return out, readErr
		}
		if page.Thread.PendingRequestCount > 0 && !notifiedPending && input.OnLimitEvent != nil {
			message := "T3 needs your input or approval. Open this conversation in T3 to continue."
			if page.Thread.Link != "" {
				message += " " + page.Thread.Link
			}
			_ = input.OnLimitEvent(ctx, LimitEvent{Source: "t3", Message: message, Severity: "warning", Metadata: map[string]any{"t3_thread_id": threadID, "t3_link": page.Thread.Link}})
			notifiedPending = true
		}
		for _, item := range items {
			if (item.RunID != accepted.RunID && item.RunID != resultRunID) || item.SourceThreadID != threadID || item.Visibility != "local" {
				continue
			}
			if event, ok := t3ToolEvent(item); ok && input.OnToolEvent != nil {
				previous := seenTools[item.ItemID]
				if previous == "" {
					start := event
					start.Status = ToolEventStarted
					start.Output = nil
					_ = input.OnToolEvent(ctx, start)
				}
				if event.Status == ToolEventCompleted && previous != ToolEventCompleted {
					_ = input.OnToolEvent(ctx, event)
				}
				seenTools[item.ItemID] = event.Status
			}
		}
		status := ""
		selectedRunID := accepted.RunID
		if resultRunID != "" {
			selectedRunID = resultRunID
		}
		for _, run := range page.RecentRuns {
			if run.RunID == selectedRunID {
				status = run.Status
				break
			}
		}
		if status == "" {
			return out, fmt.Errorf("T3 exact run is missing from recent runs; refusing to infer its result")
		}
		switch status {
		case "completed":
			if resultRunID == "" {
				break
			}
			finished = true
			final, found := finalT3Item(items, threadID, selectedRunID)
			if !found {
				return out, fmt.Errorf("T3 run completed without an assistant final message")
			}
			text, err := fullT3Text(ctx, client, threadID, final)
			if err != nil {
				return out, err
			}
			out.Text = text
			if scope.Runner.Structured {
				out = parseStructuredOutput(out, input.Event.Message.ID)
			}
			return out, nil
		case "failed", "cancelled", "interrupted", "rolled_back":
			finished = true
			return out, fmt.Errorf("T3 run %s ended with status %s", accepted.RunID, status)
		}
		var waited struct {
			ThreadID  string `json:"threadId"`
			RunID     string `json:"runId"`
			TimedOut  bool   `json:"timedOut"`
			ResultRun *struct {
				RunID  string `json:"runId"`
				Status string `json:"status"`
			} `json:"resultRun"`
		}
		if err := client.Call(ctx, "t3_thread_wait", map[string]any{"threadId": threadID, "runId": accepted.RunID, "timeoutMs": 1000, "untilDelegatedWorkSettled": true}, &waited); err != nil {
			return out, err
		}
		if waited.ThreadID != threadID || waited.RunID != accepted.RunID {
			return out, fmt.Errorf("T3 wait returned a different thread or run")
		}
		if !waited.TimedOut {
			resultRunID = accepted.RunID
			if waited.ResultRun != nil {
				if waited.ResultRun.RunID == "" {
					return out, fmt.Errorf("T3 settled result did not identify a run")
				}
				resultRunID = waited.ResultRun.RunID
			}
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
	}
}

func validateT3Thread(page t3.ReadResult, threadID, projectID string) error {
	if page.Thread.ThreadID != threadID || page.Thread.ProjectID != projectID || page.Thread.ParentThreadID != nil {
		return fmt.Errorf("T3 thread is not a root in the configured project")
	}
	return nil
}

// Re-read mutable timeline items each poll; positions alone miss running-to-completed updates.
func readT3Activity(ctx context.Context, client t3Caller, threadID, projectID string) (t3.ReadResult, []t3.Item, error) {
	args := map[string]any{"threadId": threadID, "view": "activity", "limit": 100, "runLimit": 50, "maxCharsPerItem": 50000}
	var initial t3.ReadResult
	var items []t3.Item
	previous := -1
	for pageNumber := 0; pageNumber < 1000; pageNumber++ {
		var page t3.ReadResult
		if err := client.Call(ctx, "t3_thread_read", args, &page); err != nil {
			return initial, nil, err
		}
		if err := validateT3Thread(page, threadID, projectID); err != nil {
			return initial, nil, err
		}
		if pageNumber == 0 {
			initial = page
		}
		items = append(items, page.Items...)
		if !page.HasMore {
			return initial, items, nil
		}
		if page.NextPosition == nil || *page.NextPosition <= previous {
			return initial, nil, fmt.Errorf("T3 activity pagination did not advance")
		}
		previous = *page.NextPosition
		args["afterPosition"] = previous
	}
	return initial, nil, fmt.Errorf("T3 activity exceeds supported page limit")
}

func finalT3Item(items []t3.Item, threadID, runID string) (t3.Item, bool) {
	var final t3.Item
	found := false
	for _, item := range items {
		if item.RunID == runID && item.SourceThreadID == threadID && item.Visibility == "local" && item.CreatedBy == "agent" && item.Type == "assistant_message" {
			if !found || item.Position > final.Position {
				final = item
				found = true
			}
		}
	}
	return final, found
}

func fullT3Text(ctx context.Context, client t3Caller, threadID string, item t3.Item) (string, error) {
	var text strings.Builder
	text.WriteString(item.Text)
	previous := 0
	for item.TextTruncated {
		if item.NextTextOffset == nil || *item.NextTextOffset <= previous {
			return "", fmt.Errorf("T3 text pagination did not advance")
		}
		previous = *item.NextTextOffset
		var page t3.ReadResult
		if err := client.Call(ctx, "t3_thread_read", map[string]any{"threadId": threadID, "itemId": item.ItemID, "textOffset": previous, "maxCharsPerItem": 50000}, &page); err != nil {
			return "", err
		}
		if len(page.Items) != 1 || page.Items[0].ItemID != item.ItemID || page.Items[0].RunID != item.RunID || page.Items[0].SourceThreadID != threadID {
			return "", fmt.Errorf("T3 text chunk did not match requested item/run")
		}
		item = page.Items[0]
		text.WriteString(item.Text)
	}
	return text.String(), nil
}

func t3ToolEvent(item t3.Item) (ToolEvent, bool) {
	event := ToolEvent{ID: item.ItemID, Name: item.Title, Status: ToolEventStarted}
	switch item.Type {
	case "command_execution":
		event.Name = "command_execution"
		command, output, _ := strings.Cut(item.Text, "\n")
		event.Input = map[string]any{"command": strings.TrimPrefix(command, "$ ")}
		event.Output = output
	case "file_change", "web_search", "file_search", "mcp_tool_call", "dynamic_tool":
		event.Name = item.Type
		var payload map[string]any
		if json.Unmarshal([]byte(item.Text), &payload) == nil {
			if name := firstString(payload, "toolName", "name"); name != "" {
				event.Name = name
			}
			event.Input = payload["input"]
			event.Output = payload["output"]
		} else {
			event.Input = item.Title
			event.Output = item.Text
		}
	default:
		return ToolEvent{}, false
	}
	switch item.Status {
	case "completed", "failed", "cancelled", "interrupted":
		event.Status = ToolEventCompleted
	}
	event.Error = item.Status == "failed"
	return event, true
}

func buildT3Prompt(scope config.Scope, input Input) string {
	var b strings.Builder
	b.WriteString("You are responding to a user in Threads through the T3 agent bridge. Continue this T3 conversation for replies in the same Threads thread. Write your final answer for the user; the bridge posts it to Threads. Tool activity is relayed automatically.\n")
	if input.Event.Emoji != "" {
		b.WriteString("This message was invoked by an emoji reaction: " + input.Event.Emoji + "\n")
	}
	if scope.Runner.Structured {
		if input.GenerateThreadTitle {
			b.WriteString("Return your final answer as a JSON object: {\"content\":\"answer for user\",\"thread_title\":\"Concise descriptive title\",\"reactions\":[]}. Use 3-8 words for thread_title.\n")
		} else {
			b.WriteString("For the final answer, return plain text or a JSON object {\"content\":\"answer for user\",\"reactions\":[]}.\n")
		}
	}
	b.WriteString(buildUserPrompt(input))
	return b.String()
}
