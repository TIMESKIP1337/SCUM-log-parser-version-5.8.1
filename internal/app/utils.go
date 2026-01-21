package app

import (
	"io"
	"os"
)

// extractPartialFile extracts a portion of a file starting from offset
// This function is used by both killfeed.go and logs.go
func extractPartialFile(source, dest string, offset int64) error {
	srcFile, err := os.Open(source)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	if _, err := srcFile.Seek(offset, 0); err != nil {
		return err
	}

	dstFile, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	// Use buffer from pool for efficiency
	buffer := GlobalBufferPool.Get()
	defer GlobalBufferPool.Put(buffer)

	_, err = io.CopyBuffer(dstFile, srcFile, buffer)
	return err
}

// min returns the minimum of two integers
// This function is used by killfeed.go
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Additional shared utility functions can be added here as needed
