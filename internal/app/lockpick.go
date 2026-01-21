package app

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
	_ "modernc.org/sqlite"
)

// Lockpicking module specific variables
var (
	lockpickingFileWatcher *fsnotify.Watcher
	lockpickingFileOffsets = make(map[string]int64)
	lockpickingOffsetMux   sync.RWMutex

	// Stream processor for large files
	lockpickingStreamProcessor *StreamingFileProcessor

	// SQLite database
	lockpickingDB *sql.DB
	dbMux         sync.RWMutex

	// Ranking management
	lastRankingUpdate time.Time
	rankingUpdateMux  sync.RWMutex

	// Auto reset tracking
	lastWeeklyReset time.Time
	weeklyResetMux  sync.RWMutex
)

// LockpickingRecord represents a lockpicking attempt
type LockpickingRecord struct {
	ID             int       `json:"id"`
	SteamID        string    `json:"steam_id"`
	UserName       string    `json:"user_name"`
	LockType       string    `json:"lock_type"`
	Success        bool      `json:"success"`
	ElapsedTime    float64   `json:"elapsed_time"`
	FailedAttempts int       `json:"failed_attempts"`
	TargetObject   string    `json:"target_object"`
	TargetID       string    `json:"target_id"`
	UserOwner      string    `json:"user_owner"`
	LocationX      float64   `json:"location_x"`
	LocationY      float64   `json:"location_y"`
	LocationZ      float64   `json:"location_z"`
	CreatedAt      time.Time `json:"created_at"`
}

// Initialize lockpicking module
func InitializeLockpicking() error {
	// Initialize stream processor
	lockpickingStreamProcessor = NewStreamingFileProcessor(
		32*1024, // 32KB buffer
		100,     // Max 100 items per batch
		5,       // Max 5 concurrent batches
	)

	// Initialize SQLite database
	if err := initLockpickingDatabase(); err != nil {
		return fmt.Errorf("failed to initialize lockpicking database: %v", err)
	}

	// Load file offsets if using local files with file watcher
	if Config.UseLocalFiles && Config.EnableFileWatcher {
		if err := loadLockpickingFileOffsets(); err != nil {
			log.Printf("Warning: Could not load lockpicking file offsets: %v", err)
		}
	}

	// Load last weekly reset time
	loadLastWeeklyResetTime()

	log.Println("✅ Lockpicking module initialized with SQLite database and UTF-8 support")
	return nil
}

// Initialize SQLite database for lockpicking with enhanced UTF-8 support
func initLockpickingDatabase() error {
	dbMux.Lock()
	defer dbMux.Unlock()

	// Create database directory if it doesn't exist
	dbDir := "database"
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return fmt.Errorf("failed to create database directory: %v", err)
	}

	dbPath := filepath.Join(dbDir, "lockpicking.db")
	var err error

	// Enhanced UTF-8 support for SQLite connection
	connectionString := dbPath + "?_pragma=encoding(UTF8)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	lockpickingDB, err = sql.Open("sqlite", connectionString)
	if err != nil {
		return fmt.Errorf("failed to open database: %v", err)
	}

	// Test connection
	if err := lockpickingDB.Ping(); err != nil {
		return fmt.Errorf("failed to ping database: %v", err)
	}

	// Set comprehensive UTF-8 configuration
	pragmas := []string{
		"PRAGMA encoding = 'UTF-8'",
		"PRAGMA case_sensitive_like = true", // Better Unicode handling
		"PRAGMA temp_store = memory",        // Better performance
		"PRAGMA cache_size = -64000",        // 64MB cache
	}

	for _, pragma := range pragmas {
		if _, err := lockpickingDB.Exec(pragma); err != nil {
			log.Printf("Warning: Could not set pragma %s: %v", pragma, err)
		}
	}

	// Create tables with explicit UTF-8 collation
	if err := createLockpickingTables(); err != nil {
		return fmt.Errorf("failed to create tables: %v", err)
	}

	log.Printf("✅ SQLite database initialized at %s with enhanced UTF-8 support", dbPath)
	return nil
}

// Create lockpicking tables with UTF-8 collation
func createLockpickingTables() error {
	// Create lockpicking_stats table with proper UTF-8 handling
	_, err := lockpickingDB.Exec(`
		CREATE TABLE IF NOT EXISTS lockpicking_stats (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			steam_id TEXT NOT NULL COLLATE NOCASE,
			user_name TEXT NOT NULL COLLATE NOCASE,
			lock_type TEXT NOT NULL CHECK (lock_type IN ('VeryEasy', 'Basic', 'Medium', 'Advanced')) COLLATE NOCASE,
			success_count INTEGER DEFAULT 0,
			attempt_count INTEGER DEFAULT 0,
			total_time REAL DEFAULT 0.0,
			total_failed_attempts INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(steam_id, lock_type)
		)
	`)
	if err != nil {
		return err
	}

	// Create lockpicking_target_objects table with UTF-8 support
	_, err = lockpickingDB.Exec(`
		CREATE TABLE IF NOT EXISTS lockpicking_target_objects (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			steam_id TEXT NOT NULL COLLATE NOCASE,
			target_object TEXT NOT NULL COLLATE NOCASE,
			count INTEGER DEFAULT 1,
			UNIQUE(steam_id, target_object)
		)
	`)
	if err != nil {
		return err
	}

	// Create indexes for better performance
	indexes := []string{
		"CREATE INDEX IF NOT EXISTS idx_lockpicking_steam_id ON lockpicking_stats(steam_id)",
		"CREATE INDEX IF NOT EXISTS idx_lockpicking_lock_type ON lockpicking_stats(lock_type)",
		"CREATE INDEX IF NOT EXISTS idx_lockpicking_success ON lockpicking_stats(success_count DESC)",
		"CREATE INDEX IF NOT EXISTS idx_lockpicking_user_name ON lockpicking_stats(user_name COLLATE NOCASE)",
	}

	for _, indexSQL := range indexes {
		if _, err := lockpickingDB.Exec(indexSQL); err != nil {
			return err
		}
	}

	log.Println("✅ Lockpicking database tables created successfully with UTF-8 collation")
	return nil
}

// Start lockpicking module
func StartLockpicking(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [LOCKPICKING START] Panic recovered: %v\n%s", r, debug.Stack())
			UpdateSharedActivity()
		}
	}()

	// Start file watcher if enabled
	if Config.UseLocalFiles && Config.EnableFileWatcher {
		startLockpickingFileWatcher(ctx)
	}

	// Start periodic lockpicking checking
	go lockpickingPeriodicLogCheck(ctx)

	// Start progress monitor
	go monitorLockpickingProgress(ctx)

	// Start ranking system
	go lockpickingRankingSystem(ctx)

	// Start weekly auto-reset scheduler
	go weeklyAutoResetScheduler(ctx)

	log.Printf("✅ Lockpicking module started with SQLite database and UTF-8 support!")
}

// Weekly auto-reset scheduler - runs every Sunday at midnight
func weeklyAutoResetScheduler(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [WEEKLY RESET] Panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(5 * time.Second)
			go weeklyAutoResetScheduler(ctx)
		}
	}()

	// Check every minute for the reset time
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	log.Printf("✅ Weekly auto-reset scheduler started - will reset every Monday at 00:00")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()

			// Check if it's Monday and past midnight (00:00 - 00:59)
			if now.Weekday() == time.Monday && now.Hour() == 0 {
				weeklyResetMux.RLock()
				lastReset := lastWeeklyReset
				weeklyResetMux.RUnlock()

				// Check if we haven't reset yet this week
				// If last reset was before this Monday's midnight, do reset
				mondayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

				if lastReset.Before(mondayMidnight) {
					log.Printf("🔄 Triggering weekly auto-reset at %s", now.Format("2006-01-02 15:04:05"))
					performWeeklyReset()

					// Update last reset time
					weeklyResetMux.Lock()
					lastWeeklyReset = now
					weeklyResetMux.Unlock()
					saveLastWeeklyResetTime()

					// Send notification to Discord if channel is configured
					if Config.LockpickRankingChannelID != "" && SharedSession != nil {
						embed := &discordgo.MessageEmbed{
							Title:       "🔄 Weekly Lockpicking Stats Reset",
							Description: "สถิติการไขล็อคทั้งหมดถูกรีเซ็ตแล้ว (รีเซ็ตอัตโนมัติทุกวันจันทร์เวลา 00:00)",
							Color:       0xFF6B6B,
							Timestamp:   now.Format(time.RFC3339),
							Footer: &discordgo.MessageEmbedFooter{
								Text: "Auto Reset System",
							},
						}
						SharedSession.ChannelMessageSendEmbed(Config.LockpickRankingChannelID, embed)
					}
				}
			}
		}
	}
}

// Perform weekly reset - resets all lockpicking stats
func performWeeklyReset() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [WEEKLY RESET] Error during reset: %v", r)
		}
	}()

	dbMux.Lock()
	defer dbMux.Unlock()

	// Create backup table with timestamp
	backupTable := fmt.Sprintf("lockpicking_stats_backup_%s", time.Now().Format("20060102_150405"))

	// Backup lockpicking_stats
	_, err := lockpickingDB.Exec(fmt.Sprintf("CREATE TABLE %s AS SELECT * FROM lockpicking_stats", backupTable))
	if err != nil {
		log.Printf("❌ Error creating backup for lockpicking_stats: %v", err)
	} else {
		log.Printf("✅ Backup created: %s", backupTable)
	}

	// Backup lockpicking_target_objects
	backupObjectsTable := fmt.Sprintf("lockpicking_objects_backup_%s", time.Now().Format("20060102_150405"))
	_, err = lockpickingDB.Exec(fmt.Sprintf("CREATE TABLE %s AS SELECT * FROM lockpicking_target_objects", backupObjectsTable))
	if err != nil {
		log.Printf("❌ Error creating backup for lockpicking_target_objects: %v", err)
	} else {
		log.Printf("✅ Backup created: %s", backupObjectsTable)
	}

	// Reset all data in lockpicking_stats
	_, err = lockpickingDB.Exec("DELETE FROM lockpicking_stats")
	if err != nil {
		log.Printf("❌ Error resetting lockpicking_stats: %v", err)
		return
	}

	// Reset all data in lockpicking_target_objects
	_, err = lockpickingDB.Exec("DELETE FROM lockpicking_target_objects")
	if err != nil {
		log.Printf("❌ Error resetting lockpicking_target_objects: %v", err)
		return
	}

	// Reset the AUTOINCREMENT counter for clean IDs
	_, err = lockpickingDB.Exec("DELETE FROM sqlite_sequence WHERE name='lockpicking_stats'")
	if err != nil {
		log.Printf("Warning: Could not reset lockpicking_stats sequence: %v", err)
	}

	_, err = lockpickingDB.Exec("DELETE FROM sqlite_sequence WHERE name='lockpicking_target_objects'")
	if err != nil {
		log.Printf("Warning: Could not reset lockpicking_target_objects sequence: %v", err)
	}

	// Vacuum database to reclaim space
	_, err = lockpickingDB.Exec("VACUUM")
	if err != nil {
		log.Printf("Warning: Could not vacuum database: %v", err)
	}

	log.Printf("✅ Weekly auto-reset completed successfully - all lockpicking stats cleared")
	log.Printf("📦 Backups created: %s, %s", backupTable, backupObjectsTable)

	UpdateSharedActivity()
}

// Load last weekly reset time from file
func loadLastWeeklyResetTime() {
	weeklyResetMux.Lock()
	defer weeklyResetMux.Unlock()

	file, err := os.Open("last_weekly_reset.json")
	if err != nil {
		if os.IsNotExist(err) {
			// If file doesn't exist, set to a time in the past
			lastWeeklyReset = time.Now().AddDate(0, 0, -7)
			return
		}
		log.Printf("Warning: Could not load last weekly reset time: %v", err)
		lastWeeklyReset = time.Now().AddDate(0, 0, -7)
		return
	}
	defer file.Close()

	var resetData struct {
		LastReset time.Time `json:"last_reset"`
	}

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&resetData); err != nil {
		log.Printf("Warning: Could not decode last weekly reset time: %v", err)
		lastWeeklyReset = time.Now().AddDate(0, 0, -7)
		return
	}

	lastWeeklyReset = resetData.LastReset
	log.Printf("📅 Last weekly reset was at: %s", lastWeeklyReset.Format("2006-01-02 15:04:05"))
}

// Save last weekly reset time to file
func saveLastWeeklyResetTime() {
	weeklyResetMux.RLock()
	defer weeklyResetMux.RUnlock()

	resetData := struct {
		LastReset time.Time `json:"last_reset"`
	}{
		LastReset: lastWeeklyReset,
	}

	file, err := os.Create("last_weekly_reset.json")
	if err != nil {
		log.Printf("Warning: Could not save last weekly reset time: %v", err)
		return
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(resetData); err != nil {
		log.Printf("Warning: Could not encode last weekly reset time: %v", err)
	}
}

// Monitor progress from stream processor
func monitorLockpickingProgress(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case progress := <-lockpickingStreamProcessor.progressChan:
			log.Printf("📊 Lockpicking processing: %.1f%% complete", progress)
		}
	}
}

// startLockpickingRankingSystem - exported wrapper for unified scheduler
func startLockpickingRankingSystem(ctx context.Context) {
	lockpickingRankingSystem(ctx)
}

// startLockpickingWeeklyReset - exported wrapper for unified scheduler
func startLockpickingWeeklyReset(ctx context.Context) {
	weeklyAutoResetScheduler(ctx)
}

// Lockpicking ranking system
func lockpickingRankingSystem(ctx context.Context) {
	// Update immediately on start
	if Config.LockpickRankingChannelID != "" {
		log.Printf("🏆 Lockpick ranking: Initial update...")
		updateLockpickingRankings()
	}

	ticker := time.NewTicker(5 * time.Minute) // Update rankings every 5 minutes
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if Config.LockpickRankingChannelID != "" {
				updateLockpickingRankings()
			}
		}
	}
}

func loadLockpickingFileOffsets() error {
	lockpickingOffsetMux.Lock()
	defer lockpickingOffsetMux.Unlock()

	file, err := os.Open("lockpicking_file_offsets.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	return decoder.Decode(&lockpickingFileOffsets)
}

func saveLockpickingFileOffsets() error {
	lockpickingOffsetMux.RLock()
	defer lockpickingOffsetMux.RUnlock()

	file, err := os.Create("lockpicking_file_offsets.json")
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	return encoder.Encode(lockpickingFileOffsets)
}

// Remote Log Processing - HYBRID detection (Size OR ModTime)
func processLockpickingRemoteLogs() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [LOCKPICKING REMOTE] Panic recovered: %v", r)
			UpdateSharedActivity()
		}
	}()

	// Find latest gameplay file using unified remote system
	latestFile, remotePath, err := GetLatestRemoteFile("gameplay")
	if err != nil {
		log.Printf("Error finding lockpicking log files: %v", err)
		return
	}

	localDir := filepath.Join("logs", "lockpicking")
	os.MkdirAll(localDir, 0755)
	localPath := filepath.Join(localDir, latestFile.Name)

	// HYBRID check: download if Size increased OR ModTime changed
	needsDownload := true
	localInfo, err := os.Stat(localPath)
	if err == nil {
		localModTime := localInfo.ModTime().Unix()
		sizeChanged := localInfo.Size() < latestFile.Size
		modTimeChanged := latestFile.ModTime > localModTime

		if !sizeChanged && !modTimeChanged {
			needsDownload = false
		} else if sizeChanged {
			log.Printf("📥 Lockpicking: Size changed (local: %d, remote: %d bytes)",
				localInfo.Size(), latestFile.Size)
		} else if modTimeChanged {
			log.Printf("📥 Lockpicking: ModTime changed (local: %d, remote: %d)",
				localModTime, latestFile.ModTime)
		}
	}

	// Download if needed
	if !needsDownload {
		return
	}

	err = DownloadFileRemote(remotePath, localPath, 3)
	if err != nil {
		log.Printf("Error downloading lockpicking log: %v", err)
		return
	}

	// Process the file
	fileInfo, _ := os.Stat(localPath)
	if fileInfo.Size() > 10*1024*1024 { // If file > 10MB, use streaming
		log.Printf("📊 Processing large lockpicking log file (%.2f MB) with streaming",
			float64(fileInfo.Size())/1024/1024)
		err = lockpickingStreamProcessor.ProcessLargeFile(localPath, processLockpickingLines)
		if err != nil {
			log.Printf("Error processing lockpicking log file: %v", err)
			return
		}
	} else {
		// For smaller files, use traditional method
		lines, err := readLockpickingFileContent(localPath)
		if err != nil {
			log.Printf("Error reading lockpicking log file: %v", err)
			return
		}
		processLockpickingLines(lines)
	}
}

// Local File Functions for lockpicking
func findLockpickingLocalLogFiles() []string {
	pattern := filepath.Join(Config.LocalLogsPath, "*gameplay*")
	files, err := filepath.Glob(pattern)
	if err != nil {
		log.Printf("Error finding lockpicking log files: %v", err)
		return nil
	}
	return files
}

func getLockpickingLatestLocalLogFile() string {
	files := findLockpickingLocalLogFiles()
	if len(files) == 0 {
		return ""
	}

	var latestFile string
	var latestTime time.Time

	for _, file := range files {
		if info, err := os.Stat(file); err == nil {
			if info.ModTime().After(latestTime) {
				latestTime = info.ModTime()
				latestFile = file
			}
		}
	}

	return latestFile
}

// Enhanced file reading with better encoding detection
func readLockpickingFileContent(filePath string) ([]string, error) {
	// Read raw bytes first
	rawBytes, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	// Check for BOM and encoding
	var reader io.Reader = bytes.NewReader(rawBytes)
	var encoding string = "UTF-8"

	// Check for UTF-16 BOM
	if len(rawBytes) >= 2 {
		if rawBytes[0] == 0xFF && rawBytes[1] == 0xFE {
			// UTF-16 LE
			reader = transform.NewReader(bytes.NewReader(rawBytes[2:]), unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder())
			encoding = "UTF-16LE"
		} else if rawBytes[0] == 0xFE && rawBytes[1] == 0xFF {
			// UTF-16 BE
			reader = transform.NewReader(bytes.NewReader(rawBytes[2:]), unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewDecoder())
			encoding = "UTF-16BE"
		} else if len(rawBytes) >= 3 && rawBytes[0] == 0xEF && rawBytes[1] == 0xBB && rawBytes[2] == 0xBF {
			// UTF-8 BOM
			reader = bytes.NewReader(rawBytes[3:])
			encoding = "UTF-8 with BOM"
		}
	}

	// If no BOM detected, check for UTF-16 by looking for null bytes pattern
	if encoding == "UTF-8" && len(rawBytes) > 10 {
		nullCount := 0
		for i := 0; i < min(100, len(rawBytes)); i++ {
			if rawBytes[i] == 0 {
				nullCount++
			}
		}
		// If many null bytes, probably UTF-16
		if nullCount > 5 {
			// Try UTF-16 LE first (more common)
			reader = transform.NewReader(bytes.NewReader(rawBytes), unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder())
			encoding = "UTF-16LE (detected)"
		}
	}

	// Read lines
	scanner := bufio.NewScanner(reader)

	// Get buffer from pool
	buffer := GlobalBufferPool.Get()
	defer GlobalBufferPool.Put(buffer)
	scanner.Buffer(buffer, len(buffer))

	var lines []string
	const maxLines = 10000

	for scanner.Scan() && len(lines) < maxLines {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}

	if err := scanner.Err(); err != nil {
		// If failed and we used UTF-16, try UTF-8
		if strings.Contains(encoding, "UTF-16") {
			reader = bytes.NewReader(rawBytes)
			scanner = bufio.NewScanner(reader)
			scanner.Buffer(buffer, len(buffer))

			lines = lines[:0] // Clear previous results
			for scanner.Scan() && len(lines) < maxLines {
				line := strings.TrimSpace(scanner.Text())
				if line != "" && !strings.Contains(line, string(rune(0))) { // Skip lines with null chars
					lines = append(lines, line)
				}
			}
		}
	}

	return lines, nil
}

// Enhanced UTF-8 cleaning and validation function
func cleanAndValidateUTF8(s string) string {
	if s == "" {
		return s
	}

	// First, try to fix common encoding issues
	s = fixCommonEncodingIssues(s)

	// If still not valid UTF-8, convert invalid sequences
	if !utf8.ValidString(s) {
		// Convert invalid UTF-8 to valid UTF-8 with replacement characters
		s = strings.ToValidUTF8(s, "�")
	}

	// Additional cleaning for display purposes
	s = strings.Map(func(r rune) rune {
		// Keep printable characters and common whitespace
		if r == '\t' || r == '\n' || r == '\r' {
			return r
		}
		// Keep printable Unicode characters (including Thai, Chinese, etc.)
		if r >= 32 && r != 127 {
			return r
		}
		// Replace control characters with space
		return ' '
	}, s)

	return s
}

// Fix common encoding issues that might affect Thai and other Unicode text
func fixCommonEncodingIssues(s string) string {
	// Common encoding fixes for Thai text and other Unicode characters
	fixes := map[string]string{
		// Common Windows-1252 to UTF-8 issues - using hex codes to avoid compile errors
		"\xe2\x80\x99": "'",   // Right single quotation mark
		"\xe2\x80\x9c": "\"",  // Left double quotation mark
		"\xe2\x80\x9d": "\"",  // Right double quotation mark
		"\xe2\x80\xa6": "...", // Horizontal ellipsis
		"\xe2\x80\x93": "-",   // En dash
		"\xe2\x80\x94": "-",   // Em dash
		// Thai encoding fixes (common issues)
		"\xc3\xa0\xc2\xb8": "\xe0\xb8\x81", // Thai 'ก' character fix
		"\xc3\xa0\xc2\xb9": "\xe0\xb8\x82", // Thai 'ข' character fix
	}

	result := s
	for wrong, correct := range fixes {
		result = strings.ReplaceAll(result, wrong, correct)
	}

	return result
}

func readLockpickingFileFromOffset(filePath string, offset int64) ([]string, int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	// Get file size
	stat, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}

	// If file is smaller than offset, file was rotated
	if stat.Size() < offset {
		offset = 0
	}

	// Seek to offset
	if _, err := file.Seek(offset, 0); err != nil {
		return nil, 0, err
	}

	// Get buffer from pool
	buffer := GlobalBufferPool.Get()
	defer GlobalBufferPool.Put(buffer)

	// Use pooled slice
	lines := GetBatch()
	defer PutBatch(lines)

	const maxLines = 10000

	// Try UTF-8 first, then UTF-16LE if that fails
	encodings := []struct {
		name    string
		decoder *encoding.Decoder
	}{
		{"UTF-8", nil},
		{"UTF-16LE", unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()},
	}

	for _, enc := range encodings {
		// Reset position
		file.Seek(offset, 0)
		*lines = (*lines)[:0]

		var scanner *bufio.Scanner
		if enc.decoder == nil {
			scanner = bufio.NewScanner(file)
		} else {
			reader := transform.NewReader(file, enc.decoder)
			scanner = bufio.NewScanner(reader)
		}

		scanner.Buffer(buffer, len(buffer))

		success := true
		for scanner.Scan() && len(*lines) < maxLines {
			line := cleanAndValidateUTF8(strings.TrimSpace(scanner.Text()))
			if line != "" {
				*lines = append(*lines, line)
			}
		}

		if err := scanner.Err(); err != nil {
			success = false
			continue
		}

		if success {
			break
		}
	}

	// Get current position
	currentPos, _ := file.Seek(0, 1)

	// Copy to return slice
	result := make([]string, len(*lines))
	copy(result, *lines)

	return result, currentPos, nil
}

// Enhanced safe string handling for Unicode with better Thai support
func safeString(s string) string {
	if s == "" {
		return s
	}

	// Clean and validate UTF-8
	s = cleanAndValidateUTF8(s)

	// Trim excessive whitespace
	s = strings.TrimSpace(s)

	// Limit length for Discord display (Discord has character limits)
	const maxDisplayLength = 100
	if len([]rune(s)) > maxDisplayLength {
		runes := []rune(s)
		s = string(runes[:maxDisplayLength]) + "..."
	}

	return s
}

// Parse lockpicking log using improved regex
func parseLockpickingLog(line string) *LockpickingRecord {
	if line == "" || !strings.Contains(line, "[LogMinigame]") {
		return nil
	}

	// Ensure the line is valid UTF-8
	line = cleanAndValidateUTF8(line)

	// Improved regex pattern that handles various User owner formats
	pattern := `(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): \[LogMinigame\] \[([^]]+)\] User: ([^(]+) \(([^,]+), ([^)]+)\)\. Success: (Yes|No)\. Elapsed time: ([\d.]+)\. Failed attempts: (\d+)\. Target object: ([^(]+)\(ID: ([^)]*)\)\. Lock type: ([^.]+)\. User owner: ([^.]*)\. Location: X=([\d.-]+) Y=([\d.-]+) Z=([\d.-]+)`

	re := regexp.MustCompile(pattern)
	matches := re.FindStringSubmatch(line)

	if len(matches) != 16 {
		return nil
	}

	// Parse date
	date, err := time.Parse("2006.01.02-15.04.05", matches[1])
	if err != nil {
		return nil
	}

	// Parse numeric values
	elapsedTime, err := strconv.ParseFloat(matches[7], 64)
	if err != nil {
		return nil
	}

	failedAttempts, err := strconv.Atoi(matches[8])
	if err != nil {
		return nil
	}

	locationX, _ := strconv.ParseFloat(matches[13], 64)
	locationY, _ := strconv.ParseFloat(matches[14], 64)
	locationZ, _ := strconv.ParseFloat(matches[15], 64)

	// Clean up strings
	userName := strings.TrimSpace(matches[3])
	steamID := strings.TrimSpace(matches[5])
	targetObject := strings.TrimSpace(matches[9])
	targetID := strings.TrimSpace(matches[10])
	lockType := strings.TrimSpace(matches[11])
	userOwner := strings.TrimSpace(matches[12])

	// Validate lock type
	if !isValidLockType(lockType) {
		return nil
	}

	return &LockpickingRecord{
		SteamID:        safeString(steamID),
		UserName:       safeString(userName),
		LockType:       lockType,
		Success:        matches[6] == "Yes",
		ElapsedTime:    elapsedTime,
		FailedAttempts: failedAttempts,
		TargetObject:   safeString(targetObject),
		TargetID:       safeString(targetID),
		UserOwner:      safeString(userOwner),
		LocationX:      locationX,
		LocationY:      locationY,
		LocationZ:      locationZ,
		CreatedAt:      date,
	}
}

// Check if lock type is valid
func isValidLockType(lockType string) bool {
	validTypes := []string{"VeryEasy", "Basic", "Medium", "Advanced"}
	lockType = strings.TrimSpace(lockType)

	for _, valid := range validTypes {
		if lockType == valid {
			return true
		}
	}

	return false
}

// Save lockpicking record to database
func saveLockpickingRecord(record *LockpickingRecord) error {
	if record == nil {
		return fmt.Errorf("record is nil")
	}

	dbMux.Lock()
	defer dbMux.Unlock()

	// Ensure all string fields are safe UTF-8
	record.SteamID = safeString(record.SteamID)
	record.UserName = safeString(record.UserName)
	record.TargetObject = safeString(record.TargetObject)
	record.TargetID = safeString(record.TargetID)
	record.UserOwner = safeString(record.UserOwner)

	// Update aggregated stats
	successCount := 0
	if record.Success {
		successCount = 1
	}

	_, err := lockpickingDB.Exec(`
		INSERT INTO lockpicking_stats (
			steam_id, user_name, lock_type, 
			success_count, attempt_count, 
			total_time, total_failed_attempts,
			updated_at
		) VALUES (?, ?, ?, ?, 1, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(steam_id, lock_type) DO UPDATE SET
			user_name = excluded.user_name,
			success_count = success_count + excluded.success_count,
			attempt_count = attempt_count + 1,
			total_time = total_time + excluded.total_time,
			total_failed_attempts = total_failed_attempts + excluded.total_failed_attempts,
			updated_at = CURRENT_TIMESTAMP
	`, record.SteamID, record.UserName, record.LockType,
		successCount, record.ElapsedTime, record.FailedAttempts)

	if err != nil {
		return fmt.Errorf("failed to update lockpicking stats: %v", err)
	}

	// Update target objects
	if record.TargetObject != "" {
		_, err = lockpickingDB.Exec(`
			INSERT INTO lockpicking_target_objects (steam_id, target_object, count)
			VALUES (?, ?, 1)
			ON CONFLICT(steam_id, target_object) DO UPDATE SET
				count = count + 1
		`, record.SteamID, record.TargetObject)

		if err != nil {
			log.Printf("Failed to update target object count: %v", err)
		}
	}

	return nil
}

// Update lockpicking stats
func updateLockpickingStats(record *LockpickingRecord) error {
	successCount := 0
	if record.Success {
		successCount = 1
	}

	_, err := lockpickingDB.Exec(`
		INSERT INTO lockpicking_stats (
			steam_id, user_name, lock_type, success_count, attempt_count, 
			total_failed_count, total_time, updated_at
		) VALUES (?, ?, ?, ?, 1, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(steam_id, lock_type) DO UPDATE SET
			user_name = excluded.user_name,
			success_count = success_count + excluded.success_count,
			attempt_count = attempt_count + 1,
			total_failed_count = total_failed_count + excluded.total_failed_count,
			total_time = total_time + excluded.total_time,
			updated_at = CURRENT_TIMESTAMP
	`, record.SteamID, record.UserName, record.LockType, successCount,
		record.FailedAttempts, record.ElapsedTime)

	return err
}

// Update target object count
func updateTargetObjectCount(steamID, targetObject string) error {
	_, err := lockpickingDB.Exec(`
		INSERT INTO lockpicking_target_objects (steam_id, target_object, count)
		VALUES (?, ?, 1)
		ON CONFLICT(steam_id, target_object) DO UPDATE SET
			count = count + 1
	`, steamID, targetObject)

	return err
}

// Get lockpicking rankings by lock type with UTF-8 safe output
func getLockpickingRankings(lockType string, limit int) ([]map[string]interface{}, error) {
	dbMux.RLock()
	defer dbMux.RUnlock()

	query := `
		SELECT 
			steam_id,
			user_name,
			success_count,
			attempt_count,
			total_failed_attempts,
			total_time,
			-- Calculate success rate like Python: success / (success + total_failed_attempts)
			CASE 
				WHEN (success_count + total_failed_attempts) > 0 
				THEN ROUND((CAST(success_count AS REAL) / (success_count + total_failed_attempts) * 100), 2)
				ELSE 0 
			END as success_rate,
			CASE 
				WHEN attempt_count > 0 
				THEN ROUND(total_time / attempt_count, 2)
				ELSE 0 
			END as avg_time,
			CASE 
				WHEN attempt_count > 0 
				THEN ROUND(CAST(total_failed_attempts AS REAL) / attempt_count, 2)
				ELSE 0 
			END as avg_fails_per_attempt
		FROM lockpicking_stats
		WHERE lock_type = ? AND attempt_count > 0
		ORDER BY success_count DESC, success_rate DESC, attempt_count ASC
		LIMIT ?
	`

	rows, err := lockpickingDB.Query(query, lockType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var steamID, userName string
		var successCount, attemptCount, totalFailedCount int64
		var totalTime, successRate, avgTime, avgFailsPerAttempt float64

		err := rows.Scan(
			&steamID, &userName, &successCount, &attemptCount,
			&totalFailedCount, &totalTime, &successRate,
			&avgTime, &avgFailsPerAttempt,
		)
		if err != nil {
			return nil, err
		}

		// Ensure UTF-8 safety for display
		userName = safeString(userName)
		steamID = safeString(steamID)

		// Create map exactly like Python dict
		result := map[string]interface{}{
			"steam_id":              steamID,
			"user_name":             userName,
			"success_count":         successCount,
			"attempt_count":         attemptCount,
			"total_failed_attempts": totalFailedCount,
			"total_time":            totalTime,
			"success_rate":          successRate,
			"avg_time":              avgTime,
			"avg_fails_per_attempt": avgFailsPerAttempt,
		}
		results = append(results, result)
	}

	return results, nil
}

// Get player lockpicking stats with UTF-8 safety
func getPlayerLockpickingStats(steamID string) ([]map[string]interface{}, error) {
	dbMux.RLock()
	defer dbMux.RUnlock()

	query := `
		SELECT 
			steam_id, user_name, lock_type, success_count, attempt_count,
			total_failed_attempts, total_time,
			CASE 
				WHEN (success_count + total_failed_attempts) > 0 
				THEN ROUND((CAST(success_count AS REAL) / (success_count + total_failed_attempts) * 100), 2)
				ELSE 0 
			END as success_rate,
			CASE 
				WHEN attempt_count > 0 
				THEN ROUND(total_time / attempt_count, 2)
				ELSE 0 
			END as avg_time,
			CASE 
				WHEN attempt_count > 0 
				THEN ROUND(CAST(total_failed_attempts AS REAL) / attempt_count, 2)
				ELSE 0 
			END as avg_fails_per_attempt
		FROM lockpicking_stats
		WHERE steam_id = ?
		ORDER BY lock_type
	`

	rows, err := lockpickingDB.Query(query, steamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var steamID, userName, lockType string
		var successCount, attemptCount, totalFailedAttempts int64
		var totalTime, successRate, avgTime, avgFailsPerAttempt float64

		err := rows.Scan(
			&steamID, &userName, &lockType, &successCount, &attemptCount,
			&totalFailedAttempts, &totalTime, &successRate, &avgTime, &avgFailsPerAttempt,
		)
		if err != nil {
			return nil, err
		}

		// Ensure UTF-8 safety
		userName = safeString(userName)
		steamID = safeString(steamID)

		result := map[string]interface{}{
			"steam_id":              steamID,
			"user_name":             userName,
			"lock_type":             lockType,
			"success_count":         successCount,
			"attempt_count":         attemptCount,
			"total_failed_attempts": totalFailedAttempts,
			"total_time":            totalTime,
			"success_rate":          successRate,
			"avg_time":              avgTime,
			"avg_fails_per_attempt": avgFailsPerAttempt,
		}
		results = append(results, result)
	}

	return results, nil
}

// Enhanced ranking display with better UTF-8 handling
func updateLockpickingRankings() {
	rankingUpdateMux.Lock()
	defer rankingUpdateMux.Unlock()

	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ Error in lockpicking rankings update: %v", r)
		}
	}()

	// Check channel ID
	if Config.LockpickRankingChannelID == "" {
		log.Printf("⚠️ [LOCKPICK RANKING] No channel ID configured")
		return
	}

	// Check if Discord session is ready
	if SharedSession == nil {
		log.Printf("⚠️ [LOCKPICK RANKING] SharedSession is nil")
		return
	}

	// Check if database is ready
	if lockpickingDB == nil {
		log.Printf("⚠️ [LOCKPICK RANKING] Database is nil")
		return
	}

	log.Printf("🏆 [LOCKPICK RANKING] Updating rankings...")

	// Clear the ranking channel
	messages, err := SharedSession.ChannelMessages(Config.LockpickRankingChannelID, 100, "", "", "")
	if err == nil {
		for _, msg := range messages {
			if msg.Author.ID == SharedSession.State.User.ID {
				SharedSession.ChannelMessageDelete(Config.LockpickRankingChannelID, msg.ID)
				time.Sleep(100 * time.Millisecond)
			}
		}
	}

	// Check if there's any data
	dbMux.RLock()
	var count int
	err = lockpickingDB.QueryRow("SELECT COUNT(*) FROM lockpicking_stats WHERE attempt_count > 0").Scan(&count)
	dbMux.RUnlock()

	if err != nil {
		log.Printf("⚠️ [LOCKPICK RANKING] Database error: %v", err)
		return
	}

	if count == 0 {
		log.Printf("⚠️ [LOCKPICK RANKING] No data available (count=0)")
		SharedSession.ChannelMessageSend(Config.LockpickRankingChannelID, "```\nNo lockpicking data available yet\n```")
		lastRankingUpdate = time.Now()
		return
	}

	log.Printf("🏆 [LOCKPICK RANKING] Found %d records, sending embeds...", count)

	// Send rankings for each lock type
	lockTypes := []struct {
		Type  string
		Name  string
		Color int
	}{
		{"VeryEasy", "Bronze", 0xCD7F32},
		{"Basic", "Iron", 0x71797E},
		{"Medium", "Silver", 0xC0C0C0},
		{"Advanced", "Gold", 0xFFD700},
	}

	for _, lockTypeInfo := range lockTypes {
		rankings, err := getLockpickingRankings(lockTypeInfo.Type, 10)
		if err != nil || len(rankings) == 0 {
			continue
		}

		// Build ranking table
		tableLines := []string{
			"RANK   Success   Fails       %   Player",
			strings.Repeat("─", 64),
		}

		for i, stat := range rankings {
			rank := i + 1
			user := stat["user_name"].(string)

			// Smart truncation for Unicode characters
			const maxDisplayWidth = 22
			displayUser := user
			runes := []rune(user)
			if len(runes) > maxDisplayWidth {
				displayUser = string(runes[:maxDisplayWidth-3]) + "..."
			}

			successCount := stat["success_count"].(int64)
			totalFailedAttempts := stat["total_failed_attempts"].(int64)
			successRate := stat["success_rate"].(float64)

			rankStr := fmt.Sprintf("[%d]", rank)
			line := fmt.Sprintf("%-7s%7d%8d%8.2f%%   %s",
				rankStr, successCount, totalFailedAttempts, successRate, displayUser)
			tableLines = append(tableLines, line)
		}

		embed := &discordgo.MessageEmbed{
			Title:       fmt.Sprintf("BEST LOCK PICKERS - %s", lockTypeInfo.Name),
			Description: fmt.Sprintf("```\n%s\n```", strings.Join(tableLines, "\n")),
			Color:       lockTypeInfo.Color,
			Timestamp:   time.Now().Format(time.RFC3339),
			Footer: &discordgo.MessageEmbedFooter{
				Text: "Last Updated",
			},
		}

		_, sendErr := SharedSession.ChannelMessageSendEmbed(Config.LockpickRankingChannelID, embed)
		if sendErr != nil {
			log.Printf("❌ [LOCKPICK RANKING] Failed to send embed for %s: %v", lockTypeInfo.Name, sendErr)
		}
		time.Sleep(500 * time.Millisecond)
	}

	lastRankingUpdate = time.Now()
	log.Printf("✅ [LOCKPICK RANKING] Rankings updated successfully")
	UpdateSharedActivity()
}

// Process lockpicking lines - Streamlined production version
func processLockpickingLines(lines []string) {
	// Check memory before processing
	if !GlobalResourceManager.CheckMemory() {
		log.Printf("Skipping lockpicking processing due to high memory usage")
		return
	}

	processedCount := 0
	skippedCount := 0

	for _, line := range lines {
		normalizedLine := strings.TrimSpace(line)

		// Skip empty lines and non-lockpicking lines
		if normalizedLine == "" || !strings.Contains(normalizedLine, "[LogMinigame]") {
			continue
		}

		logKey := normalizedLine + "_lockpicking"

		if !IsLogSentCached(logKey) {
			record := parseLockpickingLog(normalizedLine)
			if record != nil {
				err := saveLockpickingRecord(record)
				if err != nil {
					log.Printf("Error saving lockpicking record: %v", err)
				} else {
					MarkLogAsSentCached(logKey)
					processedCount++
				}
			}
		} else {
			skippedCount++
		}
	}

	if processedCount > 0 {
		log.Printf("Processed %d new lockpicking records (%d skipped)", processedCount, skippedCount)
	}
}

// Local Log Processing for lockpicking
func processLockpickingLocalLogs() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [LOCKPICKING LOCAL] Panic recovered: %v", r)
			UpdateSharedActivity()
		}
	}()

	latestFile := getLockpickingLatestLocalLogFile()
	if latestFile == "" {
		return
	}

	// Check file size
	fileInfo, err := os.Stat(latestFile)
	if err != nil {
		log.Printf("Error getting file info: %v", err)
		return
	}

	// If file watcher is enabled, use offset-based reading
	if Config.EnableFileWatcher {
		lockpickingOffsetMux.RLock()
		offset := lockpickingFileOffsets[latestFile]
		lockpickingOffsetMux.RUnlock()

		// For large files, use streaming
		if fileInfo.Size()-offset > 10*1024*1024 { // If remaining > 10MB
			log.Printf("📊 Large update detected in lockpicking log, using streaming")

			// Create a temporary file starting from offset
			tempFile := latestFile + ".partial"
			err := extractPartialFile(latestFile, tempFile, offset)
			if err != nil {
				log.Printf("Error extracting partial file: %v", err)
				return
			}
			defer os.Remove(tempFile)

			err = lockpickingStreamProcessor.ProcessLargeFile(tempFile, processLockpickingLines)
			if err != nil {
				log.Printf("Error processing lockpicking log file: %v", err)
				return
			}

			// Update offset
			lockpickingOffsetMux.Lock()
			lockpickingFileOffsets[latestFile] = fileInfo.Size()
			lockpickingOffsetMux.Unlock()
			saveLockpickingFileOffsets()
		} else {
			// For smaller updates, use regular method
			lines, newOffset, err := readLockpickingFileFromOffset(latestFile, offset)
			if err != nil {
				log.Printf("Error reading lockpicking file %s: %v", latestFile, err)
				return
			}

			if len(lines) > 0 {
				lockpickingOffsetMux.Lock()
				lockpickingFileOffsets[latestFile] = newOffset
				lockpickingOffsetMux.Unlock()

				processLockpickingLines(lines)
				saveLockpickingFileOffsets()
			}
		}
	} else {
		// For large files without file watcher, use streaming
		if fileInfo.Size() > 10*1024*1024 { // If file > 10MB
			log.Printf("📊 Processing large lockpicking log file (%.2f MB) with streaming",
				float64(fileInfo.Size())/1024/1024)
			err := lockpickingStreamProcessor.ProcessLargeFile(latestFile, processLockpickingLines)
			if err != nil {
				log.Printf("Error processing lockpicking log file: %v", err)
				return
			}
		} else {
			// Read entire file for smaller files
			lines, err := readLockpickingFileContent(latestFile)
			if err != nil {
				log.Printf("Error reading lockpicking log file: %v", err)
				return
			}
			processLockpickingLines(lines)
		}
	}
}

// File Watcher for lockpicking
func startLockpickingFileWatcher(ctx context.Context) {
	if !Config.UseLocalFiles || !Config.EnableFileWatcher {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [LOCKPICKING WATCHER] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go startLockpickingFileWatcher(ctx)
		}
	}()

	var err error
	lockpickingFileWatcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Error creating lockpicking file watcher: %v", err)
		return
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(Config.LocalLogsPath, 0755); err != nil {
		log.Printf("Warning: Could not create lockpicking logs directory: %v", err)
	}

	// Watch the logs directory
	err = lockpickingFileWatcher.Add(Config.LocalLogsPath)
	if err != nil {
		log.Printf("Error watching lockpicking logs directory: %v", err)
		return
	}

	go func() {
		defer lockpickingFileWatcher.Close()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("❌ [LOCKPICKING WATCHER LOOP] Panic recovered: %v", r)
			}
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-lockpickingFileWatcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					filename := filepath.Base(event.Name)
					if strings.Contains(filename, "gameplay") {
						UpdateSharedActivity()
						processLockpickingLocalLogs()
					}
				}
			case err, ok := <-lockpickingFileWatcher.Errors:
				if !ok {
					return
				}
				log.Printf("Lockpicking file watcher error: %v", err)
			}
		}
	}()

	log.Printf("✅ Lockpicking file watcher started for directory: %s", Config.LocalLogsPath)
}

// Periodic Log Checking - PARALLEL processing for speed
func lockpickingPeriodicLogCheck(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [LOCKPICKING CHECK] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go lockpickingPeriodicLogCheck(ctx)
		}
	}()

	ticker := time.NewTicker(time.Duration(Config.CheckInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			UpdateSharedActivity()

			// Check memory before processing
			if !GlobalResourceManager.CheckMemory() {
				log.Printf("⚠️ Skipping lockpicking check due to high memory usage")
				continue
			}

			// Process remote and local in PARALLEL
			var wg sync.WaitGroup
			if Config.UseRemote {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() {
						if r := recover(); r != nil {
							log.Printf("❌ [LOCKPICKING REMOTE] Panic during processing: %v", r)
						}
					}()
					processLockpickingRemoteLogs()
				}()
			}
			if Config.UseLocalFiles {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() {
						if r := recover(); r != nil {
							log.Printf("❌ [LOCKPICKING LOCAL] Panic during processing: %v", r)
						}
					}()
					processLockpickingLocalLogs()
				}()
			}
			wg.Wait()

			UpdateSharedActivity()
		}
	}
}

// Bot commands for lockpicking with UTF-8 safety
func lockpickingMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [LOCKPICKING MESSAGE] Panic recovered: %v", r)
		}
	}()

	// Check for commands
	if strings.HasPrefix(m.Content, "!lockpickstats") {
		showLockpickingStatsCommand(s, m)
	} else if strings.HasPrefix(m.Content, "!resetrankinglockpick") {
		resetLockpickingRankingCommand(s, m)
	} else if strings.HasPrefix(m.Content, "!lockpickdb") {
		showLockpickingDBStats(s, m)
	} else if strings.HasPrefix(m.Content, "!forcelockpick") {
		forceUpdateRankingCommand(s, m)
	} else if strings.HasPrefix(m.Content, "!debuglockpick") {
		debugLockpickingCommand(s, m)
	} else if strings.HasPrefix(m.Content, "!testlockpick") {
		testLockpickingCommand(s, m)
	}
}

// Test lockpicking parsing command
func testLockpickingCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !hasManagePermissions(s, m) {
		s.ChannelMessageSend(m.ChannelID, "```diff\n- คุณไม่มีสิทธิ์ในการใช้คำสั่งนี้\n```")
		return
	}

	// Test with sample lines from the log
	testLines := []string{
		"2025.09.21-03.11.36: [LogMinigame] [LockpickingMinigame_C] User: 888_SaLMoN (135, 76561198316261520). Success: Yes. Elapsed time: 5.46. Failed attempts: 0. Target object: BPLockpick_Weapon_Locker_Police_C(ID: N/A). Lock type: VeryEasy. User owner: N/A. Location: X=493688.500 Y=-192791.516 Z=238.118",
		"2025.09.21-03.07.16: [LogMinigame] [BP_LockBombDefusalMinigame_C] User: 888_PCK (137, 76561198318569947). Success: Yes. Elapsed time: 5.27. Failed attempts: 0. Target object: BP_KillboxSmallDoors_01_C(ID: N/A). Lock type: Basic. User owner: 0(World). Location: X=-399072.719 Y=232053.562 Z=58959.090",
		"2025.09.21-03.09.00: [LogMinigame] [LockpickingMinigame_C] User: 888_PCK (137, 76561198318569947). Success: Yes. Elapsed time: 10.28. Failed attempts: 2. Target object: BP_KillBoxRoomMeshDoor_C(ID: N/A). Lock type: Advanced. User owner: 0(). Location: X=-400148.406 Y=233311.516 Z=58959.090",
	}

	results := []string{"🧪 **Lockpicking Parse Test Results:**", ""}
	successCount := 0

	for i, testLine := range testLines {
		record := parseLockpickingLog(testLine)
		if record != nil {
			results = append(results, fmt.Sprintf("✅ **Test %d:** Success", i+1))
			results = append(results, fmt.Sprintf("   User: %s", record.UserName))
			results = append(results, fmt.Sprintf("   Lock Type: %s", record.LockType))
			results = append(results, fmt.Sprintf("   Success: %v", record.Success))
			results = append(results, "")
			successCount++
		} else {
			results = append(results, fmt.Sprintf("❌ **Test %d:** Failed to parse", i+1))
			results = append(results, "")
		}
	}

	results = append(results, fmt.Sprintf("**Summary:** %d/%d tests passed", successCount, len(testLines)))

	embed := &discordgo.MessageEmbed{
		Title:       "🧪 Lockpicking Parse Test",
		Description: strings.Join(results, "\n"),
		Color:       0x00FF00,
		Timestamp:   time.Now().Format(time.RFC3339),
	}

	s.ChannelMessageSendEmbed(m.ChannelID, embed)
}

// Show lockpicking statistics command with UTF-8 safe display
func showLockpickingStatsCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	parts := strings.Fields(m.Content)
	if len(parts) < 2 {
		s.ChannelMessageSend(m.ChannelID, "กรุณาระบุ Steam ID: `!lockpickstats <SteamID>`")
		return
	}

	steamID := parts[1]
	stats, err := getPlayerLockpickingStats(steamID)
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, "เกิดข้อผิดพลาดในการดึงข้อมูล")
		log.Printf("Error getting lockpicking stats: %v", err)
		return
	}

	if len(stats) == 0 {
		s.ChannelMessageSend(m.ChannelID, "ไม่พบข้อมูลสำหรับ Steam ID นี้")
		return
	}

	userName := safeString(stats[0]["user_name"].(string))
	embed := &discordgo.MessageEmbed{
		Title: fmt.Sprintf("📊 สถิติการไขล็อคของ %s", userName),
		Color: 0x3498db,
	}

	totalSuccess := int64(0)
	totalAttempts := int64(0)

	for _, stat := range stats {
		lockType := stat["lock_type"].(string)
		success := stat["success_count"].(int64)
		attempts := stat["attempt_count"].(int64)
		totalTime := stat["total_time"].(float64)
		failedAttempts := stat["total_failed_attempts"].(int64)

		successRate := float64(0)
		avgTime := float64(0)
		avgFails := float64(0)

		if attempts > 0 {
			successRate = float64(success) / float64(attempts) * 100
			avgTime = totalTime / float64(attempts)
			avgFails = float64(failedAttempts) / float64(attempts)
		}

		totalSuccess += success
		totalAttempts += attempts

		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name: fmt.Sprintf("**%s**", lockType),
			Value: fmt.Sprintf("Success: %d/%d (%.1f%%)\nAvg Time: %.2fs\nAvg Fails: %.2f",
				success, attempts, successRate, avgTime, avgFails),
			Inline: true,
		})
	}

	if totalAttempts > 0 {
		totalRate := float64(totalSuccess) / float64(totalAttempts) * 100
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "**Total**",
			Value:  fmt.Sprintf("Overall: %d/%d (%.1f%%)", totalSuccess, totalAttempts, totalRate),
			Inline: false,
		})
	}

	s.ChannelMessageSendEmbed(m.ChannelID, embed)
}

// Reset lockpicking ranking command
func resetLockpickingRankingCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Check permissions (you might want to implement a proper permission system)
	if !hasManagePermissions(s, m) {
		s.ChannelMessageSend(m.ChannelID, "```diff\n- คุณไม่มีสิทธิ์ในการใช้คำสั่งนี้\n```")
		return
	}

	dbMux.Lock()
	defer dbMux.Unlock()

	// Create backup table
	backupTable := fmt.Sprintf("lockpicking_stats_backup_%s", time.Now().Format("20060102_150405"))
	_, err := lockpickingDB.Exec(fmt.Sprintf("CREATE TABLE %s AS SELECT * FROM lockpicking_stats", backupTable))
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, "```diff\n- เกิดข้อผิดพลาดในการสำรองข้อมูล\n```")
		log.Printf("Error creating backup: %v", err)
		return
	}

	// Clear data - Use DELETE instead of TRUNCATE for SQLite
	_, err = lockpickingDB.Exec("DELETE FROM lockpicking_stats")
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, "```diff\n- เกิดข้อผิดพลาดในการรีเซ็ตข้อมูล\n```")
		log.Printf("Error resetting stats: %v", err)
		return
	}

	_, err = lockpickingDB.Exec("DELETE FROM lockpicking_target_objects")
	if err != nil {
		log.Printf("Error resetting target objects: %v", err)
	}

	s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("```diff\n+ รีเซ็ตอันดับการไขล็อคเรียบร้อย\n+ สำรองข้อมูลเก่าไว้ในตาราง %s\n```", backupTable))
	log.Printf("Lockpicking rankings reset by %s", m.Author.Username)
	UpdateSharedActivity()
}

// Show lockpicking database statistics
func showLockpickingDBStats(s *discordgo.Session, m *discordgo.MessageCreate) {
	dbMux.RLock()
	defer dbMux.RUnlock()

	var totalRecords, totalPlayers int
	err := lockpickingDB.QueryRow("SELECT COUNT(*) FROM lockpicking_stats").Scan(&totalRecords)
	if err != nil {
		totalRecords = 0
	}

	err = lockpickingDB.QueryRow("SELECT COUNT(DISTINCT steam_id) FROM lockpicking_stats").Scan(&totalPlayers)
	if err != nil {
		totalPlayers = 0
	}

	// Get lock type distribution
	rows, err := lockpickingDB.Query("SELECT lock_type, COUNT(*) FROM lockpicking_stats GROUP BY lock_type")
	lockTypeStats := make(map[string]int)
	if err == nil {
		for rows.Next() {
			var lockType string
			var count int
			rows.Scan(&lockType, &count)
			lockTypeStats[lockType] = count
		}
		rows.Close()
	}

	// Get next reset time
	weeklyResetMux.RLock()
	nextReset := getNextMondayMidnight()
	timeUntilReset := time.Until(nextReset)
	weeklyResetMux.RUnlock()

	embed := &discordgo.MessageEmbed{
		Title: "🗄️ Lockpicking Database Statistics",
		Color: 0x9B59B6,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "Database Info",
				Value:  fmt.Sprintf("Total Records: %d\nActive Players: %d", totalRecords, totalPlayers),
				Inline: true,
			},
			{
				Name:   "Status",
				Value:  "✅ Online (UTF-8 Enhanced)",
				Inline: true,
			},
			{
				Name:   "Auto Reset",
				Value:  fmt.Sprintf("Next Reset: %s\n(%s)", nextReset.Format("Mon 02 Jan 15:04"), formatDuration(timeUntilReset)),
				Inline: false,
			},
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	if len(lockTypeStats) > 0 {
		statsValue := ""
		for lockType, count := range lockTypeStats {
			statsValue += fmt.Sprintf("%s: %d players\n", lockType, count)
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "Lock Type Distribution",
			Value:  statsValue,
			Inline: false,
		})
	}

	s.ChannelMessageSendEmbed(m.ChannelID, embed)
}

// Helper function to get next Monday midnight
func getNextMondayMidnight() time.Time {
	now := time.Now()

	// Calculate days until next Monday
	daysUntilMonday := (8 - int(now.Weekday())) % 7
	if daysUntilMonday == 0 {
		daysUntilMonday = 7
	}

	nextMonday := now.AddDate(0, 0, daysUntilMonday)
	return time.Date(nextMonday.Year(), nextMonday.Month(), nextMonday.Day(), 0, 0, 0, 0, now.Location())
}

// Helper function to format duration
func formatDuration(d time.Duration) string {
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	if days > 0 {
		return fmt.Sprintf("%d days %d hours", days, hours)
	} else if hours > 0 {
		return fmt.Sprintf("%d hours %d minutes", hours, minutes)
	} else {
		return fmt.Sprintf("%d minutes", minutes)
	}
}

// Helper function to check permissions (implement as needed)
func hasManagePermissions(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	// Get member permissions
	guild, err := s.Guild(m.GuildID)
	if err != nil {
		return false
	}

	member, err := s.GuildMember(guild.ID, m.Author.ID)
	if err != nil {
		return false
	}

	// Check if user has MANAGE_MESSAGES permission or is admin
	perms, err := s.UserChannelPermissions(member.User.ID, m.ChannelID)
	if err != nil {
		return false
	}

	return (perms&discordgo.PermissionManageMessages) != 0 || (perms&discordgo.PermissionAdministrator) != 0
}

// Force update ranking command for testing
func forceUpdateRankingCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !hasManagePermissions(s, m) {
		s.ChannelMessageSend(m.ChannelID, "```diff\n- คุณไม่มีสิทธิ์ในการใช้คำสั่งนี้\n```")
		return
	}

	// Check if there's data in database
	dbMux.RLock()
	var count int
	err := lockpickingDB.QueryRow("SELECT COUNT(*) FROM lockpicking_stats WHERE attempt_count > 0").Scan(&count)
	dbMux.RUnlock()

	if err != nil {
		s.ChannelMessageSend(m.ChannelID, "```diff\n- เกิดข้อผิดพลาดในการตรวจสอบฐานข้อมูล\n```")
		log.Printf("Error checking database: %v", err)
		return
	}

	if count == 0 {
		s.ChannelMessageSend(m.ChannelID, "```diff\n- ยังไม่มีข้อมูลการไขล็อคในฐานข้อมูล\n```")
		return
	}

	// Reset the last update time to force an update
	rankingUpdateMux.Lock()
	lastRankingUpdate = time.Time{} // Reset to zero time
	rankingUpdateMux.Unlock()

	// Send confirmation message
	msg, err := s.ChannelMessageSend(m.ChannelID, "```diff\n+ กำลังอัปเดตอันดับการไขล็อค...\n```")
	if err != nil {
		log.Printf("Error sending confirmation message: %v", err)
	}

	// Force update rankings in goroutine with callback
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("❌ [FORCE RANKING] Panic recovered: %v", r)
				if msg != nil {
					s.ChannelMessageEdit(m.ChannelID, msg.ID, "```diff\n- เกิดข้อผิดพลาดในการอัปเดตอันดับ\n```")
				}
			}
		}()

		// Call updateLockpickingRankings directly
		updateLockpickingRankings()

		// Update message when completed
		if msg != nil {
			s.ChannelMessageEdit(m.ChannelID, msg.ID, "```diff\n+ อัปเดตอันดับการไขล็อคเรียบร้อยแล้ว! (UTF-8 Enhanced)\n```")
		}

		log.Printf("Force ranking update completed by %s", m.Author.Username)
	}()

	log.Printf("Force ranking update triggered by %s", m.Author.Username)
}

// Debug command for lockpicking with UTF-8 information
func debugLockpickingCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	if !hasManagePermissions(s, m) {
		s.ChannelMessageSend(m.ChannelID, "```diff\n- คุณไม่มีสิทธิ์ในการใช้คำสั่งนี้\n```")
		return
	}

	dbMux.RLock()
	defer dbMux.RUnlock()

	// Check database connection
	err := lockpickingDB.Ping()
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("```diff\n- Database connection error: %v\n```", err))
		return
	}

	// Get sample data from database with Thai name display
	rows, err := lockpickingDB.Query(`
		SELECT user_name, lock_type, success_count, attempt_count,
		       LENGTH(user_name) as name_length,
		       LENGTH(user_name) - LENGTH(REPLACE(user_name, ' ', '')) as spaces
		FROM lockpicking_stats 
		ORDER BY success_count DESC 
		LIMIT 5
	`)
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("```diff\n- Query error: %v\n```", err))
		return
	}
	defer rows.Close()

	var debugInfo []string
	debugInfo = append(debugInfo, "🔍 **Lockpicking Debug Information (UTF-8 Enhanced)**")
	debugInfo = append(debugInfo, "")
	debugInfo = append(debugInfo, "**Database Status:** ✅ Connected with UTF-8 support")

	// Get total counts
	var totalRecords, totalPlayers int
	lockpickingDB.QueryRow("SELECT COUNT(*) FROM lockpicking_stats").Scan(&totalRecords)
	lockpickingDB.QueryRow("SELECT COUNT(DISTINCT steam_id) FROM lockpicking_stats").Scan(&totalPlayers)

	debugInfo = append(debugInfo, fmt.Sprintf("**Total Records:** %d", totalRecords))
	debugInfo = append(debugInfo, fmt.Sprintf("**Total Players:** %d", totalPlayers))
	debugInfo = append(debugInfo, "")

	// Get sample data with UTF-8 analysis
	debugInfo = append(debugInfo, "**Top 5 Players (UTF-8 Analysis):**")
	for rows.Next() {
		var userName, lockType string
		var successCount, attemptCount, nameLength, spaces int64
		rows.Scan(&userName, &lockType, &successCount, &attemptCount, &nameLength, &spaces)

		// UTF-8 validation check
		isValidUTF8 := utf8.ValidString(userName)
		runeCount := utf8.RuneCountInString(userName)

		debugInfo = append(debugInfo, fmt.Sprintf("- **%s** (%s): %d/%d",
			userName, lockType, successCount, attemptCount))
		debugInfo = append(debugInfo, fmt.Sprintf("  └ UTF-8: %v | Bytes: %d | Runes: %d | Spaces: %d",
			isValidUTF8, nameLength, runeCount, spaces))
	}

	var encoding string
	lockpickingDB.QueryRow("PRAGMA encoding").Scan(&encoding)

	weeklyResetMux.RLock()
	lastReset := lastWeeklyReset
	nextReset := getNextMondayMidnight()
	weeklyResetMux.RUnlock()

	debugInfo = append(debugInfo, "")
	debugInfo = append(debugInfo, fmt.Sprintf("**Database Encoding:** %s", encoding))
	debugInfo = append(debugInfo, fmt.Sprintf("**Last Ranking Update:** %s", lastRankingUpdate.Format("2006-01-02 15:04:05")))
	debugInfo = append(debugInfo, fmt.Sprintf("**Last Weekly Reset:** %s", lastReset.Format("2006-01-02 15:04:05")))
	debugInfo = append(debugInfo, fmt.Sprintf("**Next Weekly Reset:** %s", nextReset.Format("2006-01-02 15:04:05")))
	debugInfo = append(debugInfo, fmt.Sprintf("**Channel ID:** %s", Config.LockpickRankingChannelID))

	testThai := "ทดสอบภาษาไทย"
	debugInfo = append(debugInfo, fmt.Sprintf("**Thai Test String:** %s", testThai))
	debugInfo = append(debugInfo, fmt.Sprintf("**Thai UTF-8 Valid:** %v", utf8.ValidString(testThai)))

	embed := &discordgo.MessageEmbed{
		Title:       "🔍 Lockpicking Debug Information (UTF-8 Enhanced)",
		Description: strings.Join(debugInfo, "\n"),
		Color:       0x00FF00,
		Timestamp:   time.Now().Format(time.RFC3339),
		Footer: &discordgo.MessageEmbedFooter{
			Text: "UTF-8 Support: Enabled | Encoding: Enhanced | Auto-Reset: Active",
		},
	}

	s.ChannelMessageSendEmbed(m.ChannelID, embed)
}

// extractPartialFile function is implemented in utils.go

func CloseLockpickingDatabase() {
	if lockpickingDB != nil {
		lockpickingDB.Close()
		log.Println("✅ Lockpicking database closed")
	}
}
