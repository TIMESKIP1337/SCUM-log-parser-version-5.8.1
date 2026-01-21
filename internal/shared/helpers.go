package shared

import (
	"io"
	"os"
)

// Shared helper functions used across multiple modules

// StringPtr returns a pointer to a string
func StringPtr(s string) *string {
	return &s
}

// IntPtr returns a pointer to an int
func IntPtr(i int) *int {
	return &i
}

// ExtractPartialFile extracts a portion of a file starting from offset
// This function is used by both killfeed.go and logs.go
func ExtractPartialFile(source, dest string, offset int64, bufferPool BufferPool) error {
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

	// Use buffer from pool for efficiency if provided
	if bufferPool != nil {
		buffer := bufferPool.Get()
		defer bufferPool.Put(buffer)
		_, err = io.CopyBuffer(dstFile, srcFile, buffer)
	} else {
		_, err = io.Copy(dstFile, srcFile)
	}

	return err
}

// Min returns the minimum of two integers
func Min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// BufferPool interface for buffer pooling
type BufferPool interface {
	Get() []byte
	Put([]byte)
}
