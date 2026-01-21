package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// Admin module specific variables
var (
	adminFileWatcher *fsnotify.Watcher
	adminFileOffsets = make(map[string]int64)
	adminOffsetMux   sync.RWMutex

	// Stream processor for large files
	adminStreamProcessor *StreamingFileProcessor
)

// AdminInfo represents parsed admin command
type AdminInfo struct {
	Date     string `json:"date"`
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	Command  string `json:"command"`
}

// Initialize admin module
func InitializeAdmin() error {
	// Initialize stream processor
	adminStreamProcessor = NewStreamingFileProcessor(
		32*1024, // 32KB buffer
		100,     // Max 100 items per batch
		5,       // Max 5 concurrent batches
	)

	// Load file offsets if using local files with file watcher
	if Config.UseLocalFiles && Config.EnableFileWatcher {
		if err := loadAdminFileOffsets(); err != nil {
			log.Printf("Warning: Could not load admin file offsets: %v", err)
		}
	}

	log.Println("✅ Admin module initialized with performance enhancements")
	return nil
}

// Start admin module
func StartAdmin(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [ADMIN START] Panic recovered: %v\n%s", r, debug.Stack())
			UpdateSharedActivity()
		}
	}()

	// Start file watcher if enabled
	if Config.UseLocalFiles && Config.EnableFileWatcher {
		startAdminFileWatcher(ctx)
	}

	// Start periodic admin checking
	go adminPeriodicLogCheck(ctx)

	// Start progress monitor
	go monitorAdminProgress(ctx)

	log.Printf("✅ Admin module started with enhanced performance!")
}

// Monitor progress from stream processor
func monitorAdminProgress(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case progress := <-adminStreamProcessor.progressChan:
			log.Printf("📊 Admin processing: %.1f%% complete", progress)
		}
	}
}

func loadAdminFileOffsets() error {
	adminOffsetMux.Lock()
	defer adminOffsetMux.Unlock()

	file, err := os.Open("admin_file_offsets.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	return decoder.Decode(&adminFileOffsets)
}

func saveAdminFileOffsets() error {
	adminOffsetMux.RLock()
	defer adminOffsetMux.RUnlock()

	file, err := os.Create("admin_file_offsets.json")
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	return encoder.Encode(adminFileOffsets)
}

// Remote Log Processing for admin - Using unified connection system
func processAdminRemoteLogs() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [ADMIN REMOTE] Panic recovered: %v", r)
			UpdateSharedActivity()
		}
	}()

	// Find latest admin file using unified remote system
	latestFile, remotePath, err := GetLatestRemoteFile("admin")
	if err != nil {
		log.Printf("Error finding admin log files: %v", err)
		return
	}

	localDir := filepath.Join("logs", "admin")
	os.MkdirAll(localDir, 0755)
	localPath := filepath.Join(localDir, latestFile.Name)

	// Get REAL-TIME file size using FTP SIZE command
	realFileInfo, err := GetRemoteFileStat(latestFile.Name)
	if err != nil {
		realFileInfo = latestFile
	}

	// HYBRID check: download if Size increased OR ModTime changed
	needsDownload := true
	localInfo, err := os.Stat(localPath)
	if err == nil {
		localModTime := localInfo.ModTime().Unix()
		sizeChanged := localInfo.Size() < realFileInfo.Size
		modTimeChanged := realFileInfo.ModTime > localModTime

		if !sizeChanged && !modTimeChanged {
			needsDownload = false
		} else if sizeChanged {
			log.Printf("📥 Admin: Size changed (local: %d, remote: %d bytes)",
				localInfo.Size(), realFileInfo.Size)
		} else if modTimeChanged {
			log.Printf("📥 Admin: ModTime changed (local: %d, remote: %d)",
				localModTime, realFileInfo.ModTime)
		}
	}

	// Download if needed
	if needsDownload {
		err = DownloadFileRemote(remotePath, localPath, 3)
		if err != nil {
			log.Printf("Error downloading admin log: %v", err)
			return
		}
	}

	// ALWAYS process the file (cache system will prevent duplicate messages)
	fileInfo, err := os.Stat(localPath)
	if err != nil {
		log.Printf("Error accessing admin log file: %v", err)
		return
	}

	if fileInfo.Size() > 10*1024*1024 { // If file > 10MB, use streaming
		log.Printf("📊 Processing large admin log file (%.2f MB) with streaming",
			float64(fileInfo.Size())/1024/1024)
		err = adminStreamProcessor.ProcessLargeFile(localPath, processAdminLines)
		if err != nil {
			log.Printf("Error processing admin log file: %v", err)
			return
		}
	} else {
		// For smaller files, use traditional method
		lines, err := readAdminFileContent(localPath)
		if err != nil {
			log.Printf("Error reading admin log file: %v", err)
			return
		}
		processAdminLines(lines)
	}
}

// Local File Functions for admin
func findAdminLocalLogFiles() []string {
	pattern := filepath.Join(Config.LocalLogsPath, "*admin*")
	files, err := filepath.Glob(pattern)
	if err != nil {
		log.Printf("Error finding admin log files: %v", err)
		return nil
	}
	return files
}

func getAdminLatestLocalLogFile() string {
	files := findAdminLocalLogFiles()
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

// Enhanced file reading with memory efficiency
func readAdminFileContent(filePath string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Check file size
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}

	// Limit file size
	const maxFileSize = 100 * 1024 * 1024
	if stat.Size() > maxFileSize {
		log.Printf("⚠️ File %s is too large (%.2f MB), using streaming instead",
			filePath, float64(stat.Size())/1024/1024)
		return nil, fmt.Errorf("file too large for direct reading")
	}

	// Get buffer from pool
	buffer := GlobalBufferPool.Get()
	defer GlobalBufferPool.Put(buffer)

	// Try UTF-16 LE first
	decoder := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
	reader := transform.NewReader(file, decoder)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(buffer, len(buffer))

	// Use pooled slice for lines
	lines := GetBatch()
	defer PutBatch(lines)

	const maxLines = 10000
	for scanner.Scan() && len(*lines) < maxLines {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			*lines = append(*lines, line)
		}
	}

	// If UTF-16 failed, try UTF-8
	if err := scanner.Err(); err != nil {
		file.Seek(0, 0)
		scanner = bufio.NewScanner(file)
		scanner.Buffer(buffer, len(buffer))
		*lines = (*lines)[:0] // Clear lines

		for scanner.Scan() && len(*lines) < maxLines {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				*lines = append(*lines, line)
			}
		}

		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}

	// Copy to return slice
	result := make([]string, len(*lines))
	copy(result, *lines)

	return result, nil
}

func readAdminFileFromOffset(filePath string, offset int64) ([]string, int64, error) {
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

	// Try UTF-16 LE first
	decoder := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
	reader := transform.NewReader(file, decoder)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(buffer, len(buffer))

	// Use pooled slice
	lines := GetBatch()
	defer PutBatch(lines)

	const maxLines = 10000
	for scanner.Scan() && len(*lines) < maxLines {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			*lines = append(*lines, line)
		}
	}

	// Get current position
	currentPos, _ := file.Seek(0, 1)

	// Copy to return slice
	result := make([]string, len(*lines))
	copy(result, *lines)

	return result, currentPos, nil
}

// Parse admin log using regex
func parseAdminLog(line string) *AdminInfo {
	if line == "" {
		return nil
	}

	// Pattern: 2024.01.01-12.00.00: '12345678901234567:PlayerName' Command: 'command text here'
	pattern := `(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): '(\d+):([^']+)' Command: '([^']+)'`
	re := regexp.MustCompile(pattern)
	matches := re.FindStringSubmatch(line)

	if len(matches) != 5 {
		return nil
	}

	// Clean up username (remove trailing numbers in parentheses)
	userName := matches[3]
	userName = regexp.MustCompile(`\(\d+\)$`).ReplaceAllString(userName, "")
	userName = strings.TrimSpace(userName)

	// Limit command to first 3 words for public display
	command := matches[4]
	commandParts := strings.Fields(command)
	if len(commandParts) > 3 {
		command = strings.Join(commandParts[:3], " ")
	}

	return &AdminInfo{
		Date:     matches[1],
		UserID:   matches[2],
		UserName: userName,
		Command:  command,
	}
}

// Send admin log to Discord (detailed version for admin channel)
func sendAdminLog(adminInfo *AdminInfo) error {
	if adminInfo == nil || Config.AdminChannelID == "" {
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [SEND ADMIN LOG] Panic recovered: %v", r)
		}
	}()

	// Detailed message with SteamID for admin channel
	message := fmt.Sprintf("```ansi\n"+
		"Date : %s\n"+
		"SteamID : \u001b[0;34m%s\u001b[0m   Name : \u001b[0;35m%s\u001b[0m\n"+
		"Command : \u001b[0;32m%s\u001b[0m\n"+
		"```",
		adminInfo.Date,
		adminInfo.UserID,
		adminInfo.UserName,
		adminInfo.Command,
	)

	_, err := SharedSession.ChannelMessageSend(Config.AdminChannelID, message)
	if err != nil {
		return fmt.Errorf("error sending admin log: %v", err)
	}

	UpdateSharedActivity()
	time.Sleep(25 * time.Millisecond) // Reduced from 100ms for faster processing

	return nil
}

// Send public log to Discord (anonymous version for public channel)
func sendPublicAdminLog(adminInfo *AdminInfo) error {
	if adminInfo == nil || Config.AdminPublicChannelID == "" {
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [SEND PUBLIC ADMIN LOG] Panic recovered: %v", r)
		}
	}()

	// Remove coordinates from command (X=123 Y=456 Z=789 patterns)
	command := adminInfo.Command
	coordPattern := regexp.MustCompile(`[XYZ]=-?\d+(\.\d+)?`)
	command = coordPattern.ReplaceAllString(command, "")
	command = strings.TrimSpace(strings.Join(strings.Fields(command), " ")) // Clean up extra spaces

	// Public message without SteamID
	message := fmt.Sprintf("```ansi\n"+
		"Date : %s\n"+
		"Name : \u001b[0;35m%s\u001b[0m  Command : \u001b[0;32m%s\u001b[0m\n"+
		"```",
		adminInfo.Date,
		adminInfo.UserName,
		command,
	)

	_, err := SharedSession.ChannelMessageSend(Config.AdminPublicChannelID, message)
	if err != nil {
		return fmt.Errorf("error sending public admin log: %v", err)
	}

	UpdateSharedActivity()
	time.Sleep(25 * time.Millisecond) // Reduced from 100ms for faster processing

	return nil
}

// Commands to skip (bot automated commands - saves rate limit)
var skipAdminCommands = []string{
	"listplayers",
}

// shouldSkipAdminCommand checks if command should be skipped
func shouldSkipAdminCommand(command string) bool {
	cmdLower := strings.ToLower(strings.TrimSpace(command))
	for _, skip := range skipAdminCommands {
		if strings.HasPrefix(cmdLower, skip) {
			return true
		}
	}
	return false
}

// Process admin lines - Using new cache system
func processAdminLines(lines []string) {
	// Check memory before processing
	if !GlobalResourceManager.CheckMemory() {
		log.Printf("⚠️ Skipping admin processing due to high memory usage")
		return
	}

	newLogs := false
	processedCount := 0
	skippedCount := 0

	for _, line := range lines {
		normalizedLine := strings.TrimSpace(line)
		logKey := normalizedLine + "_admin"

		if !IsLogSentCached(logKey) {
			adminInfo := parseAdminLog(normalizedLine)
			if adminInfo != nil {
				// Skip bot automated commands (saves Discord rate limit)
				if shouldSkipAdminCommand(adminInfo.Command) {
					MarkLogAsSentCached(logKey) // Mark as sent so we don't check again
					skippedCount++
					continue
				}

				// Send to admin channel
				if err := sendAdminLog(adminInfo); err != nil {
					log.Printf("Error sending admin log: %v", err)
				}

				// Send to public channel
				if err := sendPublicAdminLog(adminInfo); err != nil {
					log.Printf("Error sending public admin log: %v", err)
				}

				MarkLogAsSentCached(logKey)
				newLogs = true
				processedCount++
			}
		}
	}

	if newLogs || skippedCount > 0 {
		log.Printf("✅ Processed %d admin logs (skipped %d bot commands)", processedCount, skippedCount)
	}
}

// Local Log Processing for admin
func processAdminLocalLogs() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [ADMIN LOCAL] Panic recovered: %v", r)
			UpdateSharedActivity()
		}
	}()

	latestFile := getAdminLatestLocalLogFile()
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
		adminOffsetMux.RLock()
		offset := adminFileOffsets[latestFile]
		adminOffsetMux.RUnlock()

		// For large files, use streaming
		if fileInfo.Size()-offset > 10*1024*1024 { // If remaining > 10MB
			log.Printf("📊 Large update detected in admin log, using streaming")

			// Create a temporary file starting from offset
			tempFile := latestFile + ".partial"
			err := extractPartialFile(latestFile, tempFile, offset)
			if err != nil {
				log.Printf("Error extracting partial file: %v", err)
				return
			}
			defer os.Remove(tempFile)

			err = adminStreamProcessor.ProcessLargeFile(tempFile, processAdminLines)
			if err != nil {
				log.Printf("Error processing admin log file: %v", err)
				return
			}

			// Update offset
			adminOffsetMux.Lock()
			adminFileOffsets[latestFile] = fileInfo.Size()
			adminOffsetMux.Unlock()
			saveAdminFileOffsets()
		} else {
			// For smaller updates, use regular method
			lines, newOffset, err := readAdminFileFromOffset(latestFile, offset)
			if err != nil {
				log.Printf("Error reading admin file %s: %v", latestFile, err)
				return
			}

			if len(lines) > 0 {
				adminOffsetMux.Lock()
				adminFileOffsets[latestFile] = newOffset
				adminOffsetMux.Unlock()

				processAdminLines(lines)
				saveAdminFileOffsets()
			}
		}
	} else {
		// For large files without file watcher, use streaming
		if fileInfo.Size() > 10*1024*1024 { // If file > 10MB
			log.Printf("📊 Processing large admin log file (%.2f MB) with streaming",
				float64(fileInfo.Size())/1024/1024)
			err := adminStreamProcessor.ProcessLargeFile(latestFile, processAdminLines)
			if err != nil {
				log.Printf("Error processing admin log file: %v", err)
				return
			}
		} else {
			// Read entire file for smaller files
			lines, err := readAdminFileContent(latestFile)
			if err != nil {
				log.Printf("Error reading admin log file: %v", err)
				return
			}
			processAdminLines(lines)
		}
	}
}

// File Watcher for admin
func startAdminFileWatcher(ctx context.Context) {
	if !Config.UseLocalFiles || !Config.EnableFileWatcher {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [ADMIN WATCHER] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go startAdminFileWatcher(ctx)
		}
	}()

	var err error
	adminFileWatcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Error creating admin file watcher: %v", err)
		return
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(Config.LocalLogsPath, 0755); err != nil {
		log.Printf("Warning: Could not create admin logs directory: %v", err)
	}

	// Watch the logs directory
	err = adminFileWatcher.Add(Config.LocalLogsPath)
	if err != nil {
		log.Printf("Error watching admin logs directory: %v", err)
		return
	}

	go func() {
		defer adminFileWatcher.Close()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("❌ [ADMIN WATCHER LOOP] Panic recovered: %v", r)
			}
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-adminFileWatcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					filename := filepath.Base(event.Name)
					if strings.Contains(filename, "admin") {
						UpdateSharedActivity()
						processAdminLocalLogs()
					}
				}
			case err, ok := <-adminFileWatcher.Errors:
				if !ok {
					return
				}
				log.Printf("Admin file watcher error: %v", err)
			}
		}
	}()

	log.Printf("✅ Admin file watcher started for directory: %s", Config.LocalLogsPath)
}

// Periodic Log Checking for admin
func adminPeriodicLogCheck(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [ADMIN CHECK] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go adminPeriodicLogCheck(ctx)
		}
	}()

	// Use faster interval for admin logs (5 seconds like in Python version)
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
				log.Printf("⚠️ Skipping admin check due to high memory usage")
				continue
			}

			if Config.UseRemote {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("❌ [ADMIN REMOTE] Panic during processing: %v", r)
						}
					}()
					processAdminRemoteLogs()
				}()
			}
			if Config.UseLocalFiles {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("❌ [ADMIN LOCAL] Panic during processing: %v", r)
						}
					}()
					processAdminLocalLogs()
				}()
			}

			UpdateSharedActivity()
		}
	}
}

// Bot commands for admin
func adminMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [ADMIN MESSAGE] Panic recovered: %v", r)
		}
	}()

	// Add any admin-specific commands here
	if strings.HasPrefix(m.Content, "!adminstats") {
		showAdminStats(s, m)
	}
}

// Show admin statistics
func showAdminStats(s *discordgo.Session, m *discordgo.MessageCreate) {
	stats := SentLogsCache.Stats()

	embed := &discordgo.MessageEmbed{
		Title: "👮 Admin Module Statistics",
		Color: 0xFF0000,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name: "Cache Stats",
				Value: fmt.Sprintf("Size: %d/%d\nHit Rate: %.2f%%\nProcessed Commands: %d",
					stats.Size, stats.Capacity, stats.HitRate, stats.Hits),
				Inline: true,
			},
			{
				Name:   "Status",
				Value:  "✅ Active",
				Inline: true,
			},
			{
				Name: "Channels",
				Value: fmt.Sprintf("Admin: %s\nPublic: %s",
					Config.AdminChannelID,
					Config.AdminPublicChannelID),
				Inline: true,
			},
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	s.ChannelMessageSendEmbed(m.ChannelID, embed)
}
