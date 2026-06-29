package main

import (
	"database/sql"
	"fmt"

	"github.com/agent/code_agent/internal/memory"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"go.uber.org/zap"
)

func main() {
	dsn := "postgres://agent:agent_secret@localhost:15432/code_agent?sslmode=disable"
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	logger, _ := zap.NewDevelopment()

	// 1. Init with dim=1536
	fmt.Println("Migrating with 1536...")
	cold1536 := memory.NewPGColdWithDim(db, logger, 1536)
	if err := cold1536.Migrate(); err != nil {
		panic(err)
	}

	mem1 := &memory.Memory{
		ID:        uuid.New().String(),
		UserID:    "user1",
		ProjectID: "proj1",
		Type:      "knowledge",
		Content:   "Knowledge 1536",
		Embedding: make([]float32, 1536),
	}
	mem1.Embedding[0] = 0.5
	if err := cold1536.Store(mem1); err != nil {
		panic(fmt.Errorf("Failed to store 1536: %w", err))
	}
	fmt.Println("Stored 1536 memory.")

	// 2. Init with dim=3072
	fmt.Println("Migrating with 3072...")
	cold3072 := memory.NewPGColdWithDim(db, logger, 3072)
	if err := cold3072.Migrate(); err != nil {
		panic(err)
	}

	mem2 := &memory.Memory{
		ID:        uuid.New().String(),
		UserID:    "user1",
		ProjectID: "proj1",
		Type:      "knowledge",
		Content:   "Knowledge 3072",
		Embedding: make([]float32, 3072),
	}
	mem2.Embedding[0] = 0.9
	if err := cold3072.Store(mem2); err != nil {
		panic(fmt.Errorf("Failed to store 3072: %w", err))
	}
	fmt.Println("Stored 3072 memory.")

	// 3. Search using 3072
	fmt.Println("Retrieving with 3072...")
	q := make([]float32, 3072)
	q[0] = 0.9
	res, err := cold3072.RetrieveByVector(q, "user1", "proj1", 10)
	if err != nil {
		panic(err)
	}
	for _, m := range res {
		fmt.Printf("Retrieved 3072: %s (score=%.2f)\n", m.Content, m.Score)
	}

	fmt.Println("All done successfully!")
}
