package engine

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"chattoneko/internal/mcphub"
	"chattoneko/internal/provider"
	"chattoneko/internal/store"
)

// defaultGracePeriod is how long a finished generation's replay buffer is
// kept for late subscribers (Engine.graceInterval overrides it in tests).
const defaultGracePeriod = 5 * time.Second

// defaultFlushInterval is how often streamed content is persisted
// mid-generation (Engine.flushInterval overrides it in tests).
const defaultFlushInterval = 500 * time.Millisecond

// cancelOutcome maps a canceled generation to (status, error text): a user
// stop keeps "stopped" with no error; any other cancellation (server
// shutdown) is a failed generation with a clear reason.
func cancelOutcome(ag *activeGen) (string, string) {
	ag.mu.Lock()
	stopped := ag.stopped
	ag.mu.Unlock()
	if stopped {
		return store.StatusStopped, ""
	}
	return store.StatusFailed, "server shutting down"
}

// runGeneration is the turn loop for one generation. It publishes events to
// the chat hub, persists incrementally, executes the tool loop bounded by
// limits.max_tool_iterations, and finalizes with the tool-call history
// invariant on EVERY exit path (B2).
func (e *Engine) runGeneration(ag *activeGen) {
	h := e.hubFor(ag.chatID)
	ctx := ag.ctx

	// Track per-turn usage + wall-clock duration across all iterations.
	// Declared before the finish closure so it can capture them.
	startTime := time.Now()
	var totalPrompt, totalCompletion int64

	// Incremental persistence ticker.
	done := make(chan struct{})
	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		ticker := time.NewTicker(e.flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ag.mu.Lock()
				if ag.dirty && !ag.deleted && !ag.done {
					text, reasoning := ag.text, ag.reasoning
					ag.dirty = false
					ag.mu.Unlock()
					if err := e.store.UpdateMessageContent(ctx, ag.messageID, text, reasoning); err != nil {
						slog.Debug("engine: flush content", "error", err)
						// A failed flush (transient DB error) must be retried on
						// the next tick or the snapshot is silently lost.
						// After a cancel the finalize persistence covers the
						// content, so don't retry then.
						if ctx.Err() == nil {
							ag.mu.Lock()
							ag.dirty = true
							ag.mu.Unlock()
						}
					}
				} else {
					ag.mu.Unlock()
				}
			case <-done:
				return
			}
		}
	}()

	finish := func(status, errText string) {
		// Stop the incremental flusher FIRST and wait for any in-flight flush
		// to land: a stale flush snapshot (up to flushInterval old) committing
		// AFTER FinalizeMessage would overwrite the final content and lose the
		// last tokens on the serialized DB connection.
		close(done)
		<-flushDone

		ag.mu.Lock()
		deleted := ag.deleted
		text, reasoning := ag.text, ag.reasoning
		ag.done = true
		ag.mu.Unlock()

		if status == store.StatusFailed {
			slog.Error("engine: generation failed", "chat", ag.chatID, "message", ag.messageID, "error", errText)
		}

		// Persistence in finalize MUST survive the generation ctx being
		// canceled (stop/shutdown/deletion): detach cancellation. DB writes
		// happen OUTSIDE the hub lock so subscribers are not blocked.
		persistCtx := context.WithoutCancel(ctx)

		// Persist per-turn usage + duration on the assistant message.
		durationMs := time.Since(startTime).Milliseconds()

		if !deleted {
			// Invariant finalize: synthetic results for dangling tool calls,
			// then the terminal status. Order matters: readers must never see
			// a terminal assistant message with unanswered calls.
			if err := e.synthesizeToolResults(persistCtx, ag.chatID, ag.messageID, interruptText(status)); err != nil {
				slog.Error("engine: synthesize tool results", "error", err)
			}
			if err := e.store.FinalizeMessage(persistCtx, ag.messageID, status, errText, text, reasoning); err != nil {
				slog.Error("engine: finalize message", "error", err)
			}
			if totalPrompt > 0 || totalCompletion > 0 || durationMs > 0 {
				if err := e.store.UpdateMessageUsage(persistCtx, ag.messageID, totalPrompt, totalCompletion, durationMs); err != nil {
					slog.Debug("engine: persist usage", "error", err)
				}
			}
			_ = e.store.TouchChat(persistCtx, ag.chatID)
		}

		// Include usage in done so the client can render per-message stats
		// (#5) and the top-bar totals without a refetch. done is replayed, so
		// it can't be lost.
		h.mu.Lock()
		h.publishGen(WireEvent{Type: "status", Status: status, Error: errText})
		h.publishGen(WireEvent{
			Type:             "done",
			PromptTokens:     totalPrompt,
			CompletionTokens: totalCompletion,
			DurationMs:       durationMs,
		})
		h.mu.Unlock()

		// Grace period: keep the replay buffer for late subscribers, then
		// drop the reference (after this, Subscribe yields "idle"). With no
		// generation and no subscribers left, the hub itself can go too.
		time.AfterFunc(e.graceInterval, func() {
			h.mu.Lock()
			replaced := h.gen != ag
			h.mu.Unlock()
			if replaced {
				return // deleted or superseded; that lifecycle owns the hub now
			}
			h.mu.Lock()
			if h.gen == ag {
				h.gen = nil
			}
			h.mu.Unlock()
			e.maybePruneHub(ag.chatID, h)
		})
	}

	// stepFailed finishes the generation after a ctx-aware step failed. If
	// the generation context is done, the step error is only a symptom of the
	// cancel (user stop / chat deletion / shutdown) — without this check a
	// stop racing a step would finalize the message as "failed" with a
	// confusing context/store error instead of the cancel outcome. Otherwise
	// it is a real failure.
	stepFailed := func(errText string) {
		if ctx.Err() != nil {
			status, cancelText := cancelOutcome(ag)
			finish(status, cancelText)
			return
		}
		finish(store.StatusFailed, errText)
	}

	chat, params, err := e.chatParams(ctx, ag.chatID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			finish(store.StatusFailed, "chat not found")
			return
		}
		stepFailed("load chat: " + err.Error())
		return
	}

	iterations := 0
	for {
		// Cancellation check between iterations.
		select {
		case <-ctx.Done():
			status, errText := cancelOutcome(ag)
			finish(status, errText)
			return
		default:
		}

		msgs, err := e.store.ListMessages(ctx, ag.chatID)
		if err != nil {
			stepFailed("load history: " + err.Error())
			return
		}
		providerMsgs, err := e.buildProviderMessages(ctx, chat, msgs, ag.attCache)
		if err != nil {
			stepFailed("build request: " + err.Error())
			return
		}
		tools, err := e.effectiveTools(ctx, chat)
		if err != nil {
			stepFailed("tools: " + err.Error())
			return
		}

		stream, err := e.prov.StreamChat(ctx, providerMsgs, tools, params)
		if err != nil {
			stepFailed("provider error: " + err.Error())
			return
		}

		var calls []provider.ToolCall
		var streamErr error
		var finishReason string
	drain:
		for stream.Next() {
			ev := stream.Event()
			switch ev.Kind {
			case provider.EventTextDelta:
				ag.mu.Lock()
				ag.text += ev.Text
				ag.dirty = true
				ag.mu.Unlock()
				h.mu.Lock()
				h.publishGen(WireEvent{Type: "delta", Content: ev.Text})
				h.mu.Unlock()
			case provider.EventReasoningDelta:
				ag.mu.Lock()
				ag.reasoning += ev.Text
				ag.dirty = true
				ag.mu.Unlock()
				h.mu.Lock()
				h.publishGen(WireEvent{Type: "reasoning_delta", Content: ev.Text})
				h.mu.Unlock()
			case provider.EventToolCallStart:
				h.mu.Lock()
				h.publishGen(WireEvent{Type: "tool_call_started", CallID: ev.CallID, Name: ev.Name})
				h.mu.Unlock()
			case provider.EventToolCallDelta:
				// Incremental arguments fragment; the client appends it to the
				// call's args. tool_call_done re-delivers the full arguments,
				// so a reconnect mid-stream self-heals.
				h.mu.Lock()
				h.publishGen(WireEvent{Type: "tool_call_delta", CallID: ev.CallID, Arguments: ev.Args})
				h.mu.Unlock()
			case provider.EventToolCallDone:
				calls = append(calls, provider.ToolCall{ID: ev.CallID, Name: ev.Name, Arguments: ev.Args})
				h.mu.Lock()
				h.publishGen(WireEvent{Type: "tool_call_done", CallID: ev.CallID, Name: ev.Name, Arguments: ev.Args})
				h.mu.Unlock()
			case provider.EventError:
				streamErr = ev.Err
			case provider.EventDone:
				// Finish reason is provider-dependent ("stop", "tool_calls",
				// "length", ...); the tool loop below keys on the presence of
				// collected calls. Accumulate usage per turn.
				finishReason = ev.Finish
				totalPrompt += ev.Usage.PromptTokens
				totalCompletion += ev.Usage.CompletionTokens
			}
			// Honor stop promptly even mid-stream.
			select {
			case <-ctx.Done():
				stream.Close()
				break drain
			default:
			}
		}
		if streamErr == nil {
			streamErr = stream.Err()
		}
		// Always release the stream: unblocks a producer goroutine that could
		// otherwise sit in Publish forever once we stop consuming (leak).
		stream.Close()

		select {
		case <-ctx.Done():
			status, errText := cancelOutcome(ag)
			finish(status, errText)
			return
		default:
		}

		if streamErr != nil {
			stepFailed("provider error: " + streamErr.Error())
			return
		}

		// Tool loop gate: the presence of collected calls drives the next
		// iteration, independent of the finish-reason spelling.
		if len(calls) > 0 {
			iterations++
			// Persist the assistant's accumulated content + calls before
			// executing tools (crash safety) and before the next iteration.
			ag.mu.Lock()
			text, reasoning := ag.text, ag.reasoning
			ag.dirty = false
			ag.mu.Unlock()
			if err := e.store.UpdateMessageContent(ctx, ag.messageID, text, reasoning); err != nil {
				stepFailed("persist: " + err.Error())
				return
			}
			for i, c := range calls {
				if _, err := e.store.CreateToolCall(ctx, ag.messageID, c.ID, c.Name, c.Arguments, int64(i)); err != nil {
					stepFailed("persist tool call: " + err.Error())
					return
				}
			}
			e.executeTools(ctx, h, chat, ag, calls)

			if iterations >= e.cfg.Get().Limits.MaxToolIterations {
				// Cap reached: every call above already has a real result;
				// finalize complete (invariant holds — nothing dangling).
				finish(store.StatusComplete, "")
				return
			}
			continue
		}

		// A truncation finish reason means the provider cut the answer off at
		// the output-token limit: surface it as a failure instead of silently
		// finalizing a cut-off message as a clean success. Providers spell it
		// differently ("length", "max_tokens", "max_output_tokens", ...).
		if isTruncationFinish(finishReason) {
			finish(store.StatusFailed, "provider stopped at the output length limit ("+finishReason+"); the answer was truncated")
			return
		}

		finish(store.StatusComplete, "")
		return
	}
}

// isTruncationFinish reports whether a provider finish reason means the
// answer was cut off at an output-token limit (as opposed to a clean stop or
// a tool-call handoff). The reason strings are provider-specific, so match
// on the well-known spellings.
func isTruncationFinish(reason string) bool {
	switch reason {
	case "length", // OpenAI
		"max_tokens",        // Anthropic / some OpenAI-compatible
		"max_output_tokens", // Anthropic newer spelling
		"output_length",     // generic
		"model_max_tokens":  // some proxies
		return true
	}
	return false
}

// executeTools runs each tool call and persists + publishes the results.
// A canceled generation aborts the remaining calls; the invariant finalize
// covers any left dangling with synthetic results. Events publish to the
// generation's own hub (never re-resolved: after a chat deletion the old
// hub is dropped and hubFor would create a fresh one nobody subscribes to).
func (e *Engine) executeTools(ctx context.Context, h *chatHub, chat *store.Chat, ag *activeGen, calls []provider.ToolCall) {
	enabled := e.enabledTools(chat)
	meta := mcphub.CallMeta{ChatID: chat.ID, MessageID: ag.messageID}
	// Tool side effects happen under the cancellable ctx, but persisting a
	// result that EXISTS must survive a user stop / server shutdown landing
	// mid-batch: otherwise the invariant finalize would falsely record
	// "interrupted before this tool call could run" and the model would
	// re-run the tool on the next generation, doubling its side effects.
	persistCtx := context.WithoutCancel(ctx)
	for _, c := range calls {
		if ctx.Err() != nil {
			return
		}
		var result string
		isError := false
		if !enabled[c.Name] {
			result = "Error: tool '" + c.Name + "' is disabled by the user."
			isError = true
		} else {
			r, toolErr, err := e.catalog.Call(ctx, c.Name, c.Arguments, meta)
			if err != nil {
				result = "Error: " + err.Error()
				isError = true
			} else {
				result = r
				isError = toolErr
			}
			if isError {
				slog.Warn("engine: tool call failed", "tool", c.Name, "chat", chat.ID, "error", result)
			}
		}
		if _, err := e.store.CreateMessage(persistCtx, store.NewMessageParams{
			ChatID:     chat.ID,
			Role:       store.RoleTool,
			Status:     store.StatusComplete,
			Content:    result,
			ToolCallID: c.ID,
			Name:       c.Name,
		}); err != nil {
			slog.Error("engine: persist tool result", "error", err)
			continue
		}
		h.mu.Lock()
		h.publishGen(WireEvent{Type: "tool_result", CallID: c.ID, Name: c.Name, Result: result, IsError: isError})
		h.mu.Unlock()
		e.publishNewAttachments(persistCtx, h, ag)
	}
}

// publishNewAttachments publishes an attachment_created event for every
// attachment on the generating assistant message not yet announced (files
// persisted by integrated tools during executeTools). Metas only — no blob
// reads; the event is replayed so reconnecting clients don't lose the chip.
func (e *Engine) publishNewAttachments(ctx context.Context, h *chatHub, ag *activeGen) {
	metas, err := e.store.ListAttachmentsByMessage(ctx, ag.messageID)
	if err != nil {
		slog.Debug("engine: list message attachments", "error", err)
		return
	}
	for i := range metas {
		if ag.attSent[metas[i].ID] {
			continue
		}
		ag.attSent[metas[i].ID] = true
		m := metas[i]
		h.mu.Lock()
		h.publishGen(WireEvent{Type: "attachment_created", Attachment: &m})
		h.mu.Unlock()
	}
}

// interruptText is the synthetic tool result for each termination cause.
func interruptText(status string) string {
	switch status {
	case store.StatusStopped:
		return "Error: generation stopped by user before this tool call could run."
	case store.StatusFailed:
		return "Error: generation was interrupted before this tool call could run."
	default:
		return "Error: tool iteration limit reached; this tool call was not executed."
	}
}
