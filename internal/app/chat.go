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

// Chat module specific variables
var (
	chatFileWatcher *fsnotify.Watcher
	chatFileOffsets = make(map[string]int64)
	chatOffsetMux   sync.RWMutex

	// Stream processor for large files
	chatStreamProcessor *StreamingFileProcessor
)

// ChatInfo represents parsed chat message
type ChatInfo struct {
	Date     string `json:"date"`
	Player   string `json:"player"`
	PlayerID string `json:"player_id"`
	Message  string `json:"message"`
	Channel  string `json:"channel"`
}

// Initialize chat module
func InitializeChat() error {
	// Initialize stream processor
	chatStreamProcessor = NewStreamingFileProcessor(
		32*1024, // 32KB buffer
		100,     // Max 100 items per batch
		5,       // Max 5 concurrent batches
	)

	// Load file offsets if using local files with file watcher
	if Config.UseLocalFiles && Config.EnableFileWatcher {
		if err := loadChatFileOffsets(); err != nil {
			log.Printf("Warning: Could not load chat file offsets: %v", err)
		}
	}

	log.Println("✅ Chat module initialized with performance enhancements")
	return nil
}

// Start chat module
func StartChat(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [CHAT START] Panic recovered: %v\n%s", r, debug.Stack())
			UpdateSharedActivity()
		}
	}()

	// Start file watcher if enabled
	if Config.UseLocalFiles && Config.EnableFileWatcher {
		startChatFileWatcher(ctx)
	}

	// Start periodic chat checking
	go chatPeriodicLogCheck(ctx)

	// Start progress monitor
	go monitorChatProgress(ctx)

	log.Printf("✅ Chat module started with enhanced performance!")
}

// Monitor progress from stream processor
func monitorChatProgress(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case progress := <-chatStreamProcessor.progressChan:
			log.Printf("📊 Chat processing: %.1f%% complete", progress)
		}
	}
}

func loadChatFileOffsets() error {
	chatOffsetMux.Lock()
	defer chatOffsetMux.Unlock()

	file, err := os.Open("chat_file_offsets.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	return decoder.Decode(&chatFileOffsets)
}

func saveChatFileOffsets() error {
	chatOffsetMux.RLock()
	defer chatOffsetMux.RUnlock()

	file, err := os.Create("chat_file_offsets.json")
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	return encoder.Encode(chatFileOffsets)
}

// Remote Log Processing for chat - HYBRID detection (Size OR ModTime)
func processChatRemoteLogs() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [CHAT REMOTE] Panic recovered: %v", r)
			UpdateSharedActivity()
		}
	}()

	// Find latest chat file using unified remote system
	latestFile, remotePath, err := GetLatestRemoteFile("chat")
	if err != nil {
		log.Printf("Error finding chat log files: %v", err)
		return
	}

	localDir := filepath.Join("logs", "chat")
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
			log.Printf("📥 Chat: Size changed (local: %d, remote: %d bytes)",
				localInfo.Size(), realFileInfo.Size)
		} else if modTimeChanged {
			log.Printf("📥 Chat: ModTime changed (local: %d, remote: %d)",
				localModTime, realFileInfo.ModTime)
		}
	}

	// Download if needed
	if needsDownload {
		err = DownloadFileRemote(remotePath, localPath, 3)
		if err != nil {
			log.Printf("Error downloading chat log: %v", err)
			return
		}
	}

	// ALWAYS process the file (cache system will prevent duplicate messages)
	fileInfo, err := os.Stat(localPath)
	if err != nil {
		log.Printf("Error accessing chat log file: %v", err)
		return
	}

	if fileInfo.Size() > 10*1024*1024 { // If file > 10MB, use streaming
		log.Printf("📊 Processing large chat log file (%.2f MB) with streaming",
			float64(fileInfo.Size())/1024/1024)
		err = chatStreamProcessor.ProcessLargeFile(localPath, processChatLines)
		if err != nil {
			log.Printf("Error processing chat log file: %v", err)
			return
		}
	} else {
		// For smaller files, use traditional method
		lines, err := readChatFileContent(localPath)
		if err != nil {
			log.Printf("Error reading chat log file: %v", err)
			return
		}
		processChatLines(lines)
	}
}

// Local File Functions for chat
func findChatLocalLogFiles() []string {
	pattern := filepath.Join(Config.LocalLogsPath, "*chat*")
	files, err := filepath.Glob(pattern)
	if err != nil {
		log.Printf("Error finding chat log files: %v", err)
		return nil
	}
	return files
}

func getChatLatestLocalLogFile() string {
	files := findChatLocalLogFiles()
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
func readChatFileContent(filePath string) ([]string, error) {
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

func readChatFileFromOffset(filePath string, offset int64) ([]string, int64, error) {
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

// Parse chat log using regex
func parseChatLog(line string) *ChatInfo {
	if line == "" {
		return nil
	}

	// Pattern: 2024.01.01-12.00.00: 'STEAMID:PlayerName(xx, yy)' 'Channel: Message'
	pattern := `(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): '(.*?)' '(.*?)'`
	re := regexp.MustCompile(pattern)
	matches := re.FindStringSubmatch(line)

	if len(matches) != 4 {
		return nil
	}

	// Parse player info (STEAMID:PlayerName(xx, yy))
	playerInfo := matches[2]
	playerParts := strings.SplitN(playerInfo, ":", 2)
	if len(playerParts) != 2 {
		return nil
	}

	steamID := strings.TrimSpace(playerParts[0])

	// Extract player name (remove coordinates)
	playerNameMatch := regexp.MustCompile(`^([^(]+)`).FindStringSubmatch(playerParts[1])
	playerName := ""
	if len(playerNameMatch) > 1 {
		playerName = strings.TrimSpace(playerNameMatch[1])
	}

	// Parse message info (Channel: Message)
	messageInfo := matches[3]
	messageParts := strings.SplitN(messageInfo, ": ", 2)
	if len(messageParts) != 2 {
		return nil
	}

	channel := strings.TrimSpace(messageParts[0])
	message := strings.TrimSpace(messageParts[1])

	return &ChatInfo{
		Date:     matches[1],
		Player:   playerName,
		PlayerID: steamID,
		Message:  message,
		Channel:  channel,
	}
}

// Send chat log to Discord
func sendChatLog(chatInfo *ChatInfo) error {
	if chatInfo == nil {
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [SEND CHAT LOG] Panic recovered: %v", r)
		}
	}()

	// Get current time in Bangkok timezone
	loc, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		loc = time.UTC // fallback to UTC
	}
	currentTime := time.Now().In(loc).Format("15:04")

	// Color codes for different channels
	channelColor := ""
	switch chatInfo.Channel {
	case "Squad":
		channelColor = "```ansi\n\u001b[32;1m" // Green
	case "Global":
		channelColor = "```ansi\n\u001b[36;1m" // Cyan
	case "Admin":
		channelColor = "```ansi\n\u001b[33;1m" // Yellow
	case "Local":
		channelColor = "```ansi\n\u001b[37;1m" // White
	default:
		channelColor = "```ansi\n\u001b[37;1m" // White
	}

	// Message with SteamID (for detailed channel)
	messageWithSteamID := fmt.Sprintf("%sDate: %s\nSteamID: %s\nName: %s\nChannel: %s\u001b[0m\nMessage: %s\n```",
		channelColor,
		chatInfo.Date,
		chatInfo.PlayerID,
		chatInfo.Player,
		chatInfo.Channel,
		chatInfo.Message,
	)

	// Message without SteamID (for simple global channel)
	messageWithoutSteamID := fmt.Sprintf("```ansi\n🕐 \u001b[36;1m%s\u001b[0m | \u001b[35;1m%s\u001b[0m: %s\n```",
		currentTime,
		chatInfo.Player,
		chatInfo.Message,
	)

	// Send to appropriate channels
	if chatInfo.Channel == "Global" {
		// Send to Global channel (without SteamID)
		if Config.ChatGlobalChannelID != "" {
			_, err := SharedSession.ChannelMessageSend(Config.ChatGlobalChannelID, messageWithoutSteamID)
			if err != nil {
				log.Printf("Error sending to global channel: %v", err)
			}
			UpdateSharedActivity()
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Always send to main chat channel (with SteamID)
	if Config.ChatChannelID != "" {
		_, err := SharedSession.ChannelMessageSend(Config.ChatChannelID, messageWithSteamID)
		if err != nil {
			return fmt.Errorf("error sending chat log: %v", err)
		}
		UpdateSharedActivity()
		time.Sleep(100 * time.Millisecond)
	}

	return nil
}

// Process chat lines - Using new cache system
func processChatLines(lines []string) {
	// Check memory before processing
	if !GlobalResourceManager.CheckMemory() {
		log.Printf("⚠️ Skipping chat processing due to high memory usage")
		return
	}

	newLogs := false
	processedCount := 0

	for _, line := range lines {
		normalizedLine := strings.TrimSpace(line)
		logKey := normalizedLine + "_chat"

		if !IsLogSentCached(logKey) {
			chatInfo := parseChatLog(normalizedLine)
			if chatInfo != nil {
				if err := sendChatLog(chatInfo); err != nil {
					log.Printf("Error sending chat log: %v", err)
				}

				MarkLogAsSentCached(logKey)
				newLogs = true
				processedCount++
			}
		}
	}

	if newLogs {
		log.Printf("✅ Processed %d new chat logs", processedCount)
	}
}

// Local Log Processing for chat
func processChatLocalLogs() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [CHAT LOCAL] Panic recovered: %v", r)
			UpdateSharedActivity()
		}
	}()

	latestFile := getChatLatestLocalLogFile()
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
		chatOffsetMux.RLock()
		offset := chatFileOffsets[latestFile]
		chatOffsetMux.RUnlock()

		// For large files, use streaming
		if fileInfo.Size()-offset > 10*1024*1024 { // If remaining > 10MB
			log.Printf("📊 Large update detected in chat log, using streaming")

			// Create a temporary file starting from offset
			tempFile := latestFile + ".partial"
			err := extractPartialFile(latestFile, tempFile, offset)
			if err != nil {
				log.Printf("Error extracting partial file: %v", err)
				return
			}
			defer os.Remove(tempFile)

			err = chatStreamProcessor.ProcessLargeFile(tempFile, processChatLines)
			if err != nil {
				log.Printf("Error processing chat log file: %v", err)
				return
			}

			// Update offset
			chatOffsetMux.Lock()
			chatFileOffsets[latestFile] = fileInfo.Size()
			chatOffsetMux.Unlock()
			saveChatFileOffsets()
		} else {
			// For smaller updates, use regular method
			lines, newOffset, err := readChatFileFromOffset(latestFile, offset)
			if err != nil {
				log.Printf("Error reading chat file %s: %v", latestFile, err)
				return
			}

			if len(lines) > 0 {
				chatOffsetMux.Lock()
				chatFileOffsets[latestFile] = newOffset
				chatOffsetMux.Unlock()

				processChatLines(lines)
				saveChatFileOffsets()
			}
		}
	} else {
		// For large files without file watcher, use streaming
		if fileInfo.Size() > 10*1024*1024 { // If file > 10MB
			log.Printf("📊 Processing large chat log file (%.2f MB) with streaming",
				float64(fileInfo.Size())/1024/1024)
			err := chatStreamProcessor.ProcessLargeFile(latestFile, processChatLines)
			if err != nil {
				log.Printf("Error processing chat log file: %v", err)
				return
			}
		} else {
			// Read entire file for smaller files
			lines, err := readChatFileContent(latestFile)
			if err != nil {
				log.Printf("Error reading chat log file: %v", err)
				return
			}
			processChatLines(lines)
		}
	}
}

// File Watcher for chat
func startChatFileWatcher(ctx context.Context) {
	if !Config.UseLocalFiles || !Config.EnableFileWatcher {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [CHAT WATCHER] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go startChatFileWatcher(ctx)
		}
	}()

	var err error
	chatFileWatcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Error creating chat file watcher: %v", err)
		return
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(Config.LocalLogsPath, 0755); err != nil {
		log.Printf("Warning: Could not create chat logs directory: %v", err)
	}

	// Watch the logs directory
	err = chatFileWatcher.Add(Config.LocalLogsPath)
	if err != nil {
		log.Printf("Error watching chat logs directory: %v", err)
		return
	}

	go func() {
		defer chatFileWatcher.Close()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("❌ [CHAT WATCHER LOOP] Panic recovered: %v", r)
			}
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-chatFileWatcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					filename := filepath.Base(event.Name)
					if strings.Contains(filename, "chat") {
						UpdateSharedActivity()
						processChatLocalLogs()
					}
				}
			case err, ok := <-chatFileWatcher.Errors:
				if !ok {
					return
				}
				log.Printf("Chat file watcher error: %v", err)
			}
		}
	}()

	log.Printf("✅ Chat file watcher started for directory: %s", Config.LocalLogsPath)
}

// Periodic Log Checking for chat - PARALLEL processing for speed
func chatPeriodicLogCheck(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [CHAT CHECK] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go chatPeriodicLogCheck(ctx)
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
				log.Printf("⚠️ Skipping chat check due to high memory usage")
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
							log.Printf("❌ [CHAT REMOTE] Panic during processing: %v", r)
						}
					}()
					processChatRemoteLogs()
				}()
			}
			if Config.UseLocalFiles {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() {
						if r := recover(); r != nil {
							log.Printf("❌ [CHAT LOCAL] Panic during processing: %v", r)
						}
					}()
					processChatLocalLogs()
				}()
			}
			wg.Wait()

			UpdateSharedActivity()
		}
	}
}

// Bot commands for chat
func chatMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [CHAT MESSAGE] Panic recovered: %v", r)
		}
	}()

	// Add any chat-specific commands here
	if strings.HasPrefix(m.Content, "!chatstats") {
		showChatStats(s, m)
	}
}

// Show chat statistics
func showChatStats(s *discordgo.Session, m *discordgo.MessageCreate) {
	stats := SentLogsCache.Stats()

	embed := &discordgo.MessageEmbed{
		Title: "💬 Chat Module Statistics",
		Color: 0x00FF00,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name: "Cache Stats",
				Value: fmt.Sprintf("Size: %d/%d\nHit Rate: %.2f%%\nProcessed Messages: %d",
					stats.Size, stats.Capacity, stats.HitRate, stats.Hits),
				Inline: true,
			},
			{
				Name:   "Status",
				Value:  "✅ Active",
				Inline: true,
			},
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	s.ChannelMessageSendEmbed(m.ChannelID, embed)
}
