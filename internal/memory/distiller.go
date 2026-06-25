package memory

import (
	"context"
	"fmt"
	"time"
)

type LLMClient interface {
	GenerateContent(ctx context.Context, prompt string) (string, error)
}

type Distiller struct {
	store      *RedisHot
	llm        LLMClient
	blackboard *Blackboard
}

func NewDistiller(store *RedisHot, llm LLMClient, bb *Blackboard) *Distiller {
	return &Distiller{store: store, llm: llm, blackboard: bb}
}

func (d *Distiller) Distill(ctx context.Context, userID, projectID string) error {
	mems, err := d.store.Retrieve(ctx, userID, projectID, 50)
	if err != nil {
		return err
	}
	
	var episodicContext string
	for _, m := range mems {
		if m.Type == MemoryTypeEpisodic {
			episodicContext += m.Content + "\n"
		}
	}
	
	if episodicContext == "" {
		return nil // Nothing to distill
	}
	
	prompt := fmt.Sprintf("Distill the following episodic memories into a single semantic rule:\n%s", episodicContext)
	distilled, err := d.llm.GenerateContent(ctx, prompt)
	if err != nil {
		return err
	}
	
	semanticMem := &Memory{
		ID:        fmt.Sprintf("sem-%d", time.Now().UnixNano()),
		UserID:    userID,
		ProjectID: projectID,
		Type:      MemoryTypeSemantic,
		Content:   distilled,
		Score:     1.0,
		UpdatedAt: time.Now(),
		CreatedAt: time.Now(),
	}
	
	if err := d.store.Store(ctx, semanticMem); err != nil {
		return err
	}
	
	if d.blackboard != nil {
		_ = d.blackboard.Publish(ctx, "distilled", semanticMem)
	}
	return nil
}
