package sandbox

import (
	"archive/tar"
	"io"
	"path/filepath"
	"time"
)

// tarWriter is a helper that builds in-memory tar archives for Docker CopyToContainer.
// (F7) This enables multi-file code injection into sandbox containers without
// relying on shell escaping, eliminating shell injection attack vectors.
type tarWriter struct {
	tw *tar.Writer
}

// newTarWriter creates a new tar archive writer backed by the given io.Writer.
func newTarWriter(w io.Writer) *tarWriter {
	return &tarWriter{tw: tar.NewWriter(w)}
}

// writeFile adds a single file to the tar archive.
func (t *tarWriter) writeFile(name string, data []byte) error {
	// Clean the path to prevent directory traversal attacks
	cleanName := filepath.Clean(name)
	if filepath.IsAbs(cleanName) {
		cleanName = cleanName[1:] // Strip leading /
	}

	header := &tar.Header{
		Name:    cleanName,
		Size:    int64(len(data)),
		Mode:    0644,
		ModTime: time.Now(),
	}
	if err := t.tw.WriteHeader(header); err != nil {
		return err
	}
	_, err := t.tw.Write(data)
	return err
}

// close finalizes the tar archive.
func (t *tarWriter) close() error {
	return t.tw.Close()
}
