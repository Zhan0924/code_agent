package multiagent

import (
	"sync"

	"go.uber.org/zap"
)

// MessageBus provides channel-based in-process messaging between agents.
type MessageBus struct {
	mu          sync.RWMutex
	subscribers map[string][]chan Message
	history     []Message
	maxHistory  int
	logger      *zap.Logger
}

// NewMessageBus creates a message bus.
func NewMessageBus(logger *zap.Logger) *MessageBus {
	return &MessageBus{
		subscribers: make(map[string][]chan Message),
		maxHistory:  100,
		logger:      logger.With(zap.String("component", "multiagent.bus")),
	}
}

// Subscribe registers a channel to receive messages for a given recipient.
func (b *MessageBus) Subscribe(recipient string) chan Message {
	ch := make(chan Message, 16)
	b.mu.Lock()
	b.subscribers[recipient] = append(b.subscribers[recipient], ch)
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a channel from a recipient's subscriptions.
func (b *MessageBus) Unsubscribe(recipient string, ch chan Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.subscribers[recipient]
	for i, s := range subs {
		if s == ch {
			b.subscribers[recipient] = append(subs[:i], subs[i+1:]...)
			close(ch)
			return
		}
	}
}

// Publish sends a message to all subscribers of the recipient.
func (b *MessageBus) Publish(msg Message) {
	b.mu.Lock()
	if len(b.history) < b.maxHistory {
		b.history = append(b.history, msg)
	}
	subs := b.subscribers[msg.To]
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- msg:
		default:
			b.logger.Debug("message dropped (subscriber full)",
				zap.String("to", msg.To),
				zap.String("from", msg.From))
		}
	}
}

// Broadcast sends a message to all subscribers regardless of recipient.
func (b *MessageBus) Broadcast(msg Message) {
	b.mu.RLock()
	allSubs := make([]chan Message, 0)
	for _, subs := range b.subscribers {
		allSubs = append(allSubs, subs...)
	}
	b.mu.RUnlock()

	for _, ch := range allSubs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// History returns recent messages.
func (b *MessageBus) History() []Message {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]Message, len(b.history))
	copy(result, b.history)
	return result
}
