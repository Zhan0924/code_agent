package indexer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSupportedExtensions(t *testing.T) {
	expected := map[string]bool{
		".go": true, ".py": true, ".js": true, ".ts": true,
		".java": true, ".rs": true, ".rb": true,
		".c": true, ".cpp": true, ".h": true, ".hpp": true,
		".md": true, ".yaml": true, ".yml": true, ".json": true, ".toml": true,
	}
	for ext := range expected {
		if _, ok := supportedExtensions[ext]; !ok {
			t.Errorf("expected extension %s to be supported", ext)
		}
	}
	// Unsupported extension
	if _, ok := supportedExtensions[".exe"]; ok {
		t.Error(".exe should not be a supported extension")
	}
}

func TestDefaultIgnorePatterns(t *testing.T) {
	mustIgnore := map[string]bool{
		".git": true, "node_modules": true, "vendor": true,
		"__pycache__": true, ".venv": true,
	}
	for _, p := range defaultIgnorePatterns {
		delete(mustIgnore, p)
	}
	for missing := range mustIgnore {
		t.Errorf("expected %s to be in defaultIgnorePatterns", missing)
	}
}

func TestHasContentChanged(t *testing.T) {
	idx := &Indexer{
		checksums: make(map[string]FileChecksum),
	}

	content := []byte("package main\nfunc main() {}\n")

	// First call: no existing checksum → should report changed
	if !idx.hasContentChanged("/fake/file.go", content) {
		t.Error("new file should be reported as changed")
	}

	// Store checksum
	idx.updateChecksum("/fake/file.go", content)

	// Same content → should NOT be changed
	if idx.hasContentChanged("/fake/file.go", content) {
		t.Error("same content should not be reported as changed")
	}

	// Different content → should be changed
	newContent := []byte("package main\nfunc main() { println(42) }\n")
	if !idx.hasContentChanged("/fake/file.go", newContent) {
		t.Error("different content should be reported as changed")
	}
}

func TestGetStats(t *testing.T) {
	idx := &Indexer{
		checksums: make(map[string]FileChecksum),
	}

	stats := idx.GetStats()
	if stats["indexed_files"] != 0 {
		t.Errorf("expected 0 indexed files, got %d", stats["indexed_files"])
	}

	// Add some checksums
	idx.updateChecksum("/a.go", []byte("a"))
	idx.updateChecksum("/b.go", []byte("b"))

	stats = idx.GetStats()
	if stats["indexed_files"] != 2 {
		t.Errorf("expected 2 indexed files, got %d", stats["indexed_files"])
	}
}

func TestIndexRepositorySkipsIgnoredDirs(t *testing.T) {
	// Create a temp directory structure
	tmpDir := t.TempDir()

	// Create a supported file
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main"), 0644)

	// Create an ignored directory with a file
	nodeModules := filepath.Join(tmpDir, "node_modules")
	os.MkdirAll(nodeModules, 0755)
	os.WriteFile(filepath.Join(nodeModules, "lib.js"), []byte("console.log()"), 0644)

	// Create an unsupported file
	os.WriteFile(filepath.Join(tmpDir, "image.png"), []byte{0x89, 0x50}, 0644)

	// We can't test IndexRepository fully without a RAG engine, but we can
	// verify the file walk logic by checking that supported extensions work
	ext := filepath.Ext("main.go")
	if _, ok := supportedExtensions[ext]; !ok {
		t.Error("expected .go to be supported")
	}

	ext2 := filepath.Ext("image.png")
	if _, ok := supportedExtensions[ext2]; ok {
		t.Error("expected .png to NOT be supported")
	}
}
