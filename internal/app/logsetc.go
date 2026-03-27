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

// Paste module specific variables
var (
	pasteFileWatcher *fsnotify.Watcher
	pasteFileOffsets = make(map[string]int64)
	pasteOffsetMux   sync.RWMutex

	// Stream processor for large files
	pasteStreamProcessor *StreamingFileProcessor
)

// Log structures
type NameChangeInfo struct {
	Date       string `json:"date"`
	Action     string `json:"action"`
	PlayerID   string `json:"player_id"`
	PlayerName string `json:"player_name"`
	NewName    string `json:"new_name"`
}

type BankInfo struct {
	Date       string                 `json:"date"`
	Type       string                 `json:"type"`
	Action     string                 `json:"action"`
	User       string                 `json:"user"`
	UserID     string                 `json:"user_id"`
	AccountNum string                 `json:"account_num"`
	Details    map[string]interface{} `json:"details"`
	Location   string                 `json:"location"`
}

type VehicleInfo struct {
	Date        string `json:"date"`
	Event       string `json:"event"`
	VehicleType string `json:"vehicle_type"`
	VehicleID   string `json:"vehicle_id"`
	OwnerID     string `json:"owner_id"`
	OwnerName   string `json:"owner_name"`
	Location    string `json:"location"`
}

type ChestInfo struct {
	Date         string `json:"date"`
	ChestID      string `json:"chest_id"`
	OldOwnerID   string `json:"old_owner_id,omitempty"`
	OldOwnerName string `json:"old_owner_name,omitempty"`
	NewOwnerID   string `json:"new_owner_id,omitempty"`
	NewOwnerName string `json:"new_owner_name,omitempty"`
	OwnerID      string `json:"owner_id,omitempty"`
	OwnerName    string `json:"owner_name,omitempty"`
	Location     string `json:"location"`
}

type FlagInfo struct {
	Date         string `json:"date"`
	Action       string `json:"action"`
	FlagID       string `json:"flag_id"`
	OwnerID      string `json:"owner_id,omitempty"`
	OwnerName    string `json:"owner_name,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	UserName     string `json:"user_name,omitempty"`
	Location     string `json:"location"`
	OldOwnerID   string `json:"old_owner_id,omitempty"`
	OldOwnerName string `json:"old_owner_name,omitempty"`
}

type ViolationInfo struct {
	Date     string                 `json:"date"`
	Type     string                 `json:"type"`
	Details  map[string]interface{} `json:"details"`
	User     string                 `json:"user"`
	UserID   string                 `json:"user_id"`
	Location string                 `json:"location"`
}

// Regex patterns
var (
	nameChangePattern       = regexp.MustCompile(`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): Player ([^ ]+) \((\d+), (\d+)\) changed their name to ([^\.]+)\.`)
	purchasePattern         = regexp.MustCompile(`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d+): \[Bank\] ([^\(]+)\(ID:(\d+)\)\(Account Number:(\d+)\) purchased ([^\(]+) card \(([^)]+)\), new account balance is (\d+) credits, at X=([^ ]+) Y=([^ ]+) Z=([^ ]+)`)
	depositPattern          = regexp.MustCompile(`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d+): \[Bank\] ([^\(]+)\(ID:(\d+)\)\(Account Number:(\d+)\) deposited (\d+)\((\d+) was added\) to Account Number: (\d+)\(([^\)]+)\)\((\d+)\) at X=([^ ]+) Y=([^ ]+) Z=([^ ]+)`)
	destroyPattern          = regexp.MustCompile(`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d+): \[Bank\] ([^\(]+)\(ID:(\d+)\)\(Account Number:(\d+)\) manually destroyed ([^,]+) card belonging to Account Number:(\d+), at X=([^ ]+) Y=([^ ]+) Z=([^ ]+)`)
	vehiclePattern          = regexp.MustCompile(`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): \[(\w+)\] (\w+)\. VehicleId: (\d+)\. Owner: (N/A|\d+ \(([^)]+)\))\. Location: X=([^ ]+) Y=([^ ]+) Z=([^ ]+)`)
	ownershipChangedPattern = regexp.MustCompile(`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): Chest \(entity id: (\d+)\) ownership changed\. Old owner: (\d+) \(([^)]+)\), New owner: (\d+) \(([^)]+)\)\. Location: X=([^ ]+) Y=([^ ]+) Z=([^ ]+)`)
	ownershipClaimedPattern = regexp.MustCompile(`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): Chest \(entity id: (\d+)\) ownership claimed\. Owner: (\d+) \(([^)]+)\)\. Location: X=([^ ]+) Y=([^ ]+) Z=([^ ]+)`)
	createdPattern          = regexp.MustCompile(`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): \[LogBaseBuilding\] \[Flag\] Created\. FlagId: (\d+)\. Owner: (\d+) \(([^)]+)\)\. Location: X=([^ ]+) Y=([^ ]+) Z=([^ ]+)`)
	overtakeStartedPattern  = regexp.MustCompile(`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): \[LogBaseBuilding\] \[Flag\] Overtake started\. User: ([^ ]+) \((\d+), (\d+)\) Location: X=([^ ]+) Y=([^ ]+) Z=([^ ]+)\. Owner: (\d+) \(([^)]+)\)\. FlagId: (\d+)\. Location: X=([^ ]+) Y=([^ ]+) Z=([^ ]+)`)
	overtakeCanceledPattern = regexp.MustCompile(`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): \[LogBaseBuilding\] \[Flag\] Overtake canceled\. User: ([^ ]+) \((\d+), (\d+)\)\. Owner: (\d+) \(([^)]+)\)\. FlagId: (\d+)\. Location: X=([^ ]+) Y=([^ ]+) Z=([^ ]+)`)
	kickPattern             = regexp.MustCompile(`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d+): AConZGameMode::KickPlayer: User id: '(\d+)', Reason: ([^ ]+)`)
)

// Initialize paste module
func InitializePaste() error {
	// Initialize stream processor
	pasteStreamProcessor = NewStreamingFileProcessor(
		32*1024, // 32KB buffer
		100,     // Max 100 items per batch
		5,       // Max 5 concurrent batches
	)

	// Load file offsets if using local files with file watcher
	if Config.UseLocalFiles && Config.EnableFileWatcher {
		if err := loadPasteFileOffsets(); err != nil {
			log.Printf("Warning: Could not load paste file offsets: %v", err)
		}
	}

	log.Println("✅ Logs (Paste) module initialized with performance enhancements")
	return nil
}

// Start paste module
func StartPaste(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [PASTE START] Panic recovered: %v\n%s", r, debug.Stack())
			UpdateSharedActivity()
		}
	}()

	// Start file watcher if enabled
	if Config.UseLocalFiles && Config.EnableFileWatcher {
		startPasteFileWatcher(ctx)
	}

	// Start periodic log checking
	go pastePeriodicLogCheck(ctx)

	// Start progress monitor
	go monitorPasteProgress(ctx)

	log.Printf("✅ Logs (Paste) module started with enhanced performance!")
}

// Monitor progress from stream processor
func monitorPasteProgress(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case progress := <-pasteStreamProcessor.progressChan:
			log.Printf("📊 Paste logs processing: %.1f%% complete", progress)
		}
	}
}

func loadPasteFileOffsets() error {
	pasteOffsetMux.Lock()
	defer pasteOffsetMux.Unlock()

	file, err := os.Open("paste_file_offsets.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	return decoder.Decode(&pasteFileOffsets)
}

func savePasteFileOffsets() error {
	pasteOffsetMux.RLock()
	defer pasteOffsetMux.RUnlock()

	file, err := os.Create("paste_file_offsets.json")
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	return encoder.Encode(pasteFileOffsets)
}

// Remote Log Processing - HYBRID detection (Size OR ModTime)
func processPasteRemoteLogs(logType string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [PASTE REMOTE] Panic recovered: %v", r)
			UpdateSharedActivity()
		}
	}()

	// Find latest file using unified remote system
	latestFile, remotePath, err := GetLatestRemoteFile(logType)
	if err != nil {
		log.Printf("Error finding paste %s log files: %v", logType, err)
		return
	}

	localDir := filepath.Join("logs", logType)
	os.MkdirAll(localDir, 0755)
	localPath := filepath.Join(localDir, latestFile.Name)

	// Get REAL-TIME file size using FTP SIZE command
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
			log.Printf("📥 Paste %s: Size changed (local: %d, remote: %d bytes)",
				logType, localInfo.Size(), realFileInfo.Size)
		} else if modTimeChanged {
			log.Printf("📥 Paste %s: ModTime changed (local: %d, remote: %d)",
				logType, localModTime, realFileInfo.ModTime)
		}
	}

	// Download if needed
	if needsDownload {
		err = DownloadFileRemote(remotePath, localPath, 3)
		if err != nil {
			log.Printf("Error downloading paste %s log: %v", logType, err)
			return
		}
	}

	// ALWAYS process the file (cache system will prevent duplicate messages)
	fileInfo, err := os.Stat(localPath)
	if err != nil {
		log.Printf("Error accessing paste %s log file: %v", logType, err)
		return
	}

	if fileInfo.Size() > 10*1024*1024 { // If file > 10MB, use streaming
		log.Printf("📊 Processing large paste %s log file (%.2f MB) with streaming",
			logType, float64(fileInfo.Size())/1024/1024)
		err = pasteStreamProcessor.ProcessLargeFile(localPath, func(lines []string) {
			processPasteLines(lines, logType)
		})
		if err != nil {
			log.Printf("Error processing paste %s log file: %v", logType, err)
			return
		}
	} else {
		// For smaller files, use traditional method
		lines, err := readPasteFileContent(localPath)
		if err != nil {
			log.Printf("Error reading paste %s log file: %v", logType, err)
			return
		}
		processPasteLines(lines, logType)
	}
}

// Local File Functions
func findPasteLocalLogFiles(logType string) []string {
	pattern := filepath.Join(Config.LocalLogsPath, fmt.Sprintf("*%s*", logType))
	files, err := filepath.Glob(pattern)
	if err != nil {
		log.Printf("Error finding %s log files: %v", logType, err)
		return nil
	}
	return files
}

func getPasteLatestLocalLogFile(logType string) string {
	files := findPasteLocalLogFiles(logType)
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
func readPasteFileContent(filePath string) ([]string, error) {
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

func readPasteFileFromOffset(filePath string, offset int64) ([]string, int64, error) {
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

// Parse functions
func parseEconomyNameLog(line string) *NameChangeInfo {
	if line == "" {
		return nil
	}

	matches := nameChangePattern.FindStringSubmatch(line)
	if len(matches) != 6 {
		return nil
	}

	return &NameChangeInfo{
		Date:       matches[1],
		Action:     "Name Change",
		PlayerName: matches[2],
		PlayerID:   matches[4],
		NewName:    matches[5],
	}
}

func parseEconomyBankLog(line string) *BankInfo {
	if line == "" {
		return nil
	}

	var info *BankInfo

	if matches := purchasePattern.FindStringSubmatch(line); len(matches) == 11 {
		balance, _ := strconv.Atoi(matches[7])
		info = &BankInfo{
			Date:       matches[1],
			Type:       "Bank",
			Action:     "Purchased Card",
			User:       matches[2],
			UserID:     matches[3],
			AccountNum: matches[4],
			Details: map[string]interface{}{
				"card_type":      matches[5],
				"free_renewal":   matches[6],
				"balance_remain": balance,
			},
			Location: fmt.Sprintf("X=%s Y=%s Z=%s", matches[8], matches[9], matches[10]),
		}
	} else if matches := depositPattern.FindStringSubmatch(line); len(matches) == 13 {
		deposited, _ := strconv.Atoi(matches[5])
		added, _ := strconv.Atoi(matches[6])
		info = &BankInfo{
			Date:       matches[1],
			Type:       "Bank",
			Action:     "Deposited",
			User:       matches[2],
			UserID:     matches[3],
			AccountNum: matches[4],
			Details: map[string]interface{}{
				"amount_deposited": deposited,
				"amount_added":     added,
			},
			Location: fmt.Sprintf("X=%s Y=%s Z=%s", matches[10], matches[11], matches[12]),
		}
	} else if matches := destroyPattern.FindStringSubmatch(line); len(matches) == 10 {
		info = &BankInfo{
			Date:       matches[1],
			Type:       "Bank",
			Action:     "Destroyed Card",
			User:       matches[2],
			UserID:     matches[3],
			AccountNum: matches[4],
			Details: map[string]interface{}{
				"card_type": matches[5],
			},
			Location: fmt.Sprintf("X=%s Y=%s Z=%s", matches[7], matches[8], matches[9]),
		}
	}

	return info
}

func parseVehicleDestructionLog(line string) *VehicleInfo {
	if line == "" {
		return nil
	}

	matches := vehiclePattern.FindStringSubmatch(line)
	if len(matches) != 10 {
		return nil
	}

	info := &VehicleInfo{
		Date:        matches[1],
		Event:       matches[2],
		VehicleType: matches[3],
		VehicleID:   matches[4],
		Location:    fmt.Sprintf("X=%s Y=%s Z=%s", matches[7], matches[8], matches[9]),
	}

	ownerInfo := matches[5]
	if ownerInfo == "N/A" {
		info.OwnerID = "N/A"
		info.OwnerName = "N/A"
	} else {
		ownerMatch := regexp.MustCompile(`(\d+) \(([^,]+),\s*([^\)]+)\)`).FindStringSubmatch(ownerInfo)
		if len(ownerMatch) == 4 {
			info.OwnerID = ownerMatch[1]
			info.OwnerName = fmt.Sprintf("%s, %s", ownerMatch[2], ownerMatch[3])
		}
	}

	return info
}

func parseChestOwnershipLog(line string) *ChestInfo {
	if line == "" {
		return nil
	}

	var info *ChestInfo

	if matches := ownershipChangedPattern.FindStringSubmatch(line); len(matches) == 10 {
		info = &ChestInfo{
			Date:         matches[1],
			ChestID:      matches[2],
			OldOwnerID:   matches[3],
			OldOwnerName: matches[4],
			NewOwnerID:   matches[5],
			NewOwnerName: matches[6],
			Location:     fmt.Sprintf("X=%s Y=%s Z=%s", matches[7], matches[8], matches[9]),
		}
	} else if matches := ownershipClaimedPattern.FindStringSubmatch(line); len(matches) == 8 {
		info = &ChestInfo{
			Date:      matches[1],
			ChestID:   matches[2],
			OwnerID:   matches[3],
			OwnerName: matches[4],
			Location:  fmt.Sprintf("X=%s Y=%s Z=%s", matches[5], matches[6], matches[7]),
		}
	}

	return info
}

// parseFlagLog - Add this missing function
func parseFlagLog(line string) *FlagInfo {
	if line == "" {
		return nil
	}

	var info *FlagInfo

	// Check for Created pattern
	if matches := createdPattern.FindStringSubmatch(line); len(matches) == 8 {
		info = &FlagInfo{
			Date:      matches[1],
			Action:    "Created",
			FlagID:    matches[2],
			OwnerID:   matches[3],
			OwnerName: matches[4],
			Location:  fmt.Sprintf("X=%s Y=%s Z=%s", matches[5], matches[6], matches[7]),
		}
	} else if matches := overtakeStartedPattern.FindStringSubmatch(line); len(matches) == 14 {
		// Overtake started
		info = &FlagInfo{
			Date:         matches[1],
			Action:       "Overtake Started",
			UserName:     matches[2],
			UserID:       matches[4],
			OldOwnerID:   matches[8],
			OldOwnerName: matches[9],
			FlagID:       matches[10],
			Location:     fmt.Sprintf("X=%s Y=%s Z=%s", matches[11], matches[12], matches[13]),
		}
	} else if matches := overtakeCanceledPattern.FindStringSubmatch(line); len(matches) == 11 {
		// Overtake canceled
		info = &FlagInfo{
			Date:         matches[1],
			Action:       "Overtake Canceled",
			UserName:     matches[2],
			UserID:       matches[4],
			OldOwnerID:   matches[5],
			OldOwnerName: matches[6],
			FlagID:       matches[7],
			Location:     fmt.Sprintf("X=%s Y=%s Z=%s", matches[8], matches[9], matches[10]),
		}
	}

	return info
}

func parseViolationLog(line string) *ViolationInfo {
	if line == "" {
		return nil
	}

	matches := kickPattern.FindStringSubmatch(line)
	if len(matches) != 4 {
		return nil
	}

	return &ViolationInfo{
		Date:   matches[1],
		Type:   "KickPlayer",
		UserID: matches[2],
		User:   "ไม่ทราบ",
		Details: map[string]interface{}{
			"reason": matches[3],
		},
		Location: "ไม่มี",
	}
}

// Send functions
func sendEconomyNameLog(channel string, info *NameChangeInfo) error {
	if info == nil || channel == "" {
		return nil
	}

	msg := fmt.Sprintf("```\nวันที่: %s\nการกระทำ: %s\nPlayerID: %s ชื่อเก่า: %s ชื่อใหม่: %s\n```",
		info.Date, info.Action, info.PlayerID, info.PlayerName, info.NewName)

	_, err := SharedSession.ChannelMessageSend(channel, msg)
	if err != nil {
		return err
	}

	UpdateSharedActivity()
	time.Sleep(100 * time.Millisecond)
	return nil
}

func sendEconomyBankLog(channel string, info *BankInfo) error {
	if info == nil || channel == "" {
		return nil
	}

	var msg string
	switch info.Action {
	case "Purchased Card":
		msg = fmt.Sprintf("```\nวันที่: %s\nประเภท: %s\nการกระทำ: ซื้อการ์ด\nผู้ใช้: %s (ID: %s)\nเลขบัญชี: %s\nประเภทการ์ด: %s\nการต่ออายุฟรี: %s\nยอดเงินคงเหลือ: %d credits\nตำแหน่ง: %s\n```",
			info.Date, info.Type, info.User, info.UserID, info.AccountNum,
			info.Details["card_type"], info.Details["free_renewal"],
			info.Details["balance_remain"], info.Location)
	case "Deposited":
		msg = fmt.Sprintf("```\nวันที่: %s\nประเภท: %s\nการกระทำ: ฝากเงิน\nผู้ใช้: %s (ID: %s)\nเลขบัญชี: %s\nจำนวนเงินฝาก: %d credits\nจำนวนที่เพิ่ม: %d credits\nตำแหน่ง: %s\n```",
			info.Date, info.Type, info.User, info.UserID, info.AccountNum,
			info.Details["amount_deposited"], info.Details["amount_added"], info.Location)
	case "Destroyed Card":
		msg = fmt.Sprintf("```\nวันที่: %s\nประเภท: %s\nการกระทำ: ทำลายการ์ด\nผู้ใช้: %s (ID: %s)\nเลขบัญชี: %s\nประเภทการ์ด: %s\nตำแหน่ง: %s\n```",
			info.Date, info.Type, info.User, info.UserID, info.AccountNum,
			info.Details["card_type"], info.Location)
	}

	_, err := SharedSession.ChannelMessageSend(channel, msg)
	if err != nil {
		return err
	}

	UpdateSharedActivity()
	time.Sleep(100 * time.Millisecond)
	return nil
}

func sendVehicleDestructionLog(channel string, info *VehicleInfo) error {
	if info == nil || channel == "" {
		return nil
	}

	msg := fmt.Sprintf("```\nวันที่: %s\nเหตุการณ์: %s\nประเภทยานพาหนะ: %s\nรหัสยานพาหนะ: %s\nรหัสเจ้าของ: %s ชื่อเจ้าของ: %s\nตำแหน่ง: %s\n```",
		info.Date, info.Event, info.VehicleType, info.VehicleID,
		info.OwnerID, info.OwnerName, info.Location)

	_, err := SharedSession.ChannelMessageSend(channel, msg)
	if err != nil {
		return err
	}

	UpdateSharedActivity()
	time.Sleep(100 * time.Millisecond)
	return nil
}

func sendChestOwnershipLog(channel string, info *ChestInfo) error {
	if info == nil || channel == "" {
		return nil
	}

	var msg string
	if info.OldOwnerID != "" && info.NewOwnerID != "" {
		msg = fmt.Sprintf("```\nวันที่: %s\nChestID: %s\nOldOwnerID: %s OldOwnerName: %s\nNewOwnerID: %s NewOwnerName: %s\nตำแหน่ง: %s\n```",
			info.Date, info.ChestID, info.OldOwnerID, info.OldOwnerName,
			info.NewOwnerID, info.NewOwnerName, info.Location)
	} else {
		msg = fmt.Sprintf("```\nวันที่: %s\nChestID: %s\nOwnerID: %s OwnerName: %s\nตำแหน่ง: %s\n```",
			info.Date, info.ChestID, info.OwnerID, info.OwnerName, info.Location)
	}

	_, err := SharedSession.ChannelMessageSend(channel, msg)
	if err != nil {
		return err
	}

	UpdateSharedActivity()
	time.Sleep(100 * time.Millisecond)
	return nil
}

func sendFlagLog(channel string, info *FlagInfo) error {
	if info == nil || channel == "" {
		return nil
	}

	msg := fmt.Sprintf("```\nวันที่: %s\nการกระทำ: %s\nFlagID: %s\n", info.Date, info.Action, info.FlagID)

	if info.Action == "Created" {
		msg += fmt.Sprintf("OwnerID: %s OwnerName: %s\nตำแหน่ง: %s\n", info.OwnerID, info.OwnerName, info.Location)
	} else if info.Action == "Overtake Started" || info.Action == "Overtake Canceled" {
		msg += fmt.Sprintf("UserID: %s UserName: %s\nOldOwnerID: %s OldOwnerName: %s\nตำแหน่ง: %s\n",
			info.UserID, info.UserName, info.OldOwnerID, info.OldOwnerName, info.Location)
	}
	msg += "```"

	_, err := SharedSession.ChannelMessageSend(channel, msg)
	if err != nil {
		return err
	}

	UpdateSharedActivity()
	time.Sleep(100 * time.Millisecond)
	return nil
}

func sendViolationLog(channel string, info *ViolationInfo) error {
	if info == nil || channel == "" {
		return nil
	}

	msg := fmt.Sprintf("```\nวันที่: %s\nประเภท: %s\nID ผู้ใช้: %s\nเหตุผล: %s\nตำแหน่ง: %s\n```",
		info.Date, info.Type, info.UserID, info.Details["reason"], info.Location)

	_, err := SharedSession.ChannelMessageSend(channel, msg)
	if err != nil {
		return err
	}

	UpdateSharedActivity()
	time.Sleep(100 * time.Millisecond)
	return nil
}

// Process lines - Using new cache system
func processPasteLines(lines []string, logType string) {
	// Check memory before processing
	if !GlobalResourceManager.CheckMemory() {
		log.Printf("⚠️ Skipping paste processing due to high memory usage")
		return
	}

	newLogs := false
	processedCount := 0

	// Messages for each channel
	economyNameMessages := []NameChangeInfo{}
	economyBankMessages := []BankInfo{}
	vehicleMessages := []VehicleInfo{}
	chestMessages := []ChestInfo{}
	gameplayMessages := []FlagInfo{}
	violationMessages := []ViolationInfo{}

	for _, line := range lines {
		normalizedLine := strings.TrimSpace(line)
		logKey := normalizedLine + "_paste_" + logType

		if !IsLogSentCached(logKey) {
			switch logType {
			case "economy":
				if info := parseEconomyNameLog(normalizedLine); info != nil {
					economyNameMessages = append(economyNameMessages, *info)
					MarkLogAsSentCached(logKey)
					newLogs = true
					processedCount++
				}
				if info := parseEconomyBankLog(normalizedLine); info != nil {
					economyBankMessages = append(economyBankMessages, *info)
					MarkLogAsSentCached(logKey)
					newLogs = true
					processedCount++
				}

			case "vehicle_destruction":
				if info := parseVehicleDestructionLog(normalizedLine); info != nil {
					vehicleMessages = append(vehicleMessages, *info)
					MarkLogAsSentCached(logKey)
					newLogs = true
					processedCount++
				}

			case "chest_ownership":
				if info := parseChestOwnershipLog(normalizedLine); info != nil {
					chestMessages = append(chestMessages, *info)
					MarkLogAsSentCached(logKey)
					newLogs = true
					processedCount++
				}

			case "gameplay":
				if info := parseFlagLog(normalizedLine); info != nil {
					gameplayMessages = append(gameplayMessages, *info)
					MarkLogAsSentCached(logKey)
					newLogs = true
					processedCount++
				}

			case "violations":
				if info := parseViolationLog(normalizedLine); info != nil {
					violationMessages = append(violationMessages, *info)
					MarkLogAsSentCached(logKey)
					newLogs = true
					processedCount++
				}
			}
		}
	}

	// Send messages to channels
	if len(economyNameMessages) > 0 && Config.LogsChannelIDs["economy_name"] != "" {
		for _, info := range economyNameMessages {
			sendEconomyNameLog(Config.LogsChannelIDs["economy_name"], &info)
		}
	}

	if len(economyBankMessages) > 0 && Config.LogsChannelIDs["economy_bank"] != "" {
		for _, info := range economyBankMessages {
			sendEconomyBankLog(Config.LogsChannelIDs["economy_bank"], &info)
		}
	}

	if len(vehicleMessages) > 0 && Config.LogsChannelIDs["vehicle_destruction"] != "" {
		for _, info := range vehicleMessages {
			sendVehicleDestructionLog(Config.LogsChannelIDs["vehicle_destruction"], &info)
		}
	}

	if len(chestMessages) > 0 && Config.LogsChannelIDs["chest_ownership"] != "" {
		for _, info := range chestMessages {
			sendChestOwnershipLog(Config.LogsChannelIDs["chest_ownership"], &info)
		}
	}

	if len(gameplayMessages) > 0 && Config.LogsChannelIDs["gameplay"] != "" {
		for _, info := range gameplayMessages {
			sendFlagLog(Config.LogsChannelIDs["gameplay"], &info)
		}
	}

	if len(violationMessages) > 0 && Config.LogsChannelIDs["violations"] != "" {
		for _, info := range violationMessages {
			sendViolationLog(Config.LogsChannelIDs["violations"], &info)
		}
	}

	if newLogs {
		log.Printf("✅ Processed %d new paste logs for %s", processedCount, logType)
	}
}

// Local Log Processing
func processPasteLocalLogs(logType string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [PASTE LOCAL] Panic recovered: %v", r)
			UpdateSharedActivity()
		}
	}()

	latestFile := getPasteLatestLocalLogFile(logType)
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
		pasteOffsetMux.RLock()
		offset := pasteFileOffsets[latestFile]
		pasteOffsetMux.RUnlock()

		// For large files, use streaming
		if fileInfo.Size()-offset > 10*1024*1024 { // If remaining > 10MB
			log.Printf("📊 Large update detected in paste %s log, using streaming", logType)

			// Create a temporary file starting from offset
			tempFile := latestFile + ".partial"
			err := extractPartialFile(latestFile, tempFile, offset)
			if err != nil {
				log.Printf("Error extracting partial file: %v", err)
				return
			}
			defer os.Remove(tempFile)

			err = pasteStreamProcessor.ProcessLargeFile(tempFile, func(lines []string) {
				processPasteLines(lines, logType)
			})
			if err != nil {
				log.Printf("Error processing paste %s log file: %v", logType, err)
				return
			}

			// Update offset
			pasteOffsetMux.Lock()
			pasteFileOffsets[latestFile] = fileInfo.Size()
			pasteOffsetMux.Unlock()
			savePasteFileOffsets()
		} else {
			// For smaller updates, use regular method
			lines, newOffset, err := readPasteFileFromOffset(latestFile, offset)
			if err != nil {
				log.Printf("Error reading paste file %s: %v", latestFile, err)
				return
			}

			if len(lines) > 0 {
				pasteOffsetMux.Lock()
				pasteFileOffsets[latestFile] = newOffset
				pasteOffsetMux.Unlock()

				processPasteLines(lines, logType)
				savePasteFileOffsets()
			}
		}
	} else {
		// For large files without file watcher, use streaming
		if fileInfo.Size() > 10*1024*1024 { // If file > 10MB
			log.Printf("📊 Processing large paste %s log file (%.2f MB) with streaming",
				logType, float64(fileInfo.Size())/1024/1024)
			err := pasteStreamProcessor.ProcessLargeFile(latestFile, func(lines []string) {
				processPasteLines(lines, logType)
			})
			if err != nil {
				log.Printf("Error processing paste %s log file: %v", logType, err)
				return
			}
		} else {
			// Read entire file for smaller files
			lines, err := readPasteFileContent(latestFile)
			if err != nil {
				log.Printf("Error reading paste %s log file: %v", logType, err)
				return
			}
			processPasteLines(lines, logType)
		}
	}
}

// File Watcher
func startPasteFileWatcher(ctx context.Context) {
	if !Config.UseLocalFiles || !Config.EnableFileWatcher {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [PASTE WATCHER] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go startPasteFileWatcher(ctx)
		}
	}()

	var err error
	pasteFileWatcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Error creating paste file watcher: %v", err)
		return
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(Config.LocalLogsPath, 0755); err != nil {
		log.Printf("Warning: Could not create paste logs directory: %v", err)
	}

	// Watch the logs directory
	err = pasteFileWatcher.Add(Config.LocalLogsPath)
	if err != nil {
		log.Printf("Error watching paste logs directory: %v", err)
		return
	}

	go func() {
		defer pasteFileWatcher.Close()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("❌ [PASTE WATCHER LOOP] Panic recovered: %v", r)
			}
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-pasteFileWatcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					filename := filepath.Base(event.Name)
					logTypes := []string{"economy", "vehicle_destruction", "chest_ownership", "gameplay", "violations"}
					for _, logType := range logTypes {
						if strings.Contains(filename, logType) {
							processPasteLocalLogs(logType)
						}
					}
				}
			case err, ok := <-pasteFileWatcher.Errors:
				if !ok {
					return
				}
				log.Printf("Paste file watcher error: %v", err)
			}
		}
	}()

	log.Printf("✅ Paste file watcher started for directory: %s", Config.LocalLogsPath)
}

// Periodic Log Checking - PARALLEL processing for speed
func pastePeriodicLogCheck(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [PASTE CHECK] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go pastePeriodicLogCheck(ctx)
		}
	}()

	ticker := time.NewTicker(time.Duration(Config.CheckInterval) * time.Second)
	defer ticker.Stop()

	logTypes := []string{"economy", "vehicle_destruction", "chest_ownership", "gameplay", "violations"}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			UpdateSharedActivity()

			// Check memory before processing
			if !GlobalResourceManager.CheckMemory() {
				log.Printf("⚠️ Skipping paste check due to high memory usage")
				continue
			}

			// Process all log types in PARALLEL using goroutines
			var wg sync.WaitGroup
			for _, logType := range logTypes {
				wg.Add(1)
				go func(lt string) {
					defer wg.Done()
					defer func() {
						if r := recover(); r != nil {
							log.Printf("❌ [PASTE] Panic during %s processing: %v", lt, r)
						}
					}()

					if Config.UseRemote {
						processPasteRemoteLogs(lt)
					}
					if Config.UseLocalFiles {
						processPasteLocalLogs(lt)
					}
				}(logType)
			}
			wg.Wait()

			UpdateSharedActivity()
		}
	}
}
