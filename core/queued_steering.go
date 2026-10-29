package core

import (
	"crypto/rand"
	"log/slog"
	"sync"
)

// Shared by queue copies and button callbacks. Serialize updates so a delayed
// Keep response cannot overwrite the status after automatic queue delivery.
type queuedChoiceReceipt struct {
	mu         sync.Mutex
	replyCtx   any
	processing bool
}

func newQueuedMessageID() string { return rand.Text() }

func (e *Engine) sendQueuedMessageChoice(p Platform, replyCtx any, queued queuedMessage) *queuedChoiceReceipt {
	card := NewCard().Title(e.i18n.T(MsgQueueChoiceTitle), "blue").
		Markdown(e.i18n.T(MsgMessageQueued)).
		Markdown(truncateIf(queued.content, 240)).
		Buttons(
			PrimaryBtn(e.i18n.T(MsgQueueSteerButton), "cmd:/ps --queued "+queued.queueID),
		).Build()
	if sender, ok := p.(TrackedCardSender); ok {
		if err := e.waitOutgoing(p); err != nil {
			slog.Warn("queued choice card send cancelled", "error", err)
			return nil
		}
		ctx, err := sender.SendTrackedCard(e.ctx, replyCtx, e.renderCardForPlatform(p, card))
		if err != nil {
			slog.Warn("queued choice card send failed", "error", err)
			e.reply(p, replyCtx, e.i18n.T(MsgMessageQueued))
			return nil
		}
		if ctx != nil {
			return &queuedChoiceReceipt{replyCtx: ctx}
		}
		return nil
	}
	e.sendWithCard(p, replyCtx, card)
	return nil
}

func (e *Engine) markQueuedMessageProcessing(queued queuedMessage) {
	if queued.choiceReceipt != nil {
		e.updateQueuedMessageChoice(queued.platform, &Message{}, queued, MsgQueueProcessing, MsgQueueProcessing, "green")
	}
}

// Only call after claiming the author's choice. Rejected or duplicate clicks
// must not overwrite an in-flight or completed card with a different status.
func (e *Engine) updateQueuedMessageChoice(p Platform, msg *Message, queued queuedMessage, title, detail MsgKey, color string) bool {
	replyCtx := msg.ReplyCtx
	if receipt := queued.choiceReceipt; receipt != nil {
		receipt.mu.Lock()
		defer receipt.mu.Unlock()
		if receipt.processing && title != MsgQueueProcessing {
			return true
		}
		if title == MsgQueueProcessing {
			receipt.processing = true
		}
		replyCtx = receipt.replyCtx
	}
	updater, ok := p.(CardActionUpdater)
	if !ok {
		return false
	}
	card := NewCard().Title(e.i18n.T(title), color).
		Markdown(e.i18n.T(detail)).Markdown(truncateIf(queued.content, 240)).Build()
	if err := e.waitOutgoing(p); err != nil {
		slog.Warn("queued choice card update cancelled", "error", err)
		return false
	}
	updated, err := updater.UpdateActionCard(e.ctx, replyCtx, e.renderCardForPlatform(p, card))
	if err != nil {
		slog.Warn("queued choice card update failed", "error", err)
		return false
	}
	return updated
}

func (e *Engine) finishQueuedMessageChoice(p Platform, msg *Message, queued queuedMessage, title, detail MsgKey, color string) {
	if !e.updateQueuedMessageChoice(p, msg, queued, title, detail, color) {
		e.reply(p, msg.ReplyCtx, e.i18n.T(detail))
	}
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
		queued := state.pendingMessages[index]
		state.mu.Unlock()
		e.finishQueuedMessageChoice(p, msg, queued, MsgQueueKept, MsgMessageQueued, "blue")
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
	e.updateQueuedMessageChoice(p, msg, queued, MsgQueueSteerPending, MsgQueueSteerPending, "blue")

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
			e.finishQueuedMessageChoice(p, msg, queued, MsgQueueChoiceTitle, MsgQueueSteerUnconfirmed, "orange")
		} else {
			e.finishQueuedMessageChoice(p, msg, queued, MsgQueueSteerSent, MsgPsSent, "green")
		}
		return
	}
	if !e.restoreQueuedMessage(key, state, queued, predecessors) {
		e.finishQueuedMessageChoice(p, msg, queued, MsgQueueChoiceTitle, MsgQueueChoiceExpired, "grey")
		return
	}
	e.finishQueuedMessageChoice(p, msg, queued, MsgQueueKept, MsgMessageQueued, "blue")
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
