package core

import (
	"crypto/rand"
	"log/slog"
)

func newQueuedMessageID() string { return rand.Text() }

func (e *Engine) sendQueuedMessageChoice(p Platform, replyCtx any, queued queuedMessage) {
	card := NewCard().Title(e.i18n.T(MsgQueueChoiceTitle), "blue").
		Markdown(e.i18n.T(MsgMessageQueued)).
		Markdown(truncateIf(queued.content, 240)).
		Buttons(
			PrimaryBtn(e.i18n.T(MsgQueueSteerButton), "cmd:/ps --queued "+queued.queueID),
			DefaultBtn(e.i18n.T(MsgQueueKeepButton), "cmd:/ps --keep "+queued.queueID),
		).Build()
	e.sendWithCard(p, replyCtx, card)
}

// Remove the message before calling the adapter so result handling cannot also
// drain it. Only a confirmed non-delivery (false, nil) returns it to the queue;
// an error may mean the remote accepted input but its acknowledgement was lost.
func (e *Engine) handleQueuedMessageChoice(p Platform, msg *Message, id string, keep bool) {
	key := e.interactiveKeyForSessionKey(msg.SessionKey)
	e.interactiveMu.Lock()
	state := e.interactiveStates[key]
	if state == nil {
		e.interactiveMu.Unlock()
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQueueChoiceExpired))
		return
	}
	state.mu.Lock()
	e.interactiveMu.Unlock()
	index := -1
	for i, queued := range state.pendingMessages {
		if queued.queueID == id && !queued.queueChoiceClosed && msg.UserID != "" && queued.userID == msg.UserID {
			index = i
			break
		}
	}
	if index < 0 || state.stopped {
		state.mu.Unlock()
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQueueChoiceExpired))
		return
	}
	if keep {
		state.pendingMessages[index].queueChoiceClosed = true
		state.mu.Unlock()
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMessageQueued))
		return
	}
	sender, ok := state.agentSession.(UserInputSteerer)
	if !ok || state.queueSteering {
		state.mu.Unlock()
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMessageQueued))
		return
	}
	queued := state.pendingMessages[index]
	queued.queueChoiceClosed = true
	predecessors := make(map[string]bool, index)
	for _, earlier := range state.pendingMessages[:index] {
		predecessors[earlier.queueID] = true
	}
	state.pendingMessages = append(state.pendingMessages[:index], state.pendingMessages[index+1:]...)
	state.queueSteering = true
	defer func() {
		state.mu.Lock()
		state.queueSteering = false
		state.mu.Unlock()
	}()
	session := state.busySession
	workspace := state.workspaceDir
	state.mu.Unlock()

	agent, sessions := e.sessionContextForKey(msg.SessionKey)
	if session == nil {
		session = sessions.GetOrCreateActive(msg.SessionKey)
	}
	prompt := e.buildSenderPrompt(queued.content, queued.userID, queued.userName, queued.msgPlatform, queued.msgSessionKey, queued.channelKey)
	steered, err := sender.SteerUserInput(prompt)
	if steered || err != nil {
		session.AddHistory("user", queued.content)
		sessions.Save()
		if err != nil {
			slog.Warn("queued message steering unconfirmed; not replaying", "error", err)
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQueueSteerUnconfirmed))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPsSent))
		}
		return
	}
	if !e.restoreQueuedMessage(key, state, queued, predecessors) {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQueueChoiceExpired))
		return
	}
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMessageQueued))
	if lockGen, locked := session.TryLock(); locked {
		go e.drainOrphanedQueue(session, sessions, key, agent, workspace, lockGen)
	}
}

func (e *Engine) restoreQueuedMessage(key string, state *interactiveState, queued queuedMessage, predecessors map[string]bool) bool {
	e.interactiveMu.Lock()
	if e.interactiveStates[key] != state {
		e.interactiveMu.Unlock()
		return false
	}
	state.mu.Lock()
	e.interactiveMu.Unlock()
	defer state.mu.Unlock()
	if state.stopped {
		return false
	}
	// Earlier entries may have drained while the adapter was responding. Insert
	// after surviving predecessors, before later entries and newly queued input.
	index := 0
	for i, pending := range state.pendingMessages {
		if predecessors[pending.queueID] {
			index = i + 1
		}
	}
	state.pendingMessages = append(state.pendingMessages, queuedMessage{})
	copy(state.pendingMessages[index+1:], state.pendingMessages[index:])
	state.pendingMessages[index] = queued
	return true
}
