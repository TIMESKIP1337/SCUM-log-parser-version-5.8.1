package app

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
)

// FTPPool manages a pool of FTP connections with exclusive access
type FTPPool struct {
	mu          sync.Mutex
	conn        *ftp.ServerConn
	lastUsed    time.Time
	maxIdleTime time.Duration
	inUse       bool // Track if connection is currently in use
}

var (
	globalFTPPool *FTPPool
	ftpPoolOnce   sync.Once
)

// InitFTPPool initializes the global FTP connection pool
func InitFTPPool() {
	ftpPoolOnce.Do(func() {
		globalFTPPool = &FTPPool{
			maxIdleTime: 30 * time.Minute,
		}

		// Start connection monitor
		go globalFTPPool.monitor()
		// Start keepalive to prevent server timeout
		go globalFTPPool.keepAlive()
		log.Println("✅ FTP Connection Pool initialized with 30-minute timeout and keepalive")
	})
}

// GetFTPConnection is DEPRECATED - use GetFTPConnectionExclusive instead
// This function waits for exclusive operations to complete but doesn't lock for itself
// Use for quick read-only operations that don't need exclusive access
func GetFTPConnection() (*ftp.ServerConn, error) {
	if globalFTPPool == nil {
		InitFTPPool()
	}

	globalFTPPool.mu.Lock()

	// Wait if connection is in use by exclusive operation (with timeout)
	waitStart := time.Now()
	maxWait := 30 * time.Second
	for globalFTPPool.inUse {
		globalFTPPool.mu.Unlock()
		if time.Since(waitStart) > maxWait {
			return nil, fmt.Errorf("timeout waiting for FTP connection (waited %v)", maxWait)
		}
		time.Sleep(100 * time.Millisecond)
		globalFTPPool.mu.Lock()
	}

	// Check if existing connection is still valid
	if globalFTPPool.conn != nil {
		// Test connection with NOOP command
		err := globalFTPPool.conn.NoOp()
		if err == nil {
			globalFTPPool.lastUsed = time.Now()
			conn := globalFTPPool.conn
			globalFTPPool.mu.Unlock()
			return conn, nil
		}
		// Connection is dead, close it
		log.Printf("⚠️ Existing FTP connection is dead (%v), creating new one", err)
		globalFTPPool.closeConnection()
	}

	// Create new connection
	log.Printf("🔄 Creating new FTP connection...")
	conn, err := createNewFTPConnection()
	if err != nil {
		globalFTPPool.mu.Unlock()
		return nil, err
	}

	globalFTPPool.conn = conn
	globalFTPPool.lastUsed = time.Now()
	globalFTPPool.mu.Unlock()

	UpdateSharedActivity()
	return conn, nil
}

// GetFTPConnectionExclusive returns a FTP connection with exclusive access
// Returns: connection, release function, error
// IMPORTANT: Caller MUST call the release function when done!
func GetFTPConnectionExclusive() (*ftp.ServerConn, func(), error) {
	if globalFTPPool == nil {
		InitFTPPool()
	}

	globalFTPPool.mu.Lock()

	// Wait if connection is in use (with timeout)
	waitStart := time.Now()
	maxWait := 30 * time.Second
	for globalFTPPool.inUse {
		globalFTPPool.mu.Unlock()
		if time.Since(waitStart) > maxWait {
			return nil, nil, fmt.Errorf("timeout waiting for FTP connection (waited %v)", maxWait)
		}
		time.Sleep(100 * time.Millisecond)
		globalFTPPool.mu.Lock()
	}

	// Mark as in use
	globalFTPPool.inUse = true

	// Check if existing connection is still valid
	if globalFTPPool.conn != nil {
		// Test connection with NOOP command
		err := globalFTPPool.conn.NoOp()
		if err == nil {
			globalFTPPool.lastUsed = time.Now()
			conn := globalFTPPool.conn
			globalFTPPool.mu.Unlock()

			// Return connection with release function
			return conn, func() {
				globalFTPPool.mu.Lock()
				globalFTPPool.inUse = false
				globalFTPPool.mu.Unlock()
			}, nil
		}
		// Connection is dead, close it
		log.Printf("⚠️ Existing FTP connection is dead (%v), creating new one", err)
		globalFTPPool.closeConnection()
	}

	// Create new connection
	log.Printf("🔄 Creating new FTP connection...")
	conn, err := createNewFTPConnection()
	if err != nil {
		globalFTPPool.inUse = false
		globalFTPPool.mu.Unlock()
		return nil, nil, err
	}

	globalFTPPool.conn = conn
	globalFTPPool.lastUsed = time.Now()
	globalFTPPool.mu.Unlock()

	UpdateSharedActivity()

	// Return connection with release function
	return conn, func() {
		globalFTPPool.mu.Lock()
		globalFTPPool.inUse = false
		globalFTPPool.mu.Unlock()
	}, nil
}

// createNewFTPConnection creates a new FTP connection
func createNewFTPConnection() (*ftp.ServerConn, error) {
	address := fmt.Sprintf("%s:%s", Config.FTPHost, Config.FTPPort)

	// Check if user wants to force TLS mode
	forceTLS := os.Getenv("FTP_FORCE_TLS") == "true"
	forcePlain := os.Getenv("FTP_FORCE_PLAIN") == "true"

	var conn *ftp.ServerConn
	var err error

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         Config.FTPHost,
		MinVersion:         tls.VersionTLS12,
	}

	if forceTLS {
		// Force Explicit FTPS mode
		log.Printf("🔒 Attempting Explicit FTPS connection to %s (forced)...", address)
		conn, err = ftp.Dial(address,
			ftp.DialWithTimeout(30*time.Second),
			ftp.DialWithExplicitTLS(tlsConfig),
			ftp.DialWithDisabledEPSV(true),
		)
		if err != nil {
			return nil, fmt.Errorf("FTPS connection failed: %v", err)
		}
		log.Printf("✅ Connected via Explicit FTPS (TLS)")
	} else if forcePlain {
		// Force plain FTP mode
		log.Printf("🔄 Attempting plain FTP connection to %s (forced)...", address)
		conn, err = ftp.Dial(address,
			ftp.DialWithTimeout(30*time.Second),
			ftp.DialWithDisabledEPSV(true),
		)
		if err != nil {
			return nil, fmt.Errorf("plain FTP connection failed: %v", err)
		}
		log.Printf("✅ Connected via plain FTP")
	} else {
		// Auto mode: Try plain FTP first, then Explicit FTPS
		log.Printf("🔄 Attempting plain FTP connection to %s...", address)
		conn, err = ftp.Dial(address,
			ftp.DialWithTimeout(30*time.Second),
			ftp.DialWithDisabledEPSV(true),
		)

		if err != nil {
			// Try with Explicit FTPS as fallback
			log.Printf("⚠️ Plain FTP failed (%v), trying Explicit FTPS...", err)

			conn, err = ftp.Dial(address,
				ftp.DialWithTimeout(30*time.Second),
				ftp.DialWithExplicitTLS(tlsConfig),
				ftp.DialWithDisabledEPSV(true),
			)
			if err != nil {
				return nil, fmt.Errorf("FTP connection failed (both plain and TLS): %v", err)
			}
			log.Printf("✅ Connected via Explicit FTPS (TLS)")
		} else {
			log.Printf("✅ Connected via plain FTP")
		}
	}

	// Login
	err = conn.Login(Config.FTPUser, Config.FTPPassword)
	if err != nil {
		conn.Quit()
		return nil, fmt.Errorf("FTP login failed: %v", err)
	}

	// Set binary mode
	err = conn.Type(ftp.TransferTypeBinary)
	if err != nil {
		log.Printf("⚠️ Failed to set binary mode: %v", err)
	}

	log.Printf("✅ New FTP connection established")
	return conn, nil
}

// closeConnection closes the current connection
func (p *FTPPool) closeConnection() {
	if p.conn != nil {
		p.conn.Quit()
		p.conn = nil
	}
}

// monitor periodically checks and closes idle connections
func (p *FTPPool) monitor() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		p.mu.Lock()
		// Only close if not in use and idle for too long
		if p.conn != nil && !p.inUse && time.Since(p.lastUsed) > p.maxIdleTime {
			log.Printf("🔌 Closing idle FTP connection (idle for %.0f minutes)",
				time.Since(p.lastUsed).Minutes())
			p.closeConnection()
		}
		p.mu.Unlock()
	}
}

// keepAlive sends NOOP commands periodically to prevent server timeout
func (p *FTPPool) keepAlive() {
	ticker := time.NewTicker(2 * time.Minute) // Send NOOP every 2 minutes
	defer ticker.Stop()

	for range ticker.C {
		p.mu.Lock()
		// Only send keepalive if connection exists and is NOT in use
		if p.conn != nil && !p.inUse {
			// Send NOOP to keep connection alive
			if err := p.conn.NoOp(); err != nil {
				log.Printf("⚠️ FTP keepalive failed: %v, will reconnect on next use", err)
				p.closeConnection()
			}
			// Remove log spam for successful keepalive
		}
		p.mu.Unlock()
	}
}

// CloseFTPPool closes all connections in the pool
func CloseFTPPool() {
	if globalFTPPool != nil {
		globalFTPPool.mu.Lock()
		defer globalFTPPool.mu.Unlock()
		globalFTPPool.closeConnection()
		log.Printf("🔌 FTP connection pool closed")
	}
}

// FTP specific functions

// GetLatestFileFTP finds the latest file matching a pattern via FTP
func GetLatestFileFTP(conn *ftp.ServerConn, pattern string) (*ftp.Entry, string, error) {
	entries, err := conn.List(Config.RemoteLogsPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to list directory: %v", err)
	}

	var latestFile *ftp.Entry
	var latestTime time.Time

	for _, entry := range entries {
		if entry.Type == ftp.EntryTypeFile {
			// Special handling for kill pattern
			if pattern == "kill" {
				if strings.HasPrefix(entry.Name, "kill_") && !strings.HasPrefix(entry.Name, "event_kill_") {
					if entry.Time.After(latestTime) {
						latestFile = entry
						latestTime = entry.Time
					}
				}
			} else {
				// For other patterns, use simple contains
				if strings.Contains(entry.Name, pattern) {
					if entry.Time.After(latestTime) {
						latestFile = entry
						latestTime = entry.Time
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

	remotePath := path.Join(Config.RemoteLogsPath, latestFile.Name)
	return latestFile, remotePath, nil
}

// downloadFileFTPWithRetry downloads a file via FTP with automatic retry
func downloadFileFTPWithRetry(remotePath, localPath string, maxRetries int) error {
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			log.Printf("🔄 Retry attempt %d/%d for downloading %s", attempt+1, maxRetries, remotePath)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}

		// Get connection from Multi-FTP Pool for parallel operations
		conn, release, err := GetMultiFTPConnection()
		if err != nil {
			lastErr = err
			continue
		}

		// Try to download
		err = downloadFileFTP(conn, remotePath, localPath)
		release() // IMPORTANT: Release connection after download

		if err == nil {
			return nil // Success
		}

		lastErr = err
		log.Printf("⚠️ Download failed: %v", err)
	}

	return fmt.Errorf("download failed after %d attempts: %v", maxRetries, lastErr)
}

// downloadFileFTP performs the actual file download via FTP
func downloadFileFTP(conn *ftp.ServerConn, remotePath, localPath string) error {
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
	resp, err := conn.Retr(remotePath)
	if err != nil {
		return fmt.Errorf("failed to retrieve file: %v", err)
	}
	defer resp.Close()

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

	// Download with progress tracking
	buffer := make([]byte, 32*1024) // 32KB buffer
	var totalBytes int64
	startTime := time.Now()
	lastProgress := time.Now()

	for {
		n, err := resp.Read(buffer)
		if n > 0 {
			_, writeErr := tempFile.Write(buffer[:n])
			if writeErr != nil {
				return fmt.Errorf("failed to write to temp file: %v", writeErr)
			}
			totalBytes += int64(n)

			// Progress update every second
			if time.Since(lastProgress) > time.Second {
				speed := float64(totalBytes) / time.Since(startTime).Seconds() / 1024 / 1024
				log.Printf("📥 Downloading %s: %.2f MB downloaded (%.2f MB/s)",
					filepath.Base(remotePath), float64(totalBytes)/1024/1024, speed)
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

// GetConnectionFTP is a wrapper to get appropriate connection based on protocol
func GetConnectionFTP() (*ftp.ServerConn, error) {
	return GetFTPConnection()
}

// GetLatestKillFileFTP finds the latest kill_ file via FTP
func GetLatestKillFileFTP(conn *ftp.ServerConn) (*ftp.Entry, string, error) {
	return GetLatestFileFTP(conn, "kill")
}

// Helper function to check if error is connection-related
func isFTPConnectionError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	connectionErrors := []string{
		"connection",
		"timeout",
		"EOF",
		"broken pipe",
		"reset by peer",
		"no route to host",
		"network is unreachable",
	}

	for _, connErr := range connectionErrors {
		if strings.Contains(strings.ToLower(errStr), connErr) {
			return true
		}
	}
	return false
}
