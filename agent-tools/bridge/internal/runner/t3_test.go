package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/config"
	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/t3"
	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/threads"
)

type fakeT3 struct {
	call func(context.Context, string, map[string]any) (any, error)
}

func (f fakeT3) Call(ctx context.Context, tool string, input, output any) error {
	result, err := f.call(ctx, tool, input.(map[string]any))
	if err != nil {
		return err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, output)
}
func t3Scope() config.Scope {
	return config.Scope{Runner: config.RunnerConfig{Type: "t3", T3: config.T3Config{ProjectID: "project", RuntimeMode: "approval-required", InteractionMode: "default", ModelSelection: config.ModelSelection{InstanceID: "codex", Model: "gpt-6.1-sol"}}}}
}
func t3Input() Input {
	return Input{ScopeID: "scope", ThreadID: "root", Event: threads.Event{ID: "event", ChannelID: "channel", Message: threads.Message{ID: "root", Content: "hello"}}}
}
func t3Page(status string, items ...t3.Item) any {
	return map[string]any{"thread": map[string]any{"threadId": "thread", "projectId": "project"}, "recentRuns": []map[string]any{{"runId": "run", "status": status}}, "items": items}
}
func t3Item(id, kind, text string, position int) t3.Item {
	return t3.Item{ItemID: id, RunID: "run", SourceThreadID: "thread", Visibility: "local", CreatedBy: "agent", Type: kind, Text: text, Position: position, Status: "completed"}
}

func TestT3SavesAcceptanceBeforeReadingAndReturnsOnlyExactFinal(t *testing.T) {
	saved := false
	input := t3Input()
	input.OnSessionAccepted = func(_ context.Context, id string) error { saved = id == "thread"; return nil }
	var launchID string
	client := fakeT3{call: func(ctx context.Context, tool string, args map[string]any) (any, error) {
		switch tool {
		case "t3_thread_launch":
			launchID = args["clientRequestId"].(string)
			if args["projectId"] != "project" || args["runtimeMode"] != "approval-required" {
				t.Fatalf("launch: %v", args)
			}
			prompt := args["message"].(string)
			if strings.Contains(prompt, "THREADS_TOKEN") || strings.Contains(prompt, "environment variables") {
				t.Fatalf("remote prompt promises local credentials: %s", prompt)
			}
			return map[string]any{"threadId": "thread", "projectId": "project", "runId": "run"}, nil
		case "t3_thread_wait":
			return map[string]any{"threadId": "thread", "runId": "run"}, nil
		case "t3_thread_read":
			if !saved {
				t.Fatal("read before durable acceptance")
			}
			old := t3Item("old", "assistant_message", "old answer", 0)
			old.RunID = "old-run"
			foreign := t3Item("foreign", "assistant_message", "child answer", 99)
			foreign.SourceThreadID = "child"
			return t3Page("completed", old, t3Item("comment", "assistant_message", "working...", 1), t3Item("final", "assistant_message", "final answer", 2), foreign), nil
		default:
			t.Fatalf("unexpected tool %s", tool)
			return nil, nil
		}
	}}
	out, err := runT3Client(context.Background(), client, t3Scope(), input)
	if err != nil || out.Text != "final answer" || out.RunnerSessionID != "thread" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if launchID != t3RequestID(input, "turn") || launchID == t3RequestID(input, "interrupt") {
		t.Fatal("unstable request identity")
	}
}

func TestT3MutableToolActivityAndFullFinalText(t *testing.T) {
	readCount := 0
	var events []ToolEvent
	input := t3Input()
	input.OnToolEvent = func(_ context.Context, event ToolEvent) error { events = append(events, event); return nil }
	client := fakeT3{call: func(ctx context.Context, tool string, args map[string]any) (any, error) {
		switch tool {
		case "t3_thread_launch":
			return map[string]any{"threadId": "thread", "projectId": "project", "runId": "run"}, nil
		case "t3_thread_wait":
			return map[string]any{"threadId": "thread", "runId": "run"}, nil
		case "t3_thread_read":
			if args["itemId"] != nil {
				if args["textOffset"] != 5 {
					t.Fatalf("offset=%v", args)
				}
				return t3Page("completed", t3Item("final", "assistant_message", "world", 2)), nil
			}
			readCount++
			item := t3Item("tool", "dynamic_tool", `{"toolName":"exec_command","input":{"cmd":"pwd"}}`, 1)
			if readCount == 1 {
				item.Status = "running"
				return t3Page("running", item), nil
			}
			item.Text = `{"toolName":"exec_command","input":{"cmd":"pwd"},"output":"/workspace"}`
			final := t3Item("final", "assistant_message", "hello", 2)
			final.TextTruncated = true
			offset := 5
			final.NextTextOffset = &offset
			return t3Page("completed", item, final), nil
		default:
			t.Fatalf("unexpected %s", tool)
			return nil, nil
		}
	}}
	out, err := runT3Client(context.Background(), client, t3Scope(), input)
	if err != nil || out.Text != "helloworld" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(events) != 2 || events[0].Status != ToolEventStarted || events[1].Status != ToolEventCompleted || events[1].Output != "/workspace" {
		t.Fatalf("events=%+v", events)
	}
}

func TestT3CancellationInterruptsExactRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupted := false
	client := fakeT3{call: func(ctx context.Context, tool string, args map[string]any) (any, error) {
		switch tool {
		case "t3_thread_launch":
			return map[string]any{"threadId": "thread", "projectId": "project", "runId": "run"}, nil
		case "t3_thread_read":
			cancel()
			return nil, ctx.Err()
		case "t3_thread_interrupt":
			interrupted = true
			if ctx.Err() != nil || args["runId"] != "run" || args["threadId"] != "thread" || args["stop"] != nil {
				t.Fatalf("wrong interrupt context/args: %v %v", ctx.Err(), args)
			}
			return map[string]any{"threadId": "thread", "runId": "run"}, nil
		default:
			t.Fatalf("unexpected %s", tool)
			return nil, nil
		}
	}}
	_, err := runT3Client(ctx, client, t3Scope(), t3Input())
	if !errors.Is(err, context.Canceled) || !interrupted {
		t.Fatalf("err=%v interrupted=%v", err, interrupted)
	}
}

func TestT3ResumeRejectsDifferentProjectBeforeSending(t *testing.T) {
	input := t3Input()
	input.RunnerSessionID = "thread"
	client := fakeT3{call: func(ctx context.Context, tool string, args map[string]any) (any, error) {
		if tool != "t3_thread_read" {
			t.Fatalf("unexpected dispatch %s", tool)
		}
		return map[string]any{"thread": map[string]any{"threadId": "thread", "projectId": "other"}}, nil
	}}
	if _, err := runT3Client(context.Background(), client, t3Scope(), input); err == nil {
		t.Fatal("accepted different project")
	}
}

func TestT3MissingExactRunFailsWithoutPostingOldAnswer(t *testing.T) {
	interrupted := false
	client := fakeT3{call: func(ctx context.Context, tool string, args map[string]any) (any, error) {
		switch tool {
		case "t3_thread_launch":
			return map[string]any{"threadId": "thread", "projectId": "project", "runId": "missing"}, nil
		case "t3_thread_read":
			return t3Page("completed", t3Item("old", "assistant_message", "old answer", 1)), nil
		case "t3_thread_interrupt":
			interrupted = true
			return map[string]any{}, nil
		default:
			t.Fatalf("unexpected %s", tool)
			return nil, nil
		}
	}}
	out, err := runT3Client(context.Background(), client, t3Scope(), t3Input())
	if err == nil || out.Text != "" || !interrupted {
		t.Fatalf("out=%+v err=%v interrupt=%v", out, err, interrupted)
	}
}

func TestT3WaitsForDelegatedResultAndNotifiesPendingInputOnce(t *testing.T) {
	waits := 0
	var notices []LimitEvent
	input := t3Input()
	input.OnLimitEvent = func(_ context.Context, event LimitEvent) error { notices = append(notices, event); return nil }
	client := fakeT3{call: func(ctx context.Context, tool string, args map[string]any) (any, error) {
		switch tool {
		case "t3_thread_launch":
			return map[string]any{"threadId": "thread", "projectId": "project", "runId": "run"}, nil
		case "t3_thread_read":
			items := []t3.Item{t3Item("parent", "assistant_message", "waiting for delegated work", 1)}
			runs := []map[string]any{{"runId": "run", "status": "completed"}}
			pending := 1
			if waits >= 2 {
				pending = 0
				final := t3Item("settled-final", "assistant_message", "finished result", 2)
				final.RunID = "result-run"
				items = append(items, final)
				runs = append(runs, map[string]any{"runId": "result-run", "status": "completed"})
			}
			return map[string]any{"thread": map[string]any{"threadId": "thread", "projectId": "project", "link": "t3://thread/thread", "pendingRequestCount": pending}, "recentRuns": runs, "items": items}, nil
		case "t3_thread_wait":
			if args["untilDelegatedWorkSettled"] != true || args["runId"] != "run" {
				t.Fatalf("wait=%v", args)
			}
			waits++
			if waits == 1 {
				return map[string]any{"threadId": "thread", "runId": "run", "timedOut": true}, nil
			}
			return map[string]any{"threadId": "thread", "runId": "run", "timedOut": false, "resultRun": map[string]any{"runId": "result-run", "status": "completed"}}, nil
		default:
			t.Fatalf("unexpected %s", tool)
			return nil, nil
		}
	}}
	out, err := runT3Client(context.Background(), client, t3Scope(), input)
	if err != nil || out.Text != "finished result" || waits != 2 {
		t.Fatalf("out=%+v err=%v waits=%d", out, err, waits)
	}
	if len(notices) != 1 || !strings.Contains(notices[0].Message, "t3://thread/thread") {
		t.Fatalf("notices=%+v", notices)
	}
}
