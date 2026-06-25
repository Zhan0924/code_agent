package memory

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// MemoryEvent is the payload sent over the blackboard.
type MemoryEvent struct {
	Action    string  `json:"action"` // e.g., "added", "updated"
	Memory    *Memory `json:"memory"`
	ProjectID string  `json:"project_id"`
}

// Blackboard is an event bus for memory events across agents.
type Blackboard struct {
	client *redis.Client
	logger *zap.Logger
}

func NewBlackboard(client *redis.Client, logger *zap.Logger) *Blackboard {
	return &Blackboard{
		client: client,
		logger: logger.With(zap.String("component", "memory.blackboard")),
	}
}

func (b *Blackboard) channel(projectID string) string {
	return fmt.Sprintf("blackboard:project:%s", projectID)
}

// Publish broadcasts a memory event.
func (b *Blackboard) Publish(ctx context.Context, action string, m *Memory) error {
	event := MemoryEvent{
		Action:    action,
		Memory:    m,
		ProjectID: m.ProjectID,
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return b.client.Publish(ctx, b.channel(m.ProjectID), data).Err()
}

// Subscribe returns a Go channel that receives memory events for a project.
func (b *Blackboard) Subscribe(ctx context.Context, projectID string) (<-chan MemoryEvent, error) {
	pubsub := b.client.Subscribe(ctx, b.channel(projectID))
	
	// Ensure subscription is successful before proceeding.
	_, err := pubsub.Receive(ctx)
	if err != nil {
		return nil, err
	}

	ch := pubsub.Channel()
	out := make(chan MemoryEvent, 10)
	
	go func() {
		defer pubsub.Close()
		defer close(out)
		
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-ch:
				if msg == nil {
					return
				}
				var event MemoryEvent
				if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
					b.logger.Error("failed to unmarshal blackboard event", zap.Error(err))
					continue
				}
				b.logger.Info("received blackboard event", zap.String("action", event.Action), zap.String("memory_id", event.Memory.ID))
				
				select {
				case out <- event:
				case <-ctx.Done():
					return
				default:
					b.logger.Warn("blackboard event dropped, channel full")
				}
			}
		}
	}()

	return out, nil
}
