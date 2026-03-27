package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
)

// RemoteFileInfo represents file information from remote server
type RemoteFileInfo struct {
	Name    string
	Size    int64
	ModTime int64
	IsDir   bool
}

// ==================== FILE LIST CACHE ====================
// Cache รายการไฟล์เพื่อหลีกเลี่ยง SFTP ReadDir ที่ช้า (30+ วินาที)

var (
	fileListCache      []RemoteFileInfo
	fileListCacheMux   sync.RWMutex
	fileListCacheTime  time.Time
	fileListCacheTTL   = 2 * time.Second // Cache TTL - refresh every 2 seconds for realtime
	fileListRefreshing bool
	fileListRefreshMux sync.Mutex
)

// GetCachedFileList returns cached file list or refreshes if expired
func GetCachedFileList() ([]RemoteFileInfo, error) {
	fileListCacheMux.RLock()
	cacheValid := time.Since(fileListCacheTime) < fileListCacheTTL && len(fileListCache) > 0
	cachedFiles := fileListCache
	hasCachedFiles := len(fileListCache) > 0
	fileListCacheMux.RUnlock()

	if cacheValid {
		return cachedFiles, nil
	}

	// Need to refresh - but only one goroutine should do it
	fileListRefreshMux.Lock()
	if fileListRefreshing {
		fileListRefreshMux.Unlock()
		// Another goroutine is refreshing, wait for it to complete
		// Instead of returning error, wait with timeout
		waitStart := time.Now()
		maxWait := 10 * time.Second
		for {
			time.Sleep(100 * time.Millisecond)

			fileListCacheMux.RLock()
			newCacheValid := time.Since(fileListCacheTime) < fileListCacheTTL && len(fileListCache) > 0
			newCachedFiles := fileListCache
			fileListCacheMux.RUnlock()

			if newCacheValid {
				return newCachedFiles, nil
			}

			// Check if refresh is still in progress
			fileListRefreshMux.Lock()
			stillRefreshing := fileListRefreshing
			fileListRefreshMux.Unlock()

			if !stillRefreshing {
				// Refresh completed but cache still invalid, try again
				break
			}

			if time.Since(waitStart) > maxWait {
				// Timeout - return stale cache if available
				if hasCachedFiles {
					return cachedFiles, nil
				}
				return nil, fmt.Errorf("cache refresh timeout after %v", maxWait)
			}
		}
		// Retry from the beginning after refresh completed
		return GetCachedFileList()
	}
	fileListRefreshing = true
	fileListRefreshMux.Unlock()

	defer func() {
		fileListRefreshMux.Lock()
		fileListRefreshing = false
		fileListRefreshMux.Unlock()
	}()

	// Do the actual refresh
	files, err := listRemoteFilesNoCache()
	if err != nil {
		// Return stale cache on error if available
		if hasCachedFiles {
			log.Printf("⚠️ Cache refresh failed: %v, using stale cache", err)
			return cachedFiles, nil
		}
		return nil, err
	}

	// Update cache
	fileListCacheMux.Lock()
	fileListCache = files
	fileListCacheTime = time.Now()
	fileListCacheMux.Unlock()

	log.Printf("🔄 File list cache refreshed: %d files", len(files))
	return files, nil
}

// RefreshFileListCache forces a cache refresh in background
func RefreshFileListCache() {
	go func() {
		_, _ = GetCachedFileList()
	}()
}

// StartFileListCacheRefresher starts background cache refresher
func StartFileListCacheRefresher(ctx context.Context) {
	go func() {
		// Initial refresh
		GetCachedFileList()

		ticker := time.NewTicker(fileListCacheTTL)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				GetCachedFileList()
			}
		}
	}()
	log.Printf("✅ File list cache refresher started (TTL: %v)", fileListCacheTTL)
}

// GetRemoteFileStat gets file info using real-time check
// For SFTP: uses Stat (fast single file check)
// For FTP: uses SIZE command for real-time size, falls back to cache for modtime
func GetRemoteFileStat(filename string) (*RemoteFileInfo, error) {
	if Config.ConnectionType == "FTP" {
		// For FTP, use SIZE command for real-time size check
		// Use Multi-FTP Pool for parallel operations
		conn, release, err := GetMultiFTPConnection()
		if err != nil {
			// Fall back to cache if connection fails
			files, cacheErr := GetCachedFileList()
			if cacheErr != nil {
				return nil, err
			}
			for _, f := range files {
				if f.Name == filename {
					return &f, nil
				}
			}
			return nil, fmt.Errorf("file not found: %s", filename)
		}

		remotePath := path.Join(Config.RemoteLogsPath, filename)
		size, err := conn.FileSize(remotePath)
		release() // Release immediately after SIZE command

		if err != nil {
			// Fall back to cache if SIZE command fails
			files, cacheErr := GetCachedFileList()
			if cacheErr != nil {
				return nil, err
			}
			for _, f := range files {
				if f.Name == filename {
					return &f, nil
				}
			}
			return nil, fmt.Errorf("file not found: %s", filename)
		}

		// Get modtime from cache (FTP doesn't have fast modtime check)
		var modTime int64 = 0
		files, _ := GetCachedFileList()
		for _, f := range files {
			if f.Name == filename {
				modTime = f.ModTime
				break
			}
		}

		return &RemoteFileInfo{
			Name:    filename,
			Size:    size,
			ModTime: modTime,
			IsDir:   false,
		}, nil
	}

	// SFTP - use Stat for fast single file check
	conn, release, err := GetMultiSFTPConnection()
	if err != nil {
		return nil, err
	}
	defer release()

	remotePath := Config.RemoteLogsPath + "/" + filename
	info, err := conn.Stat(remotePath)
	if err != nil {
		return nil, err
	}

	return &RemoteFileInfo{
		Name:    info.Name(),
		Size:    info.Size(),
		ModTime: info.ModTime().Unix(),
		IsDir:   info.IsDir(),
	}, nil
}

// InitializeRemoteConnections sets up FTP/SFTP based on configuration
func InitializeRemoteConnections() {
	// Check if remote configuration is provided
	if Config.FTPHost == "" || Config.FTPUser == "" || Config.FTPPassword == "" {
		log.Printf("⚠️ Remote connection not configured")
		Config.UseRemote = false
		Config.ConnectionType = ""
		return
	}

	// Determine protocol
	if Config.Protocol != "" {
		Config.ConnectionType = strings.ToUpper(Config.Protocol)
	} else {
		// Auto-detect based on port
		switch Config.FTPPort {
		case "21":
			Config.ConnectionType = "FTP"
		case "22":
			Config.ConnectionType = "SFTP"
		default:
			Config.ConnectionType = "SFTP" // Default to SFTP
		}
	}

	// Initialize appropriate connection pool
	switch Config.ConnectionType {
	case "FTP":
		// Use Multi-Pool for parallel connections (same as SFTP)
		poolSize := getEnvInt("FTP_POOL_SIZE", 4)
		InitMultiFTPPool(poolSize)
		Config.UseRemote = true
		log.Printf("✅ FTP mode initialized with %d parallel connections", poolSize)
	case "SFTP":
		// Use Multi-Pool for parallel connections
		poolSize := getEnvInt("SFTP_POOL_SIZE", 4)
		InitMultiSFTPPool(poolSize)
		Config.UseRemote = true
		log.Printf("✅ SFTP mode initialized with %d parallel connections", poolSize)
	default:
		log.Printf("❌ Unknown protocol: %s", Config.ConnectionType)
		Config.UseRemote = false
		Config.ConnectionType = ""
	}
}

// CloseRemoteConnections closes all remote connections
func CloseRemoteConnections() {
	if Config.UseRemote {
		switch Config.ConnectionType {
		case "FTP":
			CloseMultiFTPPool()
		case "SFTP":
			CloseMultiSFTPPool()
		}
	}
}

// GetLatestFileRemote finds the latest file matching pattern using FTP or SFTP
func GetLatestFileRemote(pattern string) (*RemoteFileInfo, string, error) {
	switch Config.ConnectionType {
	case "FTP":
		// Use Multi-FTP Pool for parallel operations
		conn, release, err := GetMultiFTPConnection()
		if err != nil {
			return nil, "", err
		}
		entry, path, err := GetLatestFileFTP(conn, pattern)
		release() // Release connection after use
		if err != nil {
			return nil, "", err
		}
		return &RemoteFileInfo{
			Name:    entry.Name,
			Size:    int64(entry.Size),
			ModTime: entry.Time.Unix(),
			IsDir:   entry.Type == ftp.EntryTypeFolder,
		}, path, nil

	case "SFTP":
		conn, release, err := GetMultiSFTPConnection()
		if err != nil {
			return nil, "", err
		}
		defer release()
		info, path, err := GetLatestFile(conn, pattern)
		if err != nil {
			return nil, "", err
		}
		return &RemoteFileInfo{
			Name:    info.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
			IsDir:   info.IsDir(),
		}, path, nil

	default:
		return nil, "", fmt.Errorf("no remote connection configured")
	}
}

// GetLatestKillFileRemote finds the latest kill_ file (excluding event_kill_)
// Uses cache system to avoid race conditions with FTP connection pool
func GetLatestKillFileRemote() (*RemoteFileInfo, string, error) {
	// Use unified cache system like other modules to avoid race conditions
	return GetLatestRemoteFile("kill")
}

// DownloadFileRemote downloads a file using FTP or SFTP
func DownloadFileRemote(remotePath, localPath string, maxRetries int) error {
	switch Config.ConnectionType {
	case "FTP":
		return downloadFileFTPWithRetry(remotePath, localPath, maxRetries)
	case "SFTP":
		return downloadFileWithRetry(remotePath, localPath, maxRetries)
	default:
		return fmt.Errorf("no remote connection configured")
	}
}

// ListRemoteFiles lists files using CACHE (fast - avoids 30+ second SFTP ReadDir)
func ListRemoteFiles() ([]RemoteFileInfo, error) {
	return GetCachedFileList()
}

// listRemoteFilesNoCache lists files directly without cache (SLOW - 30+ seconds!)
func listRemoteFilesNoCache() ([]RemoteFileInfo, error) {
	start := time.Now()
	defer func() {
		duration := time.Since(start)
		if duration > 500*time.Millisecond {
			log.Printf("⏱️ [CACHE REFRESH] listRemoteFilesNoCache took %v", duration)
		}
	}()

	switch Config.ConnectionType {
	case "FTP":
		// Use Multi-FTP Pool for parallel operations
		conn, release, err := GetMultiFTPConnection()
		if err != nil {
			return nil, err
		}
		defer release() // IMPORTANT: Release connection when done

		entries, err := conn.List(Config.RemoteLogsPath)
		if err != nil {
			return nil, err
		}

		files := make([]RemoteFileInfo, 0, len(entries))
		for _, entry := range entries {
			files = append(files, RemoteFileInfo{
				Name:    entry.Name,
				Size:    int64(entry.Size),
				ModTime: entry.Time.Unix(),
				IsDir:   entry.Type == ftp.EntryTypeFolder,
			})
		}
		return files, nil

	case "SFTP":
		startConn := time.Now()
		conn, release, err := GetMultiSFTPConnection()
		if time.Since(startConn) > 100*time.Millisecond {
			log.Printf("⏱️ [SLOW] GetMultiSFTPConnection took %v", time.Since(startConn))
		}
		if err != nil {
			return nil, err
		}
		defer release()

		startRead := time.Now()
		infos, err := conn.ReadDir(Config.RemoteLogsPath)
		if time.Since(startRead) > 100*time.Millisecond {
			log.Printf("⏱️ [CACHE REFRESH] SFTP ReadDir took %v", time.Since(startRead))
		}
		if err != nil {
			return nil, err
		}

		files := make([]RemoteFileInfo, 0, len(infos))
		for _, info := range infos {
			files = append(files, RemoteFileInfo{
				Name:    info.Name(),
				Size:    info.Size(),
				ModTime: info.ModTime().Unix(),
				IsDir:   info.IsDir(),
			})
		}
		return files, nil

	default:
		return nil, fmt.Errorf("no remote connection configured")
	}
}

// TestRemoteConnection tests the remote connection
func TestRemoteConnection() error {
	switch Config.ConnectionType {
	case "FTP":
		conn, release, err := GetMultiFTPConnection()
		if err != nil {
			return fmt.Errorf("FTP connection failed: %v", err)
		}
		defer release()
		// Test by listing directory
		_, err = conn.List(Config.RemoteLogsPath)
		if err != nil {
			return fmt.Errorf("FTP directory listing failed: %v", err)
		}
		return nil

	case "SFTP":
		conn, release, err := GetMultiSFTPConnection()
		if err != nil {
			return fmt.Errorf("SFTP connection failed: %v", err)
		}
		defer release()
		// Test by checking directory
		_, err = conn.Stat(Config.RemoteLogsPath)
		if err != nil {
			return fmt.Errorf("SFTP directory check failed: %v", err)
		}
		return nil

	default:
		return fmt.Errorf("no remote connection configured")
	}
}

// NeedsDownload checks if a file needs to be downloaded
func NeedsDownload(remotePath, localPath string, remoteFile *RemoteFileInfo) bool {
	localInfo, err := os.Stat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return true // File doesn't exist locally
		}
		return true // Error checking file, download anyway
	}

	// Check size and modification time
	if localInfo.Size() != remoteFile.Size {
		return true
	}

	// If remote file is newer
	if remoteFile.ModTime > localInfo.ModTime().Unix() {
		return true
	}

	return false
}

// GetRemoteFilePath constructs the full remote file path
func GetRemoteFilePath(filename string) string {
	// Use path.Join (not filepath.Join) for FTP/SFTP - always use forward slashes
	return path.Join(Config.RemoteLogsPath, filename)
}

// IsRemoteFile checks if a filename matches a specific pattern
func IsRemoteFile(filename, pattern string) bool {
	if pattern == "kill" {
		// Special handling for kill files
		return strings.HasPrefix(filename, "kill_") && !strings.HasPrefix(filename, "event_kill_")
	}
	return strings.Contains(filename, pattern)
}

// extractTimestampFromFilename extracts timestamp from filename like "economy_20251129090058.log"
// Returns the timestamp string for comparison (e.g., "20251129090058")
func extractTimestampFromFilename(filename string) string {
	// Remove extension
	name := strings.TrimSuffix(filename, ".log")

	// Find the last underscore and extract timestamp after it
	parts := strings.Split(name, "_")
	if len(parts) >= 2 {
		// Get the last part which should be the timestamp
		timestamp := parts[len(parts)-1]
		// Validate it looks like a timestamp (14 digits: YYYYMMDDHHMMSS)
		if len(timestamp) == 14 {
			return timestamp
		}
	}
	return ""
}

// GetLatestRemoteFile is a generic function to get the latest file of a specific type
// Returns the file with latest timestamp from filename (e.g., economy_20251129090058.log)
func GetLatestRemoteFile(fileType string) (*RemoteFileInfo, string, error) {
	files, err := ListRemoteFiles()
	if err != nil {
		return nil, "", err
	}

	var latestFile *RemoteFileInfo
	var latestTimestamp string

	for _, file := range files {
		if file.IsDir {
			continue
		}

		if IsRemoteFile(file.Name, fileType) {
			timestamp := extractTimestampFromFilename(file.Name)
			if timestamp > latestTimestamp {
				latestFile = &file
				latestTimestamp = timestamp
			}
		}
	}

	if latestFile == nil {
		return nil, "", fmt.Errorf("no %s files found", fileType)
	}

	remotePath := GetRemoteFilePath(latestFile.Name)
	return latestFile, remotePath, nil
}

// RemoteConnectionStats provides connection statistics
type RemoteConnectionStats struct {
	Protocol         string
	Host             string
	Port             string
	User             string
	Path             string
	ConnectionActive bool
	LastActivity     time.Time
}

// GetRemoteConnectionStats returns current connection statistics
func GetRemoteConnectionStats() RemoteConnectionStats {
	stats := RemoteConnectionStats{
		Protocol:         Config.ConnectionType,
		Host:             Config.FTPHost,
		Port:             Config.FTPPort,
		User:             Config.FTPUser,
		Path:             Config.RemoteLogsPath,
		ConnectionActive: false,
		LastActivity:     time.Time{},
	}

	// Check if connection is active
	switch Config.ConnectionType {
	case "FTP":
		if globalMultiFTPPool != nil {
			total, active, _, _ := GetFTPPoolStats()
			if total > 0 {
				stats.ConnectionActive = active > 0 || total > 0
			}
		}
	case "SFTP":
		if globalMultiPool != nil {
			total, active, _, _ := GetPoolStats()
			if total > 0 {
				stats.ConnectionActive = active > 0
			}
		}
	}

	return stats
}
