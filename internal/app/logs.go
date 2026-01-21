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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// Logs module specific variables
var (
	logsFileWatcher *fsnotify.Watcher
	logsFileOffsets = make(map[string]int64)
	logsOffsetMux   sync.RWMutex
	loginTimes      = make(map[string]time.Time)
	loginMux        sync.RWMutex

	// Stream processor for large files
	logsStreamProcessor *StreamingFileProcessor
)

// Structures for log parsing
type EconomyLog struct {
	Date          string          `json:"date"`
	TradeType     string          `json:"trade_type"`
	Seller        string          `json:"seller"`
	SellerSteamID string          `json:"seller_steam_id"`
	Buyer         string          `json:"buyer"`
	TotalPrice    int             `json:"total_price"`
	Items         map[string]Item `json:"items"`
	UsersOnline   string          `json:"users_online"`
}

type Item struct {
	Quantity         int `json:"quantity"`
	TotalPrice       int `json:"total_price"`
	TransactionCount int `json:"transaction_count"`
}

type LoginLog struct {
	Date     time.Time `json:"date"`
	IP       string    `json:"ip"`
	SteamID  string    `json:"steam_id"`
	Name     string    `json:"name"`
	Action   string    `json:"action"`
	Location Location  `json:"location"`
	AsDrone  bool      `json:"as_drone"`
}

type Location struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type KillLog struct {
	Died          string `json:"died"`
	DiedSteamID   string `json:"died_steam_id"`
	Killer        string `json:"killer"`
	KillerSteamID string `json:"killer_steam_id"`
}

type GameplayLog struct {
	Date           time.Time `json:"date"`
	User           string    `json:"user"`
	SteamID        string    `json:"steam_id"`
	Success        string    `json:"success"`
	ElapsedTime    float64   `json:"elapsed_time"`
	FailedAttempts int       `json:"failed_attempts"`
	TargetObject   string    `json:"target_object"`
	TargetID       string    `json:"target_id"`
	LockType       string    `json:"lock_type"`
	UserOwner      string    `json:"user_owner"`
	Location       Location  `json:"location"`
}

// Initialize logs module
func InitializeLogs() error {
	// Initialize stream processor
	logsStreamProcessor = NewStreamingFileProcessor(
		32*1024, // 32KB buffer
		100,     // Max 100 items per batch
		5,       // Max 5 concurrent batches
	)

	// Load file offsets if using local files with file watcher
	if Config.UseLocalFiles && Config.EnableFileWatcher {
		if err := loadLogsFileOffsets(); err != nil {
			log.Printf("Warning: Could not load logs file offsets: %v", err)
		}
	}

	log.Println("✓ Logs module initialized with performance enhancements")
	return nil
}

// Start logs module
func StartLogs(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ [LOGS START] Panic recovered: %v\n%s", r, debug.Stack())
			UpdateSharedActivity()
		}
	}()

	// Start file watcher if enabled
	if Config.UseLocalFiles && Config.EnableFileWatcher {
		startLogsFileWatcher(ctx)
	}

	// Start periodic logs checking
	go logsPeriodicLogCheck(ctx)

	// Start progress monitor
	go monitorLogsProgress(ctx)

	log.Printf("✓ Logs module started with enhanced performance!")
}

// Monitor progress from stream processor
func monitorLogsProgress(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case progress := <-logsStreamProcessor.progressChan:
			log.Printf("📊 Logs processing: %.1f%% complete", progress)
		}
	}
}

func loadLogsFileOffsets() error {
	logsOffsetMux.Lock()
	defer logsOffsetMux.Unlock()

	file, err := os.Open("logs_file_offsets.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	return decoder.Decode(&logsFileOffsets)
}

func saveLogsFileOffsets() error {
	logsOffsetMux.RLock()
	defer logsOffsetMux.RUnlock()

	file, err := os.Create("logs_file_offsets.json")
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	return encoder.Encode(logsFileOffsets)
}

// Remote Functions for logs - HYBRID detection (Size OR ModTime)
// Uses CACHE for file list + SFTP Stat for fast size check
func processLogsRemoteLogs(logType string) {
	startTotal := time.Now()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ [LOGS REMOTE] Panic recovered: %v", r)
			UpdateSharedActivity()
		}
	}()

	// Step 1: Find latest file from CACHE (fast - no SFTP ReadDir)
	latestFile, remotePath, err := GetLatestRemoteFile(logType)
	if err != nil {
		log.Printf("Error finding %s log files: %v", logType, err)
		return
	}

	localDir := filepath.Join("logs", logType)
	os.MkdirAll(localDir, 0755)
	localPath := filepath.Join(localDir, latestFile.Name)

	// Step 2: Use SFTP Stat to get REAL-TIME file size (fast - single file)
	realFileInfo, err := GetRemoteFileStat(latestFile.Name)
	if err != nil {
		// Fall back to cached info if Stat fails
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
			log.Printf("📥 %s log: Size changed (local: %d, remote: %d bytes)",
				logType, localInfo.Size(), realFileInfo.Size)
		} else if modTimeChanged {
			log.Printf("📥 %s log: ModTime changed (local: %d, remote: %d)",
				logType, localModTime, realFileInfo.ModTime)
		}
	}

	// Download if needed
	if needsDownload {
		startDownload := time.Now()
		err = DownloadFileRemote(remotePath, localPath, 3)
		if logType == "economy" {
			log.Printf("⏱️ [ECONOMY] Download took %v", time.Since(startDownload))
		}
		if err != nil {
			log.Printf("Error downloading %s log: %v", logType, err)
			return
		}
	}

	if logType == "economy" {
		log.Printf("⏱️ [ECONOMY] Total processLogsRemoteLogs took %v (download needed: %v)", time.Since(startTotal), needsDownload)
	}

	// ALWAYS process the file (even if not downloaded - Paste module may have downloaded it)
	// The cache system will prevent duplicate Discord messages
	fileInfo, err := os.Stat(localPath)
	if err != nil {
		log.Printf("Error accessing %s log file: %v", logType, err)
		return
	}

	// Process the file
	if fileInfo.Size() > 10*1024*1024 { // If file > 10MB, use streaming
		log.Printf("📊 Processing large %s log file (%.2f MB) with streaming",
			logType, float64(fileInfo.Size())/1024/1024)
		err = logsStreamProcessor.ProcessLargeFile(localPath, func(lines []string) {
			processLogsLines(lines, logType)
		})
		if err != nil {
			log.Printf("Error processing %s log file: %v", logType, err)
			return
		}
	} else {
		// For smaller files, use traditional method
		lines, err := readLogsFileContent(localPath)
		if err != nil {
			log.Printf("Error reading %s log file: %v", logType, err)
			return
		}
		log.Printf("📖 Read %d lines from %s log file", len(lines), logType)
		processLogsLines(lines, logType)
	}
}

// Local File Functions for logs
func findLogsLocalLogFiles(logType string) []string {
	pattern := filepath.Join(Config.LocalLogsPath, fmt.Sprintf("*%s*", logType))
	files, err := filepath.Glob(pattern)
	if err != nil {
		log.Printf("Error finding %s log files: %v", logType, err)
		return nil
	}
	return files
}

func getLogsLatestLocalLogFile(logType string) string {
	files := findLogsLocalLogFiles(logType)
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
func readLogsFileContent(filePath string) ([]string, error) {
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

func readLogsFileFromOffset(filePath string, offset int64) ([]string, int64, error) {
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

// parseEconomyLog - แบบ Python: ยืดหยุ่น รองรับหลาย pattern
func parseEconomyLog(line string) *EconomyLog {
	if line == "" {
		return nil
	}

	// กรอง Before/After messages ก่อน (เหมือน Python)
	if strings.Contains(line, "Before") || strings.Contains(line, "After") {
		return nil
	}

	// ตรวจสอบว่าเป็น Trade/Purchase log
	if !strings.Contains(line, "Tradeable") {
		return nil
	}

	// Pattern 1: SOLD format (มี health field) - รองรับทั้งที่มีและไม่มี uses field
	soldPattern := `(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): \[(Trade|Purchase)\] Tradeable \((.+?) \(health: ([\d.]+)(?:, uses: (\d+))?\)\) (sold|bought) by (.+?)\((\d+)\) for (\d+) \((\d+) \+ \d+ worth of contained items\) to trader (.+?), old amount in store is ([\d-]+), new amount is ([\d-]+)`

	re := regexp.MustCompile(soldPattern)
	matches := re.FindStringSubmatch(line)

	if len(matches) >= 13 {
		// uses field เป็น optional (index 5)
		quantity := 1 // default ถ้าไม่มี uses
		if matches[5] != "" {
			quantity, _ = strconv.Atoi(matches[5])
		}
		// ใช้ matches[9] (for XXXX) แทน matches[10] (ตัวเลขในวงเล็บ)
		totalPrice, _ := strconv.Atoi(matches[9])

		// ดึง users online ถ้ามี (optional)
		usersOnline := "0"
		if strings.Contains(line, "and effective users online:") {
			onlinePattern := `and effective users online: (\d+)`
			onlineRe := regexp.MustCompile(onlinePattern)
			onlineMatch := onlineRe.FindStringSubmatch(line)
			if len(onlineMatch) >= 2 {
				usersOnline = onlineMatch[1]
			}
		}

		return &EconomyLog{
			Date:          matches[1],
			TradeType:     "Sale",
			Seller:        strings.TrimSpace(matches[7]),
			SellerSteamID: matches[8],
			Buyer:         strings.TrimSpace(matches[11]),
			TotalPrice:    totalPrice,
			Items: map[string]Item{
				strings.TrimSpace(matches[3]): {
					Quantity:         quantity,
					TotalPrice:       totalPrice,
					TransactionCount: 1,
				},
			},
			UsersOnline: usersOnline,
		}
	}

	// Pattern 2: PURCHASED format (ไม่มี health field) - เหมือน Python
	purchasedPattern := `(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): \[(Trade|Purchase)\] Tradeable \((.+?) \(x(\d+)\)\) purchased by (.+?)\((\d+)\) for (\d+) money from trader (.+?), old amount in store was ([\d-]+), new amount is ([\d-]+)`

	re = regexp.MustCompile(purchasedPattern)
	matches = re.FindStringSubmatch(line)

	if len(matches) >= 10 {
		quantity, _ := strconv.Atoi(matches[4])
		totalPrice, _ := strconv.Atoi(matches[7])

		// ดึง users online ถ้ามี (optional)
		usersOnline := "0"
		if strings.Contains(line, "and effective users online:") {
			onlinePattern := `and effective users online: (\d+)`
			onlineRe := regexp.MustCompile(onlinePattern)
			onlineMatch := onlineRe.FindStringSubmatch(line)
			if len(onlineMatch) >= 2 {
				usersOnline = onlineMatch[1]
			}
		}

		return &EconomyLog{
			Date:          matches[1],
			TradeType:     "Purchase",
			Seller:        strings.TrimSpace(matches[5]), // buyer คือผู้ซื้อ
			SellerSteamID: matches[6],
			Buyer:         strings.TrimSpace(matches[8]), // trader
			TotalPrice:    totalPrice,
			Items: map[string]Item{
				strings.TrimSpace(matches[3]): {
					Quantity:         quantity,
					TotalPrice:       totalPrice,
					TransactionCount: 1,
				},
			},
			UsersOnline: usersOnline,
		}
	}

	// ไม่แสดง debug message เหมือน Python (Python ไม่มี debug output สำหรับ unparseable lines)
	return nil
}

func parseLoginLog(line string) *LoginLog {
	if line == "" {
		return nil
	}

	pattern := `(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): '(\d+\.\d+\.\d+\.\d+) (\d+):([^(]+)\((\d+)\)' (logged in|logged out) at: X=([\d.-]+) Y=([\d.-]+) Z=([\d.-]+)(?: \((as drone)\))?`
	re := regexp.MustCompile(pattern)
	matches := re.FindStringSubmatch(line)

	if len(matches) < 10 {
		return nil
	}

	date, err := time.Parse("2006.01.02-15.04.05", matches[1])
	if err != nil {
		return nil
	}

	x, _ := strconv.ParseFloat(matches[7], 64)
	y, _ := strconv.ParseFloat(matches[8], 64)
	z, _ := strconv.ParseFloat(matches[9], 64)

	return &LoginLog{
		Date:    date.UTC(),
		IP:      matches[2],
		SteamID: matches[3],
		Name:    matches[4],
		Action:  matches[6],
		Location: Location{
			X: x,
			Y: y,
			Z: z,
		},
		AsDrone: len(matches) > 10 && matches[10] == "as drone",
	}
}

func parseKillLogForCount(line string) *KillLog {
	pattern := `(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): Died: ([^\(]+)\((\d+)\),\s*Killer: ([^\(]+)\((\d+)\)`
	re := regexp.MustCompile(pattern)
	matches := re.FindStringSubmatch(line)

	if len(matches) != 6 {
		return nil
	}

	return &KillLog{
		Died:          strings.TrimSpace(matches[2]),
		DiedSteamID:   strings.TrimSpace(matches[3]),
		Killer:        strings.TrimSpace(matches[4]),
		KillerSteamID: strings.TrimSpace(matches[5]),
	}
}

func parseGameplayLog(line string) *GameplayLog {
	// รองรับหลาย minigame types
	pattern := `(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): \[LogMinigame\] \[(LockpickingMinigame_C|BP_DialLockMinigame_C|BP_BombDefusalMinigame_C|BP_LockBombDefusalMinigame_C)\] User: (.+?) \((\d+), (.+?)\)\. Success: (Yes|No)\. Elapsed time: ([\d.]+)\. Failed attempts: (\d+)\. Target object: (.+?)\(ID: (.+?)\)\. Lock type: (.+?)\. User owner: (.+?)\. Location: X=([\d.-]+) Y=([\d.-]+) Z=([\d.-]+)`
	re := regexp.MustCompile(pattern)
	matches := re.FindStringSubmatch(line)

	if len(matches) != 16 {
		return nil
	}

	date, err := time.Parse("2006.01.02-15.04.05", matches[1])
	if err != nil {
		return nil
	}

	elapsedTime, _ := strconv.ParseFloat(matches[7], 64)
	failedAttempts, _ := strconv.Atoi(matches[8])
	x, _ := strconv.ParseFloat(matches[13], 64)
	y, _ := strconv.ParseFloat(matches[14], 64)
	z, _ := strconv.ParseFloat(matches[15], 64)

	return &GameplayLog{
		Date:           date.UTC(),
		User:           strings.TrimSpace(matches[3]),
		SteamID:        strings.TrimSpace(matches[5]),
		Success:        strings.TrimSpace(matches[6]),
		ElapsedTime:    elapsedTime,
		FailedAttempts: failedAttempts,
		TargetObject:   strings.TrimSpace(matches[9]),
		TargetID:       strings.TrimSpace(matches[10]),
		LockType:       strings.TrimSpace(matches[11]),
		UserOwner:      strings.TrimSpace(matches[12]),
		Location: Location{
			X: x,
			Y: y,
			Z: z,
		},
	}
}

// Discord Sending Functions
func sendEconomyLog(data *EconomyLog) error {
	if data == nil || Config.EconomyChannelID == "" {
		return nil
	}

	// Calculate total items and transactions
	totalItems := 0
	totalTransactions := 0
	for _, item := range data.Items {
		totalItems += item.Quantity
		totalTransactions += item.TransactionCount
	}

	var msg string
	if data.TradeType == "Purchase" {
		msg = fmt.Sprintf("```\n%s [%s] - PURCHASED\nTotal Items: %d\nTrader: %s\n\nItem - Quantity - Transactions - Money Spent\n",
			data.Seller, data.SellerSteamID, totalItems, data.Buyer)
	} else {
		msg = fmt.Sprintf("```\n%s [%s] - SOLD\nTotal Items Sold: %d\nTrader: %s\n\nItem - Quantity - Transactions - Money Received\n",
			data.Seller, data.SellerSteamID, totalItems, data.Buyer)
	}

	for itemName, details := range data.Items {
		msg += fmt.Sprintf("%s (x%d) for %d\n", itemName, details.Quantity, details.TotalPrice)
	}
	msg += "```"

	_, err := SharedSession.ChannelMessageSend(Config.EconomyChannelID, msg)
	if err != nil {
		log.Printf("ERROR: Failed to send economy log to Discord: %v", err)
		return err
	}

	log.Printf("✓ Sent economy log for %s", data.Seller)
	UpdateSharedActivity()
	time.Sleep(100 * time.Millisecond)
	return nil
}

func sendLoginLog(info *LoginLog) error {
	if info == nil || Config.LoginChannelID == "" {
		return nil
	}

	var msg string
	if info.Action == "logged in" {
		loginMux.Lock()
		loginTimes[info.SteamID] = info.Date
		loginMux.Unlock()

		msg = fmt.Sprintf("```diff\n+login ID: %s IP: %s Name: %s", info.SteamID, info.IP, info.Name)
		if info.AsDrone {
			msg += " (as drone)"
		}
		msg += "\n```"
	} else {
		loginMux.Lock()
		loginTime, exists := loginTimes[info.SteamID]
		delete(loginTimes, info.SteamID)
		loginMux.Unlock()

		var minutes float64
		if exists {
			minutes = info.Date.Sub(loginTime).Minutes()
		}

		msg = fmt.Sprintf("```diff\n-logout ID: %s Name: %s Minutes: %.2f Location: %.3f %.3f %.3f",
			info.SteamID, info.Name, minutes, info.Location.X, info.Location.Y, info.Location.Z)
		if info.AsDrone {
			msg += " (as drone)"
		}
		msg += "\n```"
	}

	_, err := SharedSession.ChannelMessageSend(Config.LoginChannelID, msg)
	if err != nil {
		return err
	}

	UpdateSharedActivity()
	time.Sleep(100 * time.Millisecond)
	return nil
}

func sendKillCountLog(info *KillLog) error {
	if info == nil || Config.KillCountChannelID == "" {
		return nil
	}

	msg := fmt.Sprintf("```\nKILL: %s Name: %s\nDEATH: %s Name: %s\n```",
		info.KillerSteamID, info.Killer, info.DiedSteamID, info.Died)

	_, err := SharedSession.ChannelMessageSend(Config.KillCountChannelID, msg)
	if err != nil {
		return err
	}

	UpdateSharedActivity()
	return nil
}

func sendGameplayLog(info *GameplayLog) error {
	if info == nil || Config.LockpickChannelID == "" {
		return nil
	}

	msg := fmt.Sprintf("```diff\nLockpick Log:\nUser: %s (%s)\nSuccess: %s\nElapsed Time: %.1fs\nFailed Attempts: %d\nTarget Object: %s TargetID: %s\nLock Type: %s\nUser Owner: %s\nLocation: X=%.3f Y=%.3f Z=%.3f\n```",
		info.User, info.SteamID, info.Success, info.ElapsedTime, info.FailedAttempts,
		info.TargetObject, info.TargetID, info.LockType, info.UserOwner,
		info.Location.X, info.Location.Y, info.Location.Z)

	_, err := SharedSession.ChannelMessageSend(Config.LockpickChannelID, msg)
	if err != nil {
		log.Printf("ERROR: Failed to send gameplay log to Discord: %v", err)
		return err
	}

	log.Printf("✓ Sent lockpick log for %s", info.User)
	UpdateSharedActivity()
	time.Sleep(100 * time.Millisecond)
	return nil
}

// Processing Functions - เหมือน Python ทุกอย่าง พร้อม debug
func processEconomyLines(lines []string) {
	log.Printf("🔍 [ECONOMY] Processing %d economy lines", len(lines))

	if len(lines) == 0 {
		log.Printf("⚠️ [ECONOMY] No lines to process")
		return
	}

	// แสดง sample line แรก
	if len(lines) > 0 {
		sampleLen := min(len(lines[0]), 150)
		log.Printf("📄 [ECONOMY] Sample first line: %s", lines[0][:sampleLen])
	}

	// Group logs by time and item first (เหมือน Python)
	type GroupedLog struct {
		Date          string
		TradeType     string
		Seller        string
		SellerSteamID string
		Buyer         string
		UsersOnline   string
		Item          string
		Quantity      int
		TotalPrice    int
		Lines         []string
	}

	groupedLogs := make(map[string]*GroupedLog)
	linesToCache := []string{}
	parsedCount := 0
	cachedCount := 0
	skippedCount := 0
	beforeAfterCount := 0

	// First pass: parse all lines without checking cache
	for i, line := range lines {
		// นับ Before/After messages
		if strings.Contains(line, "Before") || strings.Contains(line, "After") {
			beforeAfterCount++
			continue
		}

		logKey := line + "_economy"

		// Only process lines that haven't been cached yet
		if !IsLogSentCached(logKey) {
			info := parseEconomyLog(line)
			if info != nil {
				parsedCount++
				// แสดง sample parsed log แรกๆ
				if parsedCount <= 2 {
					log.Printf("✅ [ECONOMY] Successfully parsed line %d: Seller=%s SteamID=%s Type=%s Items=%d",
						i, info.Seller, info.SellerSteamID, info.TradeType, len(info.Items))
					for itemName, itemDetail := range info.Items {
						log.Printf("   └─ Item: %s x%d = %d money", itemName, itemDetail.Quantity, itemDetail.TotalPrice)
					}
				}

				for itemName, itemDetails := range info.Items {
					itemKey := fmt.Sprintf("%s_%s_%s_%s", info.Seller, info.SellerSteamID, info.Date, itemName)

					if existing, exists := groupedLogs[itemKey]; exists {
						existing.Quantity += itemDetails.Quantity
						existing.TotalPrice += itemDetails.TotalPrice
						existing.Lines = append(existing.Lines, line)
					} else {
						groupedLogs[itemKey] = &GroupedLog{
							Date:          info.Date,
							TradeType:     info.TradeType,
							Seller:        info.Seller,
							SellerSteamID: info.SellerSteamID,
							Buyer:         info.Buyer,
							UsersOnline:   info.UsersOnline,
							Item:          itemName,
							Quantity:      itemDetails.Quantity,
							TotalPrice:    itemDetails.TotalPrice,
							Lines:         []string{line},
						}
					}
				}
				linesToCache = append(linesToCache, line)
			} else {
				skippedCount++
				// แสดง sample ที่ skip แรกๆ - เพิ่มการแสดง full line เพื่อ debug
				if skippedCount <= 5 && strings.Contains(line, "Tradeable") {
					log.Printf("⚠️ [ECONOMY DEBUG] Skipped line %d (parse failed):", i)
					log.Printf("   FULL LINE: %s", line)
				}
			}
		} else {
			cachedCount++
		}
	}

	log.Printf("📊 [ECONOMY] Stats: Parsed=%d, Cached=%d, Skipped=%d, Before/After=%d, Grouped=%d items",
		parsedCount, cachedCount, skippedCount, beforeAfterCount, len(groupedLogs))

	if len(groupedLogs) == 0 {
		log.Printf("ℹ️ [ECONOMY] No new logs to process")
		return
	}

	// Aggregate by seller for final output (เหมือน Python)
	aggregated := make(map[string]*EconomyLog)
	for _, group := range groupedLogs {
		sellerKey := fmt.Sprintf("%s_%s", group.Seller, group.SellerSteamID)

		if existing, exists := aggregated[sellerKey]; exists {
			if existingItem, itemExists := existing.Items[group.Item]; itemExists {
				existingItem.Quantity += group.Quantity
				existingItem.TotalPrice += group.TotalPrice
				existingItem.TransactionCount += 1
				existing.Items[group.Item] = existingItem
			} else {
				existing.Items[group.Item] = Item{
					Quantity:         group.Quantity,
					TotalPrice:       group.TotalPrice,
					TransactionCount: 1,
				}
			}
			existing.TotalPrice += group.TotalPrice
		} else {
			aggregated[sellerKey] = &EconomyLog{
				Date:          group.Date,
				TradeType:     group.TradeType,
				Seller:        group.Seller,
				SellerSteamID: group.SellerSteamID,
				Buyer:         group.Buyer,
				UsersOnline:   group.UsersOnline,
				TotalPrice:    group.TotalPrice,
				Items: map[string]Item{
					group.Item: {
						Quantity:         group.Quantity,
						TotalPrice:       group.TotalPrice,
						TransactionCount: 1,
					},
				},
			}
		}
	}

	log.Printf("📦 [ECONOMY] Aggregated into %d seller groups", len(aggregated))

	// Send aggregated data first
	processedCount := 0
	successfulLines := []string{}

	for _, data := range aggregated {
		log.Printf("📤 [ECONOMY] Sending Discord message for %s [%s]...", data.Seller, data.SellerSteamID)
		if err := sendEconomyLog(data); err != nil {
			log.Printf("❌ [ECONOMY] Error sending economy log: %v", err)
		} else {
			processedCount++
			// เก็บ lines ที่ส่งสำเร็จเพื่อ cache หลังจากนี้
			for _, group := range groupedLogs {
				if group.Seller == data.Seller && group.SellerSteamID == data.SellerSteamID {
					successfulLines = append(successfulLines, group.Lines...)
				}
			}
		}
	}

	// Cache เฉพาะ lines ที่ส่ง Discord สำเร็จเท่านั้น
	if processedCount > 0 {
		for _, line := range successfulLines {
			logKey := line + "_economy"
			MarkLogAsSentCached(logKey)
		}
		log.Printf("✅ [ECONOMY] Processed %d aggregated economy logs and cached %d lines", processedCount, len(successfulLines))
	} else {
		log.Printf("⚠️ [ECONOMY] No logs were successfully sent to Discord")
	}
}

func processLoginLines(lines []string) {
	for _, line := range lines {
		logKey := line + "_login"
		if !IsLogSentCached(logKey) {
			info := parseLoginLog(line)
			if info != nil {
				if err := sendLoginLog(info); err != nil {
					log.Printf("Error sending login log: %v", err)
				} else {
					MarkLogAsSentCached(logKey)
				}
			}
		}
	}
}

func processKillCountLines(lines []string) {
	for _, line := range lines {
		logKey := line + "_killcount"
		if !IsLogSentCached(logKey) {
			info := parseKillLogForCount(line)
			if info != nil {
				if err := sendKillCountLog(info); err != nil {
					log.Printf("Error sending kill count log: %v", err)
				} else {
					MarkLogAsSentCached(logKey)
				}
			}
		}
	}
}

func processGameplayLines(lines []string) {
	log.Printf("DEBUG: Processing %d gameplay lines", len(lines))

	parsedCount := 0
	cachedCount := 0
	trapCount := 0

	for _, line := range lines {
		// ✅ Process trap logs and send to Discord
		if strings.Contains(line, "[LogTrap]") {
			trapLogKey := line + "_trap"
			if !IsLogSentCached(trapLogKey) {
				info := ParseTrapLog(line)
				if info != nil {
					if err := sendTrapLog(info); err != nil {
						log.Printf("Error sending trap log: %v", err)
					} else {
						MarkLogAsSentCached(trapLogKey)
						trapCount++
					}
				}
			}
			continue // Trap logs are separate from gameplay logs
		}

		logKey := line + "_gameplay"
		if !IsLogSentCached(logKey) {
			info := parseGameplayLog(line)
			if info != nil {
				parsedCount++
				if err := sendGameplayLog(info); err != nil {
					log.Printf("Error sending gameplay log: %v", err)
				} else {
					MarkLogAsSentCached(logKey)
				}
			}
		} else {
			cachedCount++
		}
	}

	log.Printf("DEBUG: Parsed %d new gameplay logs, %d cached, %d trap events", parsedCount, cachedCount, trapCount)
}

func processLogsLines(lines []string, logType string) {
	// Check memory before processing
	if !GlobalResourceManager.CheckMemory() {
		log.Printf("⚠️ Skipping logs processing due to high memory usage")
		return
	}

	switch logType {
	case "economy":
		processEconomyLines(lines)
	case "login":
		processLoginLines(lines)
	case "kill":
		// Only process kill count for logs module
		if Config.KillCountChannelID != "" {
			processKillCountLines(lines)
		}
	case "gameplay":
		processGameplayLines(lines)
	}
}

// Local Log Processing for logs
func processLogsLocalLogs(logType string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ [LOGS LOCAL] Panic recovered: %v", r)
			UpdateSharedActivity()
		}
	}()

	latestFile := getLogsLatestLocalLogFile(logType)
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
		logsOffsetMux.RLock()
		offset := logsFileOffsets[latestFile]
		logsOffsetMux.RUnlock()

		// For large files, use streaming
		if fileInfo.Size()-offset > 10*1024*1024 { // If remaining > 10MB
			log.Printf("📊 Large update detected in %s log, using streaming", logType)

			// Create a temporary file starting from offset
			tempFile := latestFile + ".partial"
			err := extractPartialFile(latestFile, tempFile, offset)
			if err != nil {
				log.Printf("Error extracting partial file: %v", err)
				return
			}
			defer os.Remove(tempFile)

			err = logsStreamProcessor.ProcessLargeFile(tempFile, func(lines []string) {
				processLogsLines(lines, logType)
			})
			if err != nil {
				log.Printf("Error processing %s log file: %v", logType, err)
				return
			}

			// Update offset
			logsOffsetMux.Lock()
			logsFileOffsets[latestFile] = fileInfo.Size()
			logsOffsetMux.Unlock()
			saveLogsFileOffsets()
		} else {
			// For smaller updates, use regular method
			lines, newOffset, err := readLogsFileFromOffset(latestFile, offset)
			if err != nil {
				log.Printf("Error reading logs file %s: %v", latestFile, err)
				return
			}

			if len(lines) > 0 {
				logsOffsetMux.Lock()
				logsFileOffsets[latestFile] = newOffset
				logsOffsetMux.Unlock()

				processLogsLines(lines, logType)
				saveLogsFileOffsets()
			}
		}
	} else {
		// For large files without file watcher, use streaming
		if fileInfo.Size() > 10*1024*1024 { // If file > 10MB
			log.Printf("📊 Processing large %s log file (%.2f MB) with streaming",
				logType, float64(fileInfo.Size())/1024/1024)
			err := logsStreamProcessor.ProcessLargeFile(latestFile, func(lines []string) {
				processLogsLines(lines, logType)
			})
			if err != nil {
				log.Printf("Error processing %s log file: %v", logType, err)
				return
			}
		} else {
			// Read entire file for smaller files
			lines, err := readLogsFileContent(latestFile)
			if err != nil {
				log.Printf("Error reading %s log file: %v", logType, err)
				return
			}
			processLogsLines(lines, logType)
		}
	}
}

// File Watcher for logs
func startLogsFileWatcher(ctx context.Context) {
	if !Config.UseLocalFiles || !Config.EnableFileWatcher {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ [LOGS WATCHER] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go startLogsFileWatcher(ctx)
		}
	}()

	var err error
	logsFileWatcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Error creating logs file watcher: %v", err)
		return
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(Config.LocalLogsPath, 0755); err != nil {
		log.Printf("Warning: Could not create logs directory: %v", err)
	}

	// Watch the logs directory
	err = logsFileWatcher.Add(Config.LocalLogsPath)
	if err != nil {
		log.Printf("Error watching logs directory: %v", err)
		return
	}

	go func() {
		defer logsFileWatcher.Close()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("⚠️ [LOGS WATCHER LOOP] Panic recovered: %v", r)
			}
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-logsFileWatcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					filename := filepath.Base(event.Name)
					if strings.Contains(filename, "economy") {
						processLogsLocalLogs("economy")
					} else if strings.Contains(filename, "login") {
						processLogsLocalLogs("login")
					} else if strings.Contains(filename, "kill") && Config.KillCountChannelID != "" {
						processLogsLocalLogs("kill")
					} else if strings.Contains(filename, "gameplay") {
						processLogsLocalLogs("gameplay")
					}
				}
			case err, ok := <-logsFileWatcher.Errors:
				if !ok {
					return
				}
				log.Printf("Logs file watcher error: %v", err)
			}
		}
	}()

	log.Printf("✓ Logs file watcher started for directory: %s", Config.LocalLogsPath)
}

// Periodic Log Checking for logs - PARALLEL processing for speed
func logsPeriodicLogCheck(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ [LOGS CHECK] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go logsPeriodicLogCheck(ctx)
		}
	}()

	ticker := time.NewTicker(time.Duration(Config.CheckInterval) * time.Second)
	defer ticker.Stop()

	logTypes := []string{"economy", "login", "kill", "gameplay"}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			UpdateSharedActivity()

			// Check memory before processing
			if !GlobalResourceManager.CheckMemory() {
				log.Printf("⚠️ Skipping logs check due to high memory usage")
				continue
			}

			// Process all log types in PARALLEL using goroutines
			var wg sync.WaitGroup
			for _, logType := range logTypes {
				// Skip kill logs if kill count channel is not configured
				if logType == "kill" && Config.KillCountChannelID == "" {
					continue
				}

				wg.Add(1)
				go func(lt string) {
					defer wg.Done()
					defer func() {
						if r := recover(); r != nil {
							log.Printf("⚠️ [LOGS] Panic during %s processing: %v", lt, r)
						}
					}()

					if Config.UseRemote {
						processLogsRemoteLogs(lt)
					}
					if Config.UseLocalFiles {
						processLogsLocalLogs(lt)
					}
				}(logType)
			}
			wg.Wait()

			UpdateSharedActivity()
		}
	}
}
