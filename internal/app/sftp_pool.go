package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SFTPPool manages a pool of SFTP connections with proper resource cleanup
type SFTPPool struct {
	mu          sync.Mutex
	conn        *sftp.Client
	sshConn     *ssh.Client
	lastUsed    time.Time
	maxIdleTime time.Duration

	// Context for graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc

	// Keep-alive control
	keepAliveCtx    context.Context
	keepAliveCancel context.CancelFunc
}

var (
	globalSFTPPool *SFTPPool
	sftpPoolOnce   sync.Once
)

// InitSFTPPool initializes the global SFTP connection pool
func InitSFTPPool() {
	sftpPoolOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())

		globalSFTPPool = &SFTPPool{
			maxIdleTime: 5 * time.Minute,
			ctx:         ctx,
			cancel:      cancel,
		}

		// Start connection monitor with context
		go globalSFTPPool.monitor()
		log.Println("✅ SFTP Connection Pool initialized")
	})
}

// GetSFTPConnection returns a reusable SFTP connection
func GetSFTPConnection() (*sftp.Client, error) {
	if globalSFTPPool == nil {
		InitSFTPPool()
	}

	globalSFTPPool.mu.Lock()
	defer globalSFTPPool.mu.Unlock()

	// Check if existing connection is still valid
	if globalSFTPPool.conn != nil {
		// Test connection with a simple operation
		if _, err := globalSFTPPool.conn.Stat("."); err == nil {
			globalSFTPPool.lastUsed = time.Now()
			log.Printf("♻️ Reusing existing SFTP connection")
			return globalSFTPPool.conn, nil
		}
		// Connection is dead, close it
		log.Printf("⚠️ Existing SFTP connection is dead, creating new one")
		globalSFTPPool.closeConnection()
	}

	// Create new connection
	log.Printf("🔄 Creating new SFTP connection...")
	sshConn, sftpConn, err := globalSFTPPool.createNewConnection()
	if err != nil {
		return nil, err
	}

	globalSFTPPool.conn = sftpConn
	globalSFTPPool.sshConn = sshConn
	globalSFTPPool.lastUsed = time.Now()

	UpdateSharedActivity()
	return sftpConn, nil
}

// createNewConnection creates a new SFTP connection with proper keep-alive
func (p *SFTPPool) createNewConnection() (*ssh.Client, *sftp.Client, error) {
	sshConfig := &ssh.ClientConfig{
		User: Config.FTPUser,
		Auth: []ssh.AuthMethod{
			ssh.Password(Config.FTPPassword),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}

	sshClient, err := ssh.Dial("tcp", fmt.Sprintf("%s:%s", Config.FTPHost, Config.FTPPort), sshConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("SSH connection failed: %v", err)
	}

	// Create cancellable context for keep-alive
	p.keepAliveCtx, p.keepAliveCancel = context.WithCancel(p.ctx)

	// Set up keep-alive with proper cancellation
	go p.runKeepAlive(sshClient)

	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		p.keepAliveCancel() // Cancel keep-alive
		sshClient.Close()
		return nil, nil, fmt.Errorf("SFTP client creation failed: %v", err)
	}

	log.Printf("✅ New SFTP connection established")
	return sshClient, sftpClient, nil
}

// runKeepAlive sends keep-alive packets with proper cancellation
func (p *SFTPPool) runKeepAlive(sshClient *ssh.Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.keepAliveCtx.Done():
			// Context cancelled, stop keep-alive
			return
		case <-ticker.C:
			if sshClient != nil {
				_, _, err := sshClient.SendRequest("keepalive@openssh.com", true, nil)
				if err != nil {
					// Connection error, stop keep-alive
					return
				}
			}
		}
	}
}

// closeConnection closes the current connection and cancels keep-alive
func (p *SFTPPool) closeConnection() {
	// Cancel keep-alive goroutine first
	if p.keepAliveCancel != nil {
		p.keepAliveCancel()
		p.keepAliveCancel = nil
	}

	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
	if p.sshConn != nil {
		p.sshConn.Close()
		p.sshConn = nil
	}
}

// monitor periodically checks and closes idle connections
func (p *SFTPPool) monitor() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			// Shutdown signal received
			return
		case <-ticker.C:
			p.mu.Lock()
			if p.conn != nil && time.Since(p.lastUsed) > p.maxIdleTime {
				log.Printf("🔌 Closing idle SFTP connection (idle for %.0f minutes)",
					time.Since(p.lastUsed).Minutes())
				p.closeConnection()
			}
			p.mu.Unlock()
		}
	}
}

// CloseSFTPPool closes all connections in the pool
func CloseSFTPPool() {
	if globalSFTPPool != nil {
		// Signal shutdown to monitor
		globalSFTPPool.cancel()

		globalSFTPPool.mu.Lock()
		globalSFTPPool.closeConnection()
		globalSFTPPool.mu.Unlock()

		log.Printf("🔌 SFTP connection pool closed")
	}
}

// downloadFileWithRetry downloads a file with automatic retry and memory-efficient buffering
// Uses Multi-Pool for parallel downloads
func downloadFileWithRetry(remotePath, localPath string, maxRetries int) error {
	start := time.Now()
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			log.Printf("🔄 Retry attempt %d/%d for downloading %s", attempt+1, maxRetries, remotePath)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}

		startConn := time.Now()
		sftpClient, release, err := GetMultiSFTPConnection()
		if time.Since(startConn) > 100*time.Millisecond {
			log.Printf("⏱️ [SLOW] GetMultiSFTPConnection for download took %v", time.Since(startConn))
		}
		if err != nil {
			lastErr = err
			continue
		}

		// Try to download
		startDownload := time.Now()
		err = downloadFile(sftpClient, remotePath, localPath)
		release() // Always release connection back to pool

		if err == nil {
			totalDuration := time.Since(start)
			if totalDuration > 1*time.Second {
				log.Printf("⏱️ [SLOW] Total download took %v (actual download: %v)", totalDuration, time.Since(startDownload))
			}
			return nil // Success
		}

		lastErr = err
		log.Printf("⚠️ Download failed: %v", err)
	}

	return fmt.Errorf("download failed after %d attempts: %v", maxRetries, lastErr)
}

// Helper function to check if error is connection-related
func isSFTPConnectionError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return contains(errStr, []string{
		"connection",
		"timeout",
		"EOF",
		"broken pipe",
		"reset by peer",
		"no route to host",
		"network is unreachable",
	})
}

func contains(s string, substrs []string) bool {
	for _, substr := range substrs {
		if strings.Contains(strings.ToLower(s), substr) {
			return true
		}
	}
	return false
}

// downloadFile performs the actual file download with memory-efficient buffering
func downloadFile(sftpClient *sftp.Client, remotePath, localPath string) error {
	// Ensure directory exists
	dir := filepath.Dir(localPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}

	// Check if target file is being used
	if _, err := os.Stat(localPath); err == nil {
		// Try to open file exclusively to check if it's in use
		testFile, err := os.OpenFile(localPath, os.O_RDWR|os.O_EXCL, 0644)
		if err != nil {
			// File is in use, wait a bit
			log.Printf("⚠️ Target file %s is in use, waiting...", filepath.Base(localPath))
			time.Sleep(2 * time.Second)

			// Try again
			testFile, err = os.OpenFile(localPath, os.O_RDWR|os.O_EXCL, 0644)
			if err != nil {
				return fmt.Errorf("target file is locked: %v", err)
			}
		}
		testFile.Close()
	}

	// Open remote file
	remoteFile, err := sftpClient.Open(remotePath)
	if err != nil {
		return fmt.Errorf("failed to open remote file: %v", err)
	}
	defer remoteFile.Close()

	// Get file info
	stat, err := remoteFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat remote file: %v", err)
	}

	// Check file size
	const maxFileSize = 100 * 1024 * 1024 // 100MB
	if stat.Size() > maxFileSize {
		return fmt.Errorf("file too large: %.2f MB (max: %.0f MB)",
			float64(stat.Size())/1024/1024, float64(maxFileSize)/1024/1024)
	}

	// Create temp file with unique name
	tempPath := fmt.Sprintf("%s.tmp.%d", localPath, time.Now().UnixNano())
	tempFile, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %v", err)
	}

	// Ensure cleanup
	defer func() {
		tempFile.Close()
		// Always try to remove temp file
		if _, err := os.Stat(tempPath); err == nil {
			os.Remove(tempPath)
		}
	}()

	// Use buffer from pool to reduce memory allocations
	buffer := GlobalBufferPool.Get()
	defer GlobalBufferPool.Put(buffer)

	var totalBytes int64
	startTime := time.Now()
	lastProgress := time.Now()

	for {
		n, err := remoteFile.Read(buffer)
		if n > 0 {
			_, writeErr := tempFile.Write(buffer[:n])
			if writeErr != nil {
				return fmt.Errorf("failed to write to temp file: %v", writeErr)
			}
			totalBytes += int64(n)

			// Progress update every second
			if time.Since(lastProgress) > time.Second {
				progress := float64(totalBytes) / float64(stat.Size()) * 100
				speed := float64(totalBytes) / time.Since(startTime).Seconds() / 1024 / 1024
				log.Printf("📥 Downloading %s: %.1f%% (%.2f MB/s)",
					filepath.Base(remotePath), progress, speed)
				lastProgress = time.Now()
				UpdateSharedActivity()
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read from remote file: %v", err)
		}
	}

	// Close temp file before operations
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %v", err)
	}

	// If target file exists, remove it first (with retry)
	if _, err := os.Stat(localPath); err == nil {
		for i := 0; i < 3; i++ {
			err = os.Remove(localPath)
			if err == nil {
				break
			}
			log.Printf("⚠️ Failed to remove existing file (attempt %d/3): %v", i+1, err)
			time.Sleep(time.Duration(i+1) * time.Second)
		}
		if err != nil {
			return fmt.Errorf("failed to remove existing file after 3 attempts: %v", err)
		}
	}

	// Rename temp file to final destination (with retry)
	var renameErr error
	for i := 0; i < 3; i++ {
		renameErr = os.Rename(tempPath, localPath)
		if renameErr == nil {
			break
		}
		log.Printf("⚠️ Failed to rename file (attempt %d/3): %v", i+1, renameErr)
		time.Sleep(time.Duration(i+1) * time.Second)
	}

	if renameErr != nil {
		// If rename still fails, try copy as last resort
		log.Printf("⚠️ Rename failed, trying copy instead")

		src, err := os.Open(tempPath)
		if err != nil {
			return fmt.Errorf("failed to open temp file for copy: %v", err)
		}
		defer src.Close()

		dst, err := os.Create(localPath)
		if err != nil {
			return fmt.Errorf("failed to create destination file: %v", err)
		}
		defer dst.Close()

		if _, err := io.Copy(dst, src); err != nil {
			return fmt.Errorf("failed to copy file: %v", err)
		}

		// Try to remove temp file
		os.Remove(tempPath)
	}

	duration := time.Since(startTime)
	avgSpeed := float64(totalBytes) / duration.Seconds() / 1024 / 1024
	log.Printf("✅ Download completed: %s (%.2f MB in %.1fs, %.2f MB/s)",
		filepath.Base(remotePath), float64(totalBytes)/1024/1024, duration.Seconds(), avgSpeed)

	return nil
}

// GetLatestFile finds the latest file matching a pattern
func GetLatestFile(sftpClient *sftp.Client, pattern string) (os.FileInfo, string, error) {
	files, err := sftpClient.ReadDir(Config.RemoteLogsPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read directory: %v", err)
	}

	var latestFile os.FileInfo
	for _, file := range files {
		if !file.IsDir() {
			// Special handling for kill pattern
			if pattern == "kill" {
				// Must start with "kill_" but NOT "event_kill_"
				if strings.HasPrefix(file.Name(), "kill_") && !strings.HasPrefix(file.Name(), "event_kill_") {
					if latestFile == nil || file.ModTime().After(latestFile.ModTime()) {
						latestFile = file
					}
				}
			} else {
				// For other patterns, use simple contains
				if strings.Contains(file.Name(), pattern) {
					if latestFile == nil || file.ModTime().After(latestFile.ModTime()) {
						latestFile = file
					}
				}
			}
		}
	}

	if latestFile == nil {
		if pattern == "kill" {
			return nil, "", fmt.Errorf("no kill_ files found (excluding event_kill_)")
		}
		return nil, "", fmt.Errorf("no %s files found", pattern)
	}

	// Use path.Join for remote paths (always forward slashes)
	remotePath := path.Join(Config.RemoteLogsPath, latestFile.Name())
	return latestFile, remotePath, nil
}
