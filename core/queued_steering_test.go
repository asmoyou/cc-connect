package core

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

type queuedSteeringSession struct {
	asyncInputSession
	steer func(string) (bool, error)
}

func (s *queuedSteeringSession) SteerUserInput(prompt string) (bool, error) {
	if s.steer != nil {
		return s.steer(prompt)
	}
	return s.asyncInputSession.SteerUserInput(prompt)
}

func queuedChoiceAction(t *testing.T, card *Card, keep bool) string {
	t.Helper()
	prefix := "cmd:/ps --queued "
	if keep {
		prefix = "cmd:/ps --keep "
	}
	for _, element := range card.Elements {
		if row, ok := element.(CardActions); ok {
			for _, button := range row.Buttons {
				if strings.HasPrefix(button.Value, prefix) {
					return strings.TrimPrefix(button.Value, "cmd:")
				}
			}
		}
	}
	t.Fatalf("missing %s button in %#v", prefix, card)
	return ""
}

func TestQueuedSteering_KeepOwnershipAndUnconfirmedDelivery(t *testing.T) {
	for _, scenario := range []string{"keep", "other-user", "anonymous", "wrong-session", "uncertain", "idle", "stopped"} {
		t.Run(scenario, func(t *testing.T) {
			p := &stubPlatformEngine{n: "test"}
			s := &queuedSteeringSession{asyncInputSession: asyncInputSession{events: make(chan Event, 8), active: true}}
			e := NewEngine("test", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
			key := "test:chat:user"
			session := e.sessions.GetOrCreateActive(key)
			gen, locked := session.TryLock()
			if !locked {
				t.Fatal("lock session")
			}
			t.Cleanup(func() { session.Unlock(gen) })
			state := &interactiveState{agentSession: s, busySession: session, pendingMessages: []queuedMessage{
				{queueID: "one", content: "first", userID: "user"},
				{queueID: "two", content: "second", userID: "user"},
			}}
			e.interactiveStates[key] = state
			msg := &Message{SessionKey: key, UserID: "user", ReplyCtx: "ctx"}
			switch scenario {
			case "other-user":
				msg.UserID = "someone-else"
			case "anonymous":
				msg.UserID = ""
			case "wrong-session":
				msg.SessionKey = "test:another-chat:user"
			case "uncertain":
				s.err = errors.New("acknowledgement lost")
			case "idle":
				s.active = false
			case "stopped":
				state.stopped = true
			}
			e.handleQueuedMessageChoice(p, msg, "one", scenario == "keep")
			state.mu.Lock()
			defer state.mu.Unlock()
			if scenario == "uncertain" {
				if len(state.pendingMessages) != 1 || state.pendingMessages[0].content != "second" {
					t.Fatalf("uncertain input must not be replayed: %#v", state.pendingMessages)
				}
				history := session.GetHistory(10)
				if len(history) != 1 || history[0].Content != "first" {
					t.Fatalf("unconfirmed input missing from history: %#v", history)
				}
				if !strings.Contains(strings.Join(p.getSent(), "\n"), e.i18n.T(MsgQueueSteerUnconfirmed)) {
					t.Fatal("missing unconfirmed-delivery notice")
				}
				return
			}
			if len(state.pendingMessages) != 2 || state.pendingMessages[0].content != "first" || state.pendingMessages[1].content != "second" {
				t.Fatalf("queue lost or reordered: %#v", state.pendingMessages)
			}
			if len(s.steered) != 0 {
				t.Fatal("unexpected steering")
			}
			if scenario == "keep" && !state.pendingMessages[0].queueChoiceClosed {
				t.Fatal("keep must close the choice without removing the message")
			}
		})
	}
}

func TestQueuedSteering_ClaimBeforeRPCAndRestoreAfterConcurrentDrain(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	s := &queuedSteeringSession{asyncInputSession: asyncInputSession{events: make(chan Event, 8), active: true}}
	e := NewEngine("test", &controllableAgent{nextSession: s}, []Platform{p}, "", LangEnglish)
	const key = "test:user"
	session := e.sessions.GetOrCreateActive(key)
	gen, _ := session.TryLock()
	defer session.Unlock(gen)
	state := &interactiveState{agentSession: s, busySession: session, pendingMessages: []queuedMessage{
		{queueID: "first", content: "first", userID: "user"},
		{queueID: "chosen", content: "chosen", userID: "user"},
		{queueID: "last", content: "last", userID: "user"},
	}}
	e.interactiveStates[key] = state
	s.steer = func(string) (bool, error) {
		// Model completion and a new arrival happen while the RPC is in flight.
		state.mu.Lock()
		if len(state.pendingMessages) != 2 || state.pendingMessages[1].queueID != "last" {
			t.Errorf("selected input was not claimed before RPC: %#v", state.pendingMessages)
		}
		state.pendingMessages = state.pendingMessages[1:]
		state.pendingMessages = append(state.pendingMessages, queuedMessage{queueID: "new", content: "new"})
		state.mu.Unlock()
		return false, nil
	}
	e.handleQueuedMessageChoice(p, &Message{SessionKey: key, UserID: "user"}, "chosen", false)
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.pendingMessages) != 3 || state.pendingMessages[0].content != "chosen" || state.pendingMessages[1].content != "last" || state.pendingMessages[2].content != "new" {
		t.Fatalf("restore moved chosen input behind later arrivals: %#v", state.pendingMessages)
	}
}

func TestQueuedSteering_ConcurrentClicksDoNotDuplicateOrStealInput(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s := &queuedSteeringSession{asyncInputSession: asyncInputSession{events: make(chan Event, 8), active: true}}
	e := NewEngine("test", &controllableAgent{nextSession: s}, []Platform{p}, "", LangEnglish)
	const key = "test:user"
	session := e.sessions.GetOrCreateActive(key)
	gen, _ := session.TryLock()
	defer session.Unlock(gen)
	state := &interactiveState{agentSession: s, busySession: session, pendingMessages: []queuedMessage{
		{queueID: "one", content: "first", userID: "user"},
		{queueID: "two", content: "second", userID: "user"},
	}}
	e.interactiveStates[key] = state
	s.steer = func(string) (bool, error) { close(entered); <-release; return true, nil }
	msg := &Message{SessionKey: key, UserID: "user"}
	go func() { defer close(done); e.handleQueuedMessageChoice(p, msg, "one", false) }()
	<-entered
	e.handleQueuedMessageChoice(p, msg, "one", false)
	e.handleQueuedMessageChoice(p, msg, "two", false)
	close(release)
	<-done
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.pendingMessages) != 1 || state.pendingMessages[0].queueID != "two" {
		t.Fatalf("concurrent click removed another message: %#v", state.pendingMessages)
	}
}

func TestQueuedSteering_ReplacementStateDoesNotReceiveRestoredInput(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	s := &queuedSteeringSession{asyncInputSession: asyncInputSession{events: make(chan Event, 8), active: true}}
	e := NewEngine("test", &controllableAgent{nextSession: s}, []Platform{p}, "", LangEnglish)
	const key = "test:user"
	old := &interactiveState{agentSession: s, pendingMessages: []queuedMessage{{queueID: "one", userID: "user"}}}
	replacement := &interactiveState{agentSession: s}
	e.interactiveStates[key] = old
	s.steer = func(string) (bool, error) {
		e.interactiveMu.Lock()
		e.interactiveStates[key] = replacement
		e.interactiveMu.Unlock()
		return false, nil
	}
	e.handleQueuedMessageChoice(p, &Message{SessionKey: key, UserID: "user"}, "one", false)
	if len(replacement.pendingMessages) != 0 {
		t.Fatal("expired input crossed into a replacement conversation")
	}
}

func TestQueuedSteering_UnsupportedAndAttachmentMessagesRetainQueueReceipt(t *testing.T) {
	for _, scenario := range []string{"no-steering", "image", "file", "anonymous"} {
		t.Run(scenario, func(t *testing.T) {
			p := &asyncInputPlatform{}
			p.n = "test"
			s := &asyncInputSession{events: make(chan Event, 8), active: true}
			e := NewEngine("test", &controllableAgent{nextSession: s}, []Platform{p}, "", LangEnglish)
			var agentSession AgentSession = s
			if scenario == "no-steering" {
				agentSession = &stubAgentSession{}
			}
			const key = "test:user"
			state := &interactiveState{agentSession: agentSession}
			e.interactiveStates[key] = state
			msg := &Message{SessionKey: key, Platform: "test", UserID: "user", Content: "follow-up"}
			switch scenario {
			case "image":
				msg.Images = []ImageAttachment{{MimeType: "image/png"}}
			case "file":
				msg.Files = []FileAttachment{{FileName: "notes.txt"}}
			case "anonymous":
				msg.UserID = ""
			}
			if !e.queueMessageForBusySession(p, msg, key) {
				t.Fatal("message was not queued")
			}
			if len(p.sentCards) != 0 || len(p.repliedCards) != 0 {
				t.Fatal("unsupported message received a steering control")
			}
			if !strings.Contains(strings.Join(p.getSent(), "\n"), e.i18n.T(MsgMessageQueued)) {
				t.Fatal("ordinary queue receipt missing")
			}
			if len(state.pendingMessages) != 1 {
				t.Fatal("ordinary queue lost the message")
			}
		})
	}
}
