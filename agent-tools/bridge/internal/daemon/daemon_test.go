package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/config"
	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/runner"
	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/store"
	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/threads"
)

type fakeRunner struct {
	calls  int
	inputs []runner.Input
}

func (f *fakeRunner) Run(ctx context.Context, scope config.Scope, in runner.Input) (runner.Output, error) {
	f.calls++
	f.inputs = append(f.inputs, in)
	if in.RunnerSessionID != "" {
		return runner.Output{Text: "reply to " + in.Event.Message.Content, RunnerSessionID: in.RunnerSessionID}, nil
	}
	return runner.Output{Text: "reply to " + in.Event.Message.Content, RunnerSessionID: "runner-s1"}, nil
}

type fakeSender struct {
	afterSend        func()
	sent             []threads.SendMessageRequest
	titleMessageIDs  []string
	titles           []threads.SetThreadTitleRequest
	created          []threads.CreateProcessRequest
	updated          []threads.UpdateProcessRequest
	activities       []threads.ProcessActivityRequest
	messageStatuses  []threads.UpdateMessageProcessStatusRequest
	reactions        []runner.Reaction
	createProcessErr error
	messageStatusErr error
	titleErr         error
}

func (f *fakeSender) SendMessage(ctx context.Context, channelID string, req threads.SendMessageRequest) error {
	f.sent = append(f.sent, req)
	if f.afterSend != nil {
		f.afterSend()
	}
	return nil
}

func (f *fakeSender) AddReaction(ctx context.Context, messageID, emoji string) error {
	f.reactions = append(f.reactions, runner.Reaction{MessageID: messageID, Emoji: emoji})
	return nil
}

func (f *fakeSender) SetThreadTitle(ctx context.Context, messageID string, req threads.SetThreadTitleRequest) (threads.SetThreadTitleResponse, error) {
	f.titleMessageIDs = append(f.titleMessageIDs, messageID)
	f.titles = append(f.titles, req)
	if f.titleErr != nil {
		return threads.SetThreadTitleResponse{}, f.titleErr
	}
	return threads.SetThreadTitleResponse{OK: true, Applied: true, ThreadID: messageID, ThreadTitle: req.Title, ThreadTitleUpdatedAt: 123}, nil
}

func (f *fakeSender) CreateProcess(ctx context.Context, req threads.CreateProcessRequest) (threads.CreateProcessResponse, error) {
	f.created = append(f.created, req)
	if f.createProcessErr != nil {
		return threads.CreateProcessResponse{}, f.createProcessErr
	}
	return threads.CreateProcessResponse{ID: req.ID, Status: "running"}, nil
}

func (f *fakeSender) UpdateProcess(ctx context.Context, processID string, req threads.UpdateProcessRequest) error {
	f.updated = append(f.updated, req)
	return nil
}

func (f *fakeSender) RecordProcessActivity(ctx context.Context, processID string, req threads.ProcessActivityRequest) error {
	f.activities = append(f.activities, req)
	return nil
}

func (f *fakeSender) UpdateMessageProcessStatus(ctx context.Context, messageID string, req threads.UpdateMessageProcessStatusRequest) error {
	f.messageStatuses = append(f.messageStatuses, req)
	return f.messageStatusErr
}

func matchAllScope(id string) config.Scope {
	return config.Scope{ID: id, Match: config.MatchConfig{ChannelIDs: []string{"*"}, ThreadIDs: []string{"*"}}}
}

func TestHandleEventIgnoresOwnMessages(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &fakeRunner{}
	s := &fakeSender{}
	d := Daemon{Store: st, Runner: r}
	scope := config.Scope{ID: "s1", Threads: config.ThreadsConfig{UserID: "bot1"}}
	event := threads.Event{ID: "e1", Type: "message", ChannelID: "c1", Message: threads.Message{ID: "m1", Content: "hi", SenderID: "bot1"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 || len(s.sent) != 0 {
		t.Fatalf("calls=%d sent=%d", r.calls, len(s.sent))
	}
}

func TestHandleEventRoutesAndDedupes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &fakeRunner{}
	s := &fakeSender{}
	d := Daemon{Store: st, Runner: r}
	scope := matchAllScope("s1")
	event := threads.Event{ID: "e1", Cursor: "cur1", Type: "message.created", ChannelID: "c1", ThreadID: "t1", Message: threads.Message{ID: "m1", Content: "hi"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 || len(s.sent) != 1 {
		t.Fatalf("calls=%d sent=%d", r.calls, len(s.sent))
	}
	if s.sent[0].ThreadID != "t1" || s.sent[0].Content != "reply to hi" || s.sent[0].MessageType != "response" {
		t.Fatalf("bad send: %+v", s.sent[0])
	}
	if s.sent[0].Metadata["kind"] != "final" || s.sent[0].Metadata["scope_id"] != "s1" || s.sent[0].Metadata["runner_session_id"] != "runner-s1" {
		t.Fatalf("bad final metadata: %+v", s.sent[0].Metadata)
	}
	cursor, err := st.Cursor(context.Background(), "s1")
	if err != nil || cursor != "cur1" {
		t.Fatalf("cursor=%q err=%v", cursor, err)
	}
}

type blockingRunner struct {
	started chan struct{}
}

func (r blockingRunner) Run(ctx context.Context, scope config.Scope, in runner.Input) (runner.Output, error) {
	close(r.started)
	<-ctx.Done()
	return runner.Output{}, ctx.Err()
}

type structuredRunner struct{}

func (structuredRunner) Run(ctx context.Context, scope config.Scope, in runner.Input) (runner.Output, error) {
	return runner.Output{Text: "done", RunnerSessionID: "runner-s1", Reactions: []runner.Reaction{{MessageID: in.Event.Message.ID, Emoji: "✅"}}}, nil
}

type generatedTitleRunner struct {
	title string
}

func (r generatedTitleRunner) Run(ctx context.Context, scope config.Scope, in runner.Input) (runner.Output, error) {
	return runner.Output{Text: "done", RunnerSessionID: "runner-s1", ThreadTitle: r.title}, nil
}

type toolEventRunner struct{}

func (toolEventRunner) Run(ctx context.Context, scope config.Scope, in runner.Input) (runner.Output, error) {
	if in.OnToolEvent != nil {
		_ = in.OnToolEvent(ctx, runner.ToolEvent{ID: "tool-1", Name: "Bash", Input: map[string]any{"command": "git status"}, Status: runner.ToolEventStarted})
		_ = in.OnToolEvent(ctx, runner.ToolEvent{ID: "tool-1", Name: "Bash", Output: "clean", Status: runner.ToolEventCompleted})
	}
	return runner.Output{Text: "done", RunnerSessionID: "runner-s1"}, nil
}

type errorRunner struct{}

func (errorRunner) Run(ctx context.Context, scope config.Scope, in runner.Input) (runner.Output, error) {
	return runner.Output{}, errors.New("OpenRouter error: 402 insufficient credits")
}

type limitEventRunner struct{}

func (limitEventRunner) Run(ctx context.Context, scope config.Scope, in runner.Input) (runner.Output, error) {
	if in.OnLimitEvent != nil {
		_ = in.OnLimitEvent(ctx, runner.LimitEvent{Source: "codex", Severity: "warning", Message: "Codex limits; primary 94% used", Metadata: map[string]any{"rate_limits": map[string]any{"primary": map[string]any{"used_percent": 94}}}})
	}
	return runner.Output{Text: "done", RunnerSessionID: "runner-s1"}, nil
}

type callbackRunner struct {
	onRun func()
}

func (r callbackRunner) Run(ctx context.Context, scope config.Scope, in runner.Input) (runner.Output, error) {
	if r.onRun != nil {
		r.onRun()
	}
	return runner.Output{Text: "done", RunnerSessionID: "runner-s1"}, nil
}

type reconnectingEventSource struct {
	mu    sync.Mutex
	calls int
}

func (s *reconnectingEventSource) Events(ctx context.Context, since string) (<-chan threads.Event, <-chan error) {
	events := make(chan threads.Event, 1)
	errs := make(chan error, 1)
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	go func() {
		defer close(events)
		defer close(errs)
		if call == 1 {
			return
		}
		events <- threads.Event{ID: "e1", Type: "message.created", ChannelID: "c1", Message: threads.Message{ID: "m1", Content: "hi"}}
		<-ctx.Done()
	}()
	return events, errs
}

func (s *reconnectingEventSource) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestRunScopeReconnectsWhenEventStreamCloses(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	source := &reconnectingEventSource{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sender := &fakeSender{afterSend: cancel}
	d := Daemon{
		Store:  st,
		Runner: callbackRunner{},
		Clients: func(scope config.Scope, token string) (EventSource, Sender, ProcessClient) {
			return source, sender, nil
		},
	}
	scope := matchAllScope("s1")
	scope.Threads.Token = "test-token"
	done := make(chan error, 1)
	go func() { done <- d.runScope(ctx, scope) }()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("runScope returned before reconnecting: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runScope did not reconnect and process an event")
	}
	if source.Calls() < 2 {
		t.Fatalf("events calls=%d, want reconnect", source.Calls())
	}
	if len(sender.sent) != 1 || sender.sent[0].Content != "done" {
		t.Fatalf("bad sent messages: %+v", sender.sent)
	}
}

func TestHandleEventInvokesOnAnyReactionToScopeBotMessage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &fakeRunner{}
	s := &fakeSender{}
	d := Daemon{Store: st, Runner: r}
	scope := config.Scope{ID: "s1", Threads: config.ThreadsConfig{UserID: "bot-1"}, Match: config.MatchConfig{ChannelIDs: []string{"*"}, ThreadIDs: []string{"*"}}}
	event := threads.Event{ID: "e1", Type: "reaction_added", ChannelID: "c1", Emoji: "👍", ActorID: "user-1", Message: threads.Message{ID: "m1", SenderID: "bot-1"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 || len(s.sent) != 1 {
		t.Fatalf("reaction should invoke runner, calls=%d sent=%d", r.calls, len(s.sent))
	}
	if r.inputs[0].Event.Emoji != "👍" || r.inputs[0].ThreadID != "m1" {
		t.Fatalf("bad reaction input: %+v", r.inputs[0])
	}
}

func TestHandleEventSkipsScopeBotsOwnReaction(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &fakeRunner{}
	s := &fakeSender{}
	d := Daemon{Store: st, Runner: r}
	scope := config.Scope{ID: "s1", Threads: config.ThreadsConfig{UserID: "bot-1"}, Match: config.MatchConfig{ChannelIDs: []string{"*"}, ThreadIDs: []string{"*"}}}
	event := threads.Event{ID: "e1", Type: "reaction_added", ChannelID: "c1", Emoji: "👋", ActorID: "bot-1", Message: threads.Message{ID: "m1", SenderID: "bot-1"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 || len(s.sent) != 0 {
		t.Fatalf("scope bot's own reaction should be ignored, calls=%d sent=%d", r.calls, len(s.sent))
	}
}

func TestHandleEventSkipsReactionToOtherUserMessage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &fakeRunner{}
	s := &fakeSender{}
	d := Daemon{Store: st, Runner: r}
	scope := config.Scope{ID: "s1", Threads: config.ThreadsConfig{UserID: "bot-1"}, Match: config.MatchConfig{ChannelIDs: []string{"*"}, ThreadIDs: []string{"*"}}}
	event := threads.Event{ID: "e1", Type: "reaction_added", ChannelID: "c1", Emoji: "👍", Message: threads.Message{ID: "m1", SenderID: "user-1"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 || len(s.sent) != 0 {
		t.Fatalf("reaction to other user message should be ignored, calls=%d sent=%d", r.calls, len(s.sent))
	}
}

func TestHandleEventSkipsNonMatchingAllowlist(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &fakeRunner{}
	s := &fakeSender{}
	d := Daemon{Store: st, Runner: r}
	scope := config.Scope{ID: "s1", Match: config.MatchConfig{ChannelIDs: []string{"allowed"}, ThreadIDs: []string{"*"}}}
	event := threads.Event{ID: "e1", Type: "message.created", ChannelID: "other", Message: threads.Message{ID: "m1", Content: "hi"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 || len(s.sent) != 0 {
		t.Fatalf("non-matching event should be ignored, calls=%d sent=%d", r.calls, len(s.sent))
	}
}

func TestHandleEventCreatesProcessStatusAndKillCancelsRun(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &fakeSender{}
	started := make(chan struct{})
	d := Daemon{Store: st, Runner: blockingRunner{started: started}}
	scope := matchAllScope("s1")
	event := threads.Event{ID: "e1", Type: "message.created", ChannelID: "c1", Message: threads.Message{ID: "m1", Content: "hi", SenderID: "u1"}}
	done := make(chan error, 1)
	go func() { done <- d.HandleEvent(context.Background(), scope, s, s, event) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	if len(s.created) != 1 || s.created[0].MessageID != "m1" || s.created[0].UserID != "u1" {
		t.Fatalf("bad process create: %+v", s.created)
	}
	if len(s.messageStatuses) != 1 || s.messageStatuses[0].Status != "processing" || s.messageStatuses[0].ProcessID == "" {
		t.Fatalf("bad initial message status: %+v", s.messageStatuses)
	}
	d.HandleProcessKill(threads.Event{Type: "process_kill", ProcessID: s.messageStatuses[0].ProcessID})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner was not cancelled")
	}
	if len(s.updated) == 0 || s.updated[len(s.updated)-1].Status != "killed" {
		t.Fatalf("bad process updates: %+v", s.updated)
	}
	if got := s.messageStatuses[len(s.messageStatuses)-1].Status; got != "killed" {
		t.Fatalf("message status=%q, want killed", got)
	}
}

func TestHandleEventAppliesStructuredReactions(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &fakeSender{}
	d := Daemon{Store: st, Runner: structuredRunner{}}
	scope := matchAllScope("s1")
	event := threads.Event{ID: "e1", Type: "message.created", ChannelID: "c1", Message: threads.Message{ID: "m1", Content: "hi"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if len(s.reactions) != 1 || s.reactions[0].MessageID != "m1" || s.reactions[0].Emoji != "✅" {
		t.Fatalf("bad reactions: %+v", s.reactions)
	}
	if len(s.sent) != 1 || s.sent[0].Content != "done" {
		t.Fatalf("bad final message: %+v", s.sent)
	}
}

func TestHandleEventSetsGeneratedTitleForNewRoot(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &fakeSender{}
	d := Daemon{Store: st, Runner: generatedTitleRunner{title: "  Investigate   reconnects  "}}
	scope := matchAllScope("s1")
	scope.Runner.AutoTitle = true
	event := threads.Event{ID: "e1", Type: "message.created", ChannelID: "c1", Message: threads.Message{ID: "m1", Content: "hi"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if len(s.titles) != 1 || len(s.titleMessageIDs) != 1 || s.titleMessageIDs[0] != "m1" || s.titles[0].Title != "Investigate reconnects" || !s.titles[0].IfUnset {
		t.Fatalf("bad title requests ids=%v requests=%+v", s.titleMessageIDs, s.titles)
	}
	if len(s.sent) != 1 || s.sent[0].Content != "done" {
		t.Fatalf("bad final message: %+v", s.sent)
	}
}

func TestHandleEventTitleFailureDoesNotBlockFinalResponse(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &fakeSender{titleErr: errors.New("title endpoint unavailable")}
	d := Daemon{Store: st, Runner: generatedTitleRunner{title: "Investigate reconnects"}}
	scope := matchAllScope("s1")
	scope.Runner.AutoTitle = true
	event := threads.Event{ID: "e1", Type: "message.created", ChannelID: "c1", Message: threads.Message{ID: "m1", Content: "hi"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if len(s.titles) != 1 || len(s.sent) != 1 || s.sent[0].Content != "done" {
		t.Fatalf("title failure changed final response: titles=%+v sent=%+v", s.titles, s.sent)
	}
}

func TestHandleEventDoesNotGenerateTitleForInvocation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &fakeRunner{}
	s := &fakeSender{}
	d := Daemon{Store: st, Runner: r}
	scope := matchAllScope("s1")
	scope.Runner.AutoTitle = true
	event := threads.Event{ID: "e1", Type: "message.invoked", ChannelID: "c1", Message: threads.Message{ID: "m1", Content: "retry"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if len(r.inputs) != 1 || r.inputs[0].GenerateThreadTitle || len(s.titles) != 0 {
		t.Fatalf("invocation generated title: input=%+v titles=%+v", r.inputs, s.titles)
	}
}

func TestHandleEventDoesNotFailWhenMessageProcessStatusEndpointIsMissing(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &fakeRunner{}
	s := &fakeSender{messageStatusErr: errors.New("threads message process status: status 404: not found")}
	d := Daemon{Store: st, Runner: r}
	scope := matchAllScope("s1")
	event := threads.Event{ID: "e1", Type: "message.created", ChannelID: "c1", Message: threads.Message{ID: "m1", Content: "hi"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 || len(s.sent) != 1 || s.sent[0].Content != "reply to hi" {
		t.Fatalf("runner should still complete, calls=%d sent=%+v", r.calls, s.sent)
	}
}

func TestHandleEventDoesNotFailWhenProcessAPIIsMissing(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &fakeRunner{}
	s := &fakeSender{createProcessErr: errors.New("threads create process: status 404: not found")}
	d := Daemon{Store: st, Runner: r}
	scope := matchAllScope("s1")
	event := threads.Event{ID: "e1", Type: "message.created", ChannelID: "c1", Message: threads.Message{ID: "m1", Content: "hi"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 || len(s.sent) != 1 || s.sent[0].Content != "reply to hi" {
		t.Fatalf("runner should still complete, calls=%d sent=%+v", r.calls, s.sent)
	}
}

func TestHandleEventSurfacesRunnerErrorsInThreads(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &fakeSender{}
	d := Daemon{Store: st, Runner: errorRunner{}}
	scope := matchAllScope("s1")
	event := threads.Event{ID: "e1", Type: "message.created", ChannelID: "c1", Message: threads.Message{ID: "m1", Content: "hi"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err == nil {
		t.Fatal("expected runner error")
	}
	if len(s.sent) != 1 {
		t.Fatalf("sent=%d %+v", len(s.sent), s.sent)
	}
	if s.sent[0].MessageType != "progress" || s.sent[0].Metadata["severity"] != "error" || s.sent[0].ThreadID != "m1" || !strings.Contains(s.sent[0].Content, "OpenRouter error: 402 insufficient credits") {
		t.Fatalf("bad error message: %+v", s.sent[0])
	}
	if len(s.messageStatuses) == 0 || s.messageStatuses[len(s.messageStatuses)-1].Status != "error" || !strings.Contains(s.messageStatuses[len(s.messageStatuses)-1].ErrorText, "OpenRouter error") {
		t.Fatalf("bad message status: %+v", s.messageStatuses)
	}
}

func TestHandleEventStreamsLimitEventsAsStatusMessages(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &fakeSender{}
	d := Daemon{Store: st, Runner: limitEventRunner{}}
	scope := matchAllScope("s1")
	event := threads.Event{ID: "e1", Type: "message.created", ChannelID: "c1", Message: threads.Message{ID: "m1", Content: "hi"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if len(s.sent) != 2 {
		t.Fatalf("sent=%d %+v", len(s.sent), s.sent)
	}
	if s.sent[0].MessageType != "progress" || s.sent[0].Metadata["kind"] != "runner_limit" || s.sent[0].Metadata["provider"] != "codex" || !strings.Contains(s.sent[0].Content, "94% used") {
		t.Fatalf("bad limit message: %+v", s.sent[0])
	}
}

func TestHandleEventStreamsToolEventsAsTriggeredStepMessages(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &fakeSender{}
	d := Daemon{Store: st, Runner: toolEventRunner{}}
	scope := matchAllScope("s1")
	event := threads.Event{ID: "e1", Type: "message.created", ChannelID: "c1", Message: threads.Message{ID: "m1", Content: "hi"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, event); err != nil {
		t.Fatal(err)
	}
	if len(s.sent) != 3 {
		t.Fatalf("sent=%d %+v", len(s.sent), s.sent)
	}
	if s.sent[0].MessageType != "progress" || s.sent[0].Metadata["trigger_id"] != "m1" || s.sent[0].Metadata["tool"] != "Bash" {
		t.Fatalf("bad tool start message: %+v", s.sent[0])
	}
	if !strings.HasPrefix(s.sent[0].Content, "↳ Bash: `git status`") || !strings.Contains(s.sent[0].Content, "Input:\n\n    {") || strings.Contains(s.sent[0].Content, "…") {
		t.Fatalf("tool input should title with command and include full expandable JSON, got %q", s.sent[0].Content)
	}
	if s.sent[1].MessageType != "tool_output" || s.sent[1].Metadata["status"] != string(runner.ToolEventCompleted) || s.sent[1].Metadata["tool"] != "Bash" {
		t.Fatalf("bad tool completed message: %+v", s.sent[1])
	}
	if !strings.HasPrefix(s.sent[1].Content, "✓ Bash: `git status`") || strings.Contains(s.sent[1].Content, "Input:") || !strings.Contains(s.sent[1].Content, "Output:\n\n    clean") {
		t.Fatalf("tool completion should retain command title and show output only, got %q", s.sent[1].Content)
	}
	if s.sent[2].MessageType != "response" || s.sent[2].Content != "done" {
		t.Fatalf("bad final message: %+v", s.sent[2])
	}
}

func TestHandleEventMapsTopLevelMessageToThreadSessionAndRepliesResume(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &fakeRunner{}
	s := &fakeSender{}
	d := Daemon{Store: st, Runner: r}
	scope := matchAllScope("s1")
	scope.Runner.AutoTitle = true

	root := threads.Event{ID: "e-root", Type: "message.created", ChannelID: "c1", Message: threads.Message{ID: "m-root", Content: "start"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, root); err != nil {
		t.Fatal(err)
	}
	reply := threads.Event{ID: "e-reply", Type: "message.created", ChannelID: "c1", ThreadID: "m-root", Message: threads.Message{ID: "m-reply", Content: "continue"}}
	if err := d.HandleEvent(context.Background(), scope, s, s, reply); err != nil {
		t.Fatal(err)
	}

	if r.calls != 2 || len(r.inputs) != 2 || len(s.sent) != 2 {
		t.Fatalf("calls=%d inputs=%d sent=%d", r.calls, len(r.inputs), len(s.sent))
	}
	if !r.inputs[0].NewSession || !r.inputs[0].GenerateThreadTitle || r.inputs[0].ThreadID != "m-root" || r.inputs[0].RunnerSessionID != "" {
		t.Fatalf("bad first input: %+v", r.inputs[0])
	}
	if r.inputs[1].NewSession || r.inputs[1].GenerateThreadTitle || r.inputs[1].ThreadID != "m-root" || r.inputs[1].RunnerSessionID != "runner-s1" {
		t.Fatalf("bad reply input: %+v", r.inputs[1])
	}
	if s.sent[0].ThreadID != "m-root" || s.sent[1].ThreadID != "m-root" {
		t.Fatalf("bad reply targets: %+v", s.sent)
	}
}

type acceptedSessionRunner struct {
	accepted chan struct{}
	finish   chan struct{}
	resumed  chan string
}

func (r acceptedSessionRunner) Run(ctx context.Context, scope config.Scope, in runner.Input) (runner.Output, error) {
	if in.RunnerSessionID != "" {
		r.resumed <- in.RunnerSessionID
		return runner.Output{Text: "continued", RunnerSessionID: in.RunnerSessionID}, nil
	}
	if err := in.OnSessionAccepted(ctx, "t3-thread"); err != nil {
		return runner.Output{}, err
	}
	close(r.accepted)
	<-r.finish
	return runner.Output{}, errors.New("run failed after remote acceptance")
}

func TestRootTurnsSerializeAndKeepAcceptedSessionAfterFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := acceptedSessionRunner{accepted: make(chan struct{}), finish: make(chan struct{}), resumed: make(chan string, 1)}
	d := Daemon{Store: st, Runner: r}
	s := &fakeSender{}
	scope := matchAllScope("scope")
	event := threads.Event{ID: "first", Type: "message.created", ChannelID: "channel", ThreadID: "root", Message: threads.Message{ID: "root", Content: "first"}}
	first := make(chan error, 1)
	go func() { first <- d.HandleEvent(context.Background(), scope, s, nil, event) }()
	<-r.accepted
	session, err := st.RunnerSessionID(context.Background(), "scope", "channel", "root")
	if err != nil || session != "t3-thread" {
		t.Fatalf("acceptance not persisted before completion: %s %v", session, err)
	}
	reply := event
	reply.ID = "second"
	reply.Message.ID = "reply"
	reply.Message.Content = "second"
	second := make(chan error, 1)
	go func() { second <- d.HandleEvent(context.Background(), scope, s, nil, reply) }()
	deadline := time.After(time.Second)
	for {
		d.rootsMu.Lock()
		gate := d.roots["scope\x00channel\x00root"]
		waiting := gate != nil && gate.users == 2
		d.rootsMu.Unlock()
		if waiting {
			break
		}
		select {
		case <-deadline:
			t.Fatal("second turn never waited for the first")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case <-r.resumed:
		t.Fatal("second turn ran before first finished")
	default:
	}
	close(r.finish)
	if err := <-first; err == nil {
		t.Fatal("first failure was hidden")
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if resumed := <-r.resumed; resumed != "t3-thread" {
		t.Fatalf("resumed %q", resumed)
	}
}

type singleEventSource struct{}

func (singleEventSource) Events(ctx context.Context, since string) (<-chan threads.Event, <-chan error) {
	events := make(chan threads.Event, 1)
	events <- threads.Event{ID: "event", Type: "message.created", ChannelID: "channel", Message: threads.Message{ID: "root", Content: "hello"}}
	return events, make(chan error)
}

type shutdownRunner struct{ started, cancelling, release chan struct{} }

func (r shutdownRunner) Run(ctx context.Context, scope config.Scope, input runner.Input) (runner.Output, error) {
	close(r.started)
	<-ctx.Done()
	close(r.cancelling)
	<-r.release
	return runner.Output{}, ctx.Err()
}

func TestRunDrainsActiveRunnerBeforeReturningOnShutdown(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := shutdownRunner{started: make(chan struct{}), cancelling: make(chan struct{}), release: make(chan struct{})}
	scope := matchAllScope("scope")
	scope.Threads.Token = "test-token"
	d := Daemon{Store: st, Runner: r, Config: config.Config{Scopes: []config.Scope{scope}}, Clients: func(config.Scope, string) (EventSource, Sender, ProcessClient) {
		return singleEventSource{}, &fakeSender{}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- d.Run(ctx) }()
	<-r.started
	cancel()
	<-r.cancelling
	select {
	case err := <-result:
		t.Fatalf("daemon returned before remote cleanup: %v", err)
	default:
	}
	close(r.release)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not finish after runner drained")
	}
}
