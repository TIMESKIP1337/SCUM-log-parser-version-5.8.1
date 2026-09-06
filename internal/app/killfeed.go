package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// Kill feed specific variables
var (
	killFeedFileWatcher *fsnotify.Watcher
	killFeedFileOffsets = make(map[string]int64)
	killFeedOffsetMux   sync.RWMutex

	// Weapon images mapping
	weaponImages    = make(map[string]string)
	weaponImagesMux sync.RWMutex

	// File tracking for remote operations
	processedFiles    = make(map[string]time.Time)
	processedFilesMux sync.RWMutex

	// Stream processor for large files
	killStreamProcessor *StreamProcessor

	// Weapon name cleaning
	weaponClassRegex *regexp.Regexp
	killFeedConfig   KillFeedConfig

	killFeedSendLimiter     chan struct{}
	killFeedSendLimiterOnce sync.Once
)

func acquireKillFeedSendSlot() func() {
	killFeedSendLimiterOnce.Do(func() {
		concurrency := getEnvInt("KILLFEED_SEND_CONCURRENCY", 2)
		if concurrency < 1 {
			concurrency = 1
		}
		killFeedSendLimiter = make(chan struct{}, concurrency)
		log.Printf("✅ Killfeed Discord send concurrency limited to %d", concurrency)
	})

	killFeedSendLimiter <- struct{}{}
	return func() {
		<-killFeedSendLimiter
	}
}

func resetMessageFileReaders(messageData *discordgo.MessageSend) {
	if messageData == nil {
		return
	}

	for _, file := range messageData.Files {
		if file == nil || file.Reader == nil {
			continue
		}

		if seeker, ok := file.Reader.(interface {
			Seek(offset int64, whence int) (int64, error)
		}); ok {
			if _, err := seeker.Seek(0, 0); err != nil {
				log.Printf("Warning: could not reset attachment reader %s: %v", file.Name, err)
			}
		}
	}
}

// KillFeedConfig holds configuration for kill feed
type KillFeedConfig struct {
	WeaponNames map[string]string `json:"weapon_names"`
}

// Kill log structure
type KillInfo struct {
	Date          string
	Killer        string
	KillerSteamID string // Steam ID of killer (empty if NPC)
	Died          string
	DiedSteamID   string // Steam ID of victim
	Weapon        string
	Distance      string
	IsNPC         bool // true if killer is NPC
}

// StreamProcessor handles streaming processing of large log files
type StreamProcessor struct {
	bufferSize   int
	maxBatchSize int
	processFunc  func([]string)
	rateLimiter  chan struct{}
	progressChan chan float64
}

// NewStreamProcessor creates a new stream processor
func NewStreamProcessor(bufferSize, maxBatchSize, rateLimit int) *StreamProcessor {
	return &StreamProcessor{
		bufferSize:   bufferSize,
		maxBatchSize: maxBatchSize,
		processFunc:  nil,
		rateLimiter:  make(chan struct{}, rateLimit),
		progressChan: make(chan float64, 100),
	}
}

// getRandomColor returns a random color for kill feed embeds
func getRandomColor() int {
	colors := []int{
		0xFF0000, // Red
		0x00FF00, // Green
		0x0000FF, // Blue
		0xFF8000, // Orange
		0xFF0080, // Pink
		0x8000FF, // Purple
		0x00FF80, // Cyan
		0xFF8080, // Light Red
		0x80FF80, // Light Green
		0x8080FF, // Light Blue
		0xFFFF00, // Yellow
		0xFF00FF, // Magenta
		0x00FFFF, // Cyan
		0xFFA500, // Orange
		0xFF1493, // Deep Pink
		0x9932CC, // Dark Orchid
		0x32CD32, // Lime Green
		0x1E90FF, // Dodger Blue
		0xFF6347, // Tomato
		0x40E0D0, // Turquoise
		0xEE82EE, // Violet
		0x90EE90, // Light Green
		0xFFB6C1, // Light Pink
		0x87CEEB, // Sky Blue
		0xDDA0DD, // Plum
	}

	return colors[rand.Intn(len(colors))]
}

// ProcessFile processes a file using streaming
func (sp *StreamProcessor) ProcessFile(filePath string, processFunc func([]string)) error {
	sp.processFunc = processFunc

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return err
	}

	totalSize := fileInfo.Size()
	if totalSize > 100*1024*1024 { // If file > 100MB
		log.Printf("âš ï¸ Large file detected (%.2f MB), using streaming mode", float64(totalSize)/1024/1024)
	}

	reader, err := NewMemoryEfficientFileReader(filePath)
	if err != nil {
		return err
	}
	defer reader.Close()

	batch := GetBatch()
	defer PutBatch(batch)

	var processedBytes int64
	lastProgressReport := time.Now()

	err = reader.ReadLines(func(line string) error {
		if strings.TrimSpace(line) != "" {
			*batch = append(*batch, line)
			processedBytes += int64(len(line)) + 1 // +1 for newline

			// Report progress
			if time.Since(lastProgressReport) > time.Second {
				progress := float64(processedBytes) / float64(totalSize) * 100
				select {
				case sp.progressChan <- progress:
				default:
				}
				lastProgressReport = time.Now()
				log.Printf("📊 Processing: %.1f%% complete", progress)
			}

			// Process batch when full
			if len(*batch) >= sp.maxBatchSize {
				sp.processBatch(*batch)
				*batch = (*batch)[:0] // Clear batch
			}
		}
		return nil
	})

	// Process remaining items
	if len(*batch) > 0 {
		sp.processBatch(*batch)
	}

	return err
}

// processBatch processes a batch of lines with rate limiting
func (sp *StreamProcessor) processBatch(lines []string) {
	if sp.processFunc == nil || len(lines) == 0 {
		return
	}

	// Rate limiting
	sp.rateLimiter <- struct{}{}
	defer func() { <-sp.rateLimiter }()

	// Process with resource management
	GlobalResourceManager.AcquireGoroutine()
	defer GlobalResourceManager.ReleaseGoroutine()

	sp.processFunc(lines)
}

// Initialize kill feed module
func InitializeKillFeed() error {
	// Initialize random seed for color randomization
	rand.Seed(time.Now().UnixNano())

	// Initialize weapon class regex pattern
	// This regex extracts the weapon class from full weapon path
	// Example: BP_Weapon_AK47_C -> AK47
	weaponClassRegex = regexp.MustCompile(`BP_Weapon_(\w+)_C|BP_Item_Weapon_(\w+)_C|(\w+)_C`)

	// Load kill feed config (weapon name mappings)
	if err := loadKillFeedConfig(); err != nil {
		log.Printf("Warning: Could not load kill feed config: %v (using defaults)", err)
		// Initialize with empty map if load fails
		killFeedConfig.WeaponNames = make(map[string]string)
	}

	// Initialize stream processor
	killStreamProcessor = NewStreamProcessor(
		32*1024, // 32KB buffer
		100,     // Max 100 items per batch
		5,       // Max 5 concurrent batches
	)

	// Load weapon images
	if err := loadWeaponImages(); err != nil {
		log.Printf("Warning: Could not load weapon images: %v", err)
	}

	// Load file offsets if using local files with file watcher
	if Config.UseLocalFiles && Config.EnableFileWatcher {
		if err := loadKillFeedFileOffsets(); err != nil {
			log.Printf("Warning: Could not load kill feed file offsets: %v", err)
		}
	}

	// Load processed files for remote operations
	if Config.UseRemote {
		if err := loadProcessedFiles(); err != nil {
			log.Printf("Warning: Could not load processed files: %v", err)
		}
	}

	log.Println("✅ Kill Feed module initialized with weapon name cleaning")
	return nil
}

// Start kill feed module
func StartKillFeed(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠ [KILLFEED START] Panic recovered: %v\n%s", r, debug.Stack())
			UpdateSharedActivity()
		}
	}()

	// Start file watcher if enabled
	if Config.UseLocalFiles && Config.EnableFileWatcher {
		startKillFeedFileWatcher(ctx)
	}

	// Start periodic kill feed log checking
	go killFeedPeriodicLogCheck(ctx)

	// Start progress monitor
	go monitorProgress(ctx)

	log.Printf("✅ Kill Feed module started with enhanced performance!")
}

// Monitor progress from stream processor
func monitorProgress(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case progress := <-killStreamProcessor.progressChan:
			log.Printf("📊 Kill feed processing: %.1f%% complete", progress)
		}
	}
}

// Processed files tracking for remote operations
func loadProcessedFiles() error {
	processedFilesMux.Lock()
	defer processedFilesMux.Unlock()

	file, err := os.Open("processed_files.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	return decoder.Decode(&processedFiles)
}

func saveProcessedFiles() error {
	processedFilesMux.Lock()
	defer processedFilesMux.Unlock()

	// Clean up old entries (older than 7 days)
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	for filename, processedTime := range processedFiles {
		if processedTime.Before(cutoff) {
			delete(processedFiles, filename)
		}
	}

	file, err := os.Create("processed_files.json")
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	return encoder.Encode(processedFiles)
}

// Weapon images functions
func loadWeaponImages() error {
	weaponImagesMux.Lock()
	defer weaponImagesMux.Unlock()

	file, err := os.Open("weapon_images.json")
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("⚠️ weapon_images.json not found. Will be created when weapons are detected.")
			return nil
		}
		return err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&weaponImages); err != nil {
		return err
	}

	log.Printf("✅ Loaded %d weapon images from weapon_images.json", len(weaponImages))
	return nil
}

func saveWeaponImages() error {
	weaponImagesMux.RLock()
	defer weaponImagesMux.RUnlock()

	file, err := os.Create("weapon_images.json")
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "    ")
	return encoder.Encode(weaponImages)
}

// ==========================================
// Weapon Name Cleaning Functions
// ==========================================

// loadKillFeedConfig loads weapon name mappings from config file
func loadKillFeedConfig() error {
	file, err := os.Open("killfeed_config.json")
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("⚠️ killfeed_config.json not found. Creating default config...")
			// Create default config
			killFeedConfig.WeaponNames = map[string]string{
				"AK47":       "AK-47",
				"M4A1":       "M4A1",
				"AWP":        "AWP Sniper",
				"Deagle":     "Desert Eagle",
				"M16A4":      "M16A4 Rifle",
				"SVD":        "SVD Dragunov",
				"Mosin":      "Mosin-Nagant",
				"MP5":        "MP5",
				"UZI":        "Uzi",
				"Winchester": "Winchester 1873",
			}
			return saveKillFeedConfig()
		}
		return err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&killFeedConfig); err != nil {
		return err
	}

	log.Printf("✅ Loaded %d weapon name mappings from killfeed_config.json", len(killFeedConfig.WeaponNames))
	return nil
}

// saveKillFeedConfig saves weapon name mappings to config file
func saveKillFeedConfig() error {
	file, err := os.Create("killfeed_config.json")
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "    ")
	return encoder.Encode(killFeedConfig)
}

// cleanWeaponName extracts and cleans weapon name from raw weapon string
func cleanWeaponName(rawWeapon string) string {
	if rawWeapon == "" {
		return "Unknown Weapon"
	}

	// Remove [...] suffixes (e.g., [Explosion])
	if idx := strings.Index(rawWeapon, "["); idx != -1 {
		rawWeapon = strings.TrimSpace(rawWeapon[:idx])
	}

	// Try to extract weapon class using regex (for BP_Weapon_XXX_C format)
	match := weaponClassRegex.FindStringSubmatch(rawWeapon)
	if len(match) > 1 {
		// Find the first non-empty capture group
		var weaponClass string
		for i := 1; i < len(match); i++ {
			if match[i] != "" {
				weaponClass = match[i]
				break
			}
		}

		if weaponClass != "" {
			// Check if we have a custom name mapping
			if customName, exists := killFeedConfig.WeaponNames[weaponClass]; exists {
				return customName
			}

			// Return the extracted class name
			return weaponClass
		}
	}

	// Handle DefaultWeapon prefix
	if strings.HasPrefix(rawWeapon, "DefaultWeapon") {
		weaponName := strings.TrimPrefix(rawWeapon, "DefaultWeapon")

		// Check if we have a custom name mapping
		if customName, exists := killFeedConfig.WeaponNames[weaponName]; exists {
			return customName
		}

		// Split CamelCase into words
		weaponName = splitCamelCase(weaponName)
		return weaponName
	}

	// Check if raw weapon has a mapping
	if customName, exists := killFeedConfig.WeaponNames[rawWeapon]; exists {
		return customName
	}

	// Last resort: try to split CamelCase if it looks like a class name
	if isLikelyCamelCase(rawWeapon) {
		return splitCamelCase(rawWeapon)
	}

	// If no match, return raw weapon name
	return rawWeapon
}

// splitCamelCase splits a CamelCase string into words
// Keeps consecutive uppercase letters and numbers together (e.g., MP5, AK47, SCARL)
func splitCamelCase(s string) string {
	if s == "" {
		return s
	}

	runes := []rune(s)
	var result []rune

	for i := 0; i < len(runes); i++ {
		r := runes[i]

		if i > 0 {
			prev := runes[i-1]
			isCurrentUpper := 'A' <= r && r <= 'Z'
			isPrevUpper := 'A' <= prev && prev <= 'Z'
			isPrevDigit := '0' <= prev && prev <= '9'

			// Add space before uppercase only if:
			// - Previous was lowercase (e.g., "weaponAK" -> "weapon AK")
			// - Current is uppercase followed by lowercase (e.g., "AKWeapon" -> "AK Weapon")
			if isCurrentUpper && !isPrevUpper && !isPrevDigit {
				result = append(result, ' ')
			} else if isCurrentUpper && isPrevUpper && i+1 < len(runes) {
				next := runes[i+1]
				isNextLower := 'a' <= next && next <= 'z'
				if isNextLower {
					result = append(result, ' ')
				}
			}
		}
		result = append(result, r)
	}

	return string(result)
}

// isLikelyCamelCase checks if string looks like CamelCase
func isLikelyCamelCase(s string) bool {
	if len(s) < 2 {
		return false
	}

	hasUpper := false
	hasLower := false

	for _, r := range s {
		if 'A' <= r && r <= 'Z' {
			hasUpper = true
		} else if 'a' <= r && r <= 'z' {
			hasLower = true
		}
	}

	// CamelCase should have both upper and lower case
	return hasUpper && hasLower
}

// ==========================================
// End of Weapon Name Cleaning Functions
// ==========================================

func checkAndAddWeaponImage(weaponName string) string {
	if weaponName == "" {
		return ""
	}

	weaponImagesMux.RLock()
	imageName, exists := weaponImages[weaponName]
	weaponImagesMux.RUnlock()

	if exists {
		return imageName
	}

	// Check for existing image files
	possibleExtensions := []string{".png", ".jpg", ".jpeg", ".gif"}
	normalizedName := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(weaponName, " ", "_"), "/", "_"))

	for _, ext := range possibleExtensions {
		filename := normalizedName + ext
		imagePath := filepath.Join(Config.WeaponsFolder, filename)
		if _, err := os.Stat(imagePath); err == nil {
			weaponImagesMux.Lock()
			weaponImages[weaponName] = filename
			weaponImagesMux.Unlock()
			log.Printf("✅ Added image mapping for weapon: %s -> %s", weaponName, filename)
			return filename
		}
	}

	// Add weapon without image
	weaponImagesMux.Lock()
	weaponImages[weaponName] = ""
	weaponImagesMux.Unlock()
	log.Printf("⚠️ Added weapon without image: %s", weaponName)
	return ""
}

func loadKillFeedFileOffsets() error {
	killFeedOffsetMux.Lock()
	defer killFeedOffsetMux.Unlock()

	file, err := os.Open("killfeed_file_offsets.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	return decoder.Decode(&killFeedFileOffsets)
}

func saveKillFeedFileOffsets() error {
	killFeedOffsetMux.RLock()
	defer killFeedOffsetMux.RUnlock()

	file, err := os.Create("killfeed_file_offsets.json")
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	return encoder.Encode(killFeedFileOffsets)
}

// Helper function to check if file should be processed
func isValidKillFile(filename string) bool {
	return strings.HasPrefix(filename, "kill_") && !strings.HasPrefix(filename, "event_kill_")
}

// Cache for kill feed file info to reduce SFTP calls
var (
	lastKillFileInfo  *RemoteFileInfo
	lastKillFilePath  string
	lastKillFileCheck time.Time
	killFileCacheTTL  = 2 * time.Second // Reduced from 5s to 2s for faster detection
	killFileCacheMux  sync.RWMutex
)

// Remote Log Processing for kill feed - Using unified connection system
func processKillFeedRemoteLogs() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠ [KILLFEED REMOTE] Panic recovered: %v", r)
			UpdateSharedActivity()
		}
	}()

	// Check cache first to avoid frequent SFTP calls
	killFileCacheMux.RLock()
	cacheValid := lastKillFileInfo != nil && time.Since(lastKillFileCheck) < killFileCacheTTL
	cachedInfo := lastKillFileInfo
	cachedPath := lastKillFilePath
	killFileCacheMux.RUnlock()

	var latestFile *RemoteFileInfo
	var remotePath string
	var err error

	if cacheValid {
		// Use cached info
		latestFile = cachedInfo
		remotePath = cachedPath
	} else {
		// Find latest kill file using unified remote system
		latestFile, remotePath, err = GetLatestKillFileRemote()
		if err != nil {
			log.Printf("Error finding kill feed files: %v", err)
			return
		}

		// Update cache
		killFileCacheMux.Lock()
		lastKillFileInfo = latestFile
		lastKillFilePath = remotePath
		lastKillFileCheck = time.Now()
		killFileCacheMux.Unlock()
	}

	localDir := filepath.Join("logs", "kill")
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
			// Only log occasionally to reduce spam
			if !cacheValid {
				log.Printf("✅ Kill feed file %s up to date (local: %d, remote: %d bytes)",
					latestFile.Name, localInfo.Size(), latestFile.Size)
			}
		} else if sizeChanged {
			log.Printf("📥 Kill feed: Size changed (local: %d, remote: %d bytes)",
				localInfo.Size(), latestFile.Size)
		} else if modTimeChanged {
			log.Printf("📥 Kill feed: ModTime changed (local: %d, remote: %d)",
				localModTime, latestFile.ModTime)
		}
	}

	// Download if needed
	if needsDownload {
		err = DownloadFileRemote(remotePath, localPath, 3)
		if err != nil {
			log.Printf("Error downloading kill feed log: %v", err)
			return
		}
	}

	// Process the file using stream processor for large files
	fileInfo, _ := os.Stat(localPath)
	if fileInfo.Size() > 10*1024*1024 { // If file > 10MB, use streaming
		log.Printf("📊 Processing large kill feed file (%.2f MB) with streaming",
			float64(fileInfo.Size())/1024/1024)
		err = killStreamProcessor.ProcessFile(localPath, OptimizedProcessKillLines)
		if err != nil {
			log.Printf("Error processing kill feed file: %v", err)
			return
		}
	} else {
		// For smaller files, use traditional method
		lines, err := readKillFeedFileContent(localPath)
		if err != nil {
			log.Printf("Error reading kill feed log file: %v", err)
			return
		}
		OptimizedProcessKillLines(lines)
	}

}

// Local File Functions for kill feed
func findKillFeedLocalLogFiles() []string {
	pattern := filepath.Join(Config.LocalLogsPath, "kill_*")
	files, err := filepath.Glob(pattern)
	if err != nil {
		log.Printf("Error finding kill log files: %v", err)
		return nil
	}

	var validFiles []string
	for _, file := range files {
		filename := filepath.Base(file)
		if isValidKillFile(filename) {
			validFiles = append(validFiles, file)
		}
	}

	return validFiles
}

func getKillFeedLatestLocalLogFile() string {
	files := findKillFeedLocalLogFiles()
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
func readKillFeedFileContent(filePath string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Check file size first
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}

	// Limit file size to 100MB
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

	if len(*lines) == maxLines {
		log.Printf("⚠️ File %s has more than %d lines, processing only first %d", filePath, maxLines, maxLines)
	}

	// Copy to return slice
	result := make([]string, len(*lines))
	copy(result, *lines)

	return result, nil
}

func readKillFeedFileFromOffset(filePath string, offset int64) ([]string, int64, error) {
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

	// Check file size
	const maxFileSize = 100 * 1024 * 1024
	if stat.Size() > maxFileSize {
		log.Printf("⚠️ File %s is too large (%.2f MB), reading from end", filePath, float64(stat.Size())/1024/1024)
		offset = stat.Size() - (10 * 1024 * 1024)
		if offset < 0 {
			offset = 0
		}
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

// Enhanced parse kill log with improved regex from Python version
func parseKillLog(line string) *KillInfo {
	return OptimizedParseKillLog(line)
}

// Send kill log to Discord
func sendKillLog(killInfo *KillInfo) (err error) {
	if killInfo == nil || Config.KillChannelID == "" {
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠ [SEND KILL LOG] Panic recovered: %v", r)
		}
	}()

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic sending kill log: %v", r)
		}
	}()

	// Clean weapon name for display
	cleanedWeaponName := cleanWeaponName(killInfo.Weapon)

	// Check and add weapon image (using original weapon name for mapping)
	checkAndAddWeaponImage(killInfo.Weapon)

	// Get embed from pool
	embed := embedPool.Get().(*discordgo.MessageEmbed)
	defer func() {
		// Clear embed before returning to pool
		embed.Fields = embed.Fields[:0]
		embed.Color = 0
		embed.Timestamp = ""
		embed.Footer = nil
		embed.Thumbnail = nil
		embedPool.Put(embed)
	}()

	// Configure embed with random color
	embed.Color = getRandomColor() // ใช้สีแบบสุ่มแทนสีแดงเดิม
	embed.Timestamp = time.Now().Format(time.RFC3339)
	embed.Footer = &discordgo.MessageEmbedFooter{
		Text:    "Powered by Timeskip | ข้อมูลล่าสุด",
		IconURL: Config.DefaultWeaponImage,
	}
	embed.Fields = append(embed.Fields[:0], // Clear and append
		&discordgo.MessageEmbedField{
			Name:   "⚔️ Killer",
			Value:  fmt.Sprintf("**%s**", killInfo.Killer),
			Inline: false,
		},
		&discordgo.MessageEmbedField{
			Name:   "💀 Victim",
			Value:  fmt.Sprintf("**%s**", killInfo.Died),
			Inline: false,
		},
		&discordgo.MessageEmbedField{
			Name:   "➨ Weapon",
			Value:  fmt.Sprintf("**%s**", cleanedWeaponName),
			Inline: false,
		},
		&discordgo.MessageEmbedField{
			Name:   "➨ Distance",
			Value:  fmt.Sprintf("**%s**", killInfo.Distance),
			Inline: true,
		},
	)

	// Prepare message data
	messageData := &discordgo.MessageSend{
		Embed: embed,
	}

	// Check if killer is NPC - use NPC.jpg if available
	if killInfo.IsNPC {
		// Try to find NPC.jpg (or other extensions)
		npcImageFound := false
		possibleExtensions := []string{".jpg", ".jpeg", ".png", ".gif"}

		for _, ext := range possibleExtensions {
			npcImageName := "NPC" + ext
			npcImagePath := filepath.Join(Config.WeaponsFolder, npcImageName)
			if file, err := os.Open(npcImagePath); err == nil {
				defer file.Close()
				messageData.Files = []*discordgo.File{
					{
						Name:   npcImageName,
						Reader: file,
					},
				}
				embed.Thumbnail = &discordgo.MessageEmbedThumbnail{
					URL: fmt.Sprintf("attachment://%s", npcImageName),
				}
				log.Printf("✅ Using NPC image: %s", npcImageName)
				npcImageFound = true
				break
			}
		}

		if !npcImageFound {
			log.Printf("⚠️ NPC image not found, using default")
			embed.Thumbnail = &discordgo.MessageEmbedThumbnail{
				URL: Config.DefaultWeaponImage,
			}
		}
	} else {
		// Check if we have a weapon image (for player kills)
		// Try both raw weapon name and cleaned weapon name
		weaponImagesMux.RLock()
		imageName, hasImage := weaponImages[killInfo.Weapon]
		if !hasImage || imageName == "" {
			// Try cleaned weapon name as fallback
			imageName, hasImage = weaponImages[cleanedWeaponName]
		}
		weaponImagesMux.RUnlock()

		if hasImage && imageName != "" {
			imagePath := filepath.Join(Config.WeaponsFolder, imageName)
			if file, err := os.Open(imagePath); err == nil {
				defer file.Close()
				messageData.Files = []*discordgo.File{
					{
						Name:   imageName,
						Reader: file,
					},
				}
				embed.Thumbnail = &discordgo.MessageEmbedThumbnail{
					URL: fmt.Sprintf("attachment://%s", imageName),
				}
				log.Printf("✅ Using weapon image: %s", imageName)
			} else {
				log.Printf("⚠️ Weapon image file not found: %s", imagePath)
				embed.Thumbnail = &discordgo.MessageEmbedThumbnail{
					URL: Config.DefaultWeaponImage,
				}
			}
		} else {
			embed.Thumbnail = &discordgo.MessageEmbedThumbnail{
				URL: Config.DefaultWeaponImage,
			}
			log.Printf("🖼️ Using default image for weapon: %s", killInfo.Weapon)
		}
	}

	// Send message with retry
	releaseSendSlot := acquireKillFeedSendSlot()
	defer releaseSendSlot()

	for attempt := 0; attempt < 3; attempt++ {
		resetMessageFileReaders(messageData)
		_, err = SharedSession.ChannelMessageSendComplex(Config.KillChannelID, messageData)
		if err == nil {
			break
		}
		log.Printf("⚠️ Failed to send kill log (attempt %d/3): %v", attempt+1, err)
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}

	if err != nil {
		return fmt.Errorf("error sending kill log after 3 attempts: %v", err)
	}

	log.Printf("📤 Sent kill log: %s killed %s with %s", killInfo.Killer, killInfo.Died, cleanedWeaponName)
	UpdateSharedActivity()

	// Send NPC kill to killcount channel for ranking system
	if killInfo.IsNPC && Config.KillCountChannelID != "" {
		sendNPCKillToKillCount(killInfo)
	}

	return nil
}

// sendNPCKillToKillCount sends NPC kill info to killcount channel for ranking
func sendNPCKillToKillCount(killInfo *KillInfo) {
	if killInfo == nil || !killInfo.IsNPC || Config.KillCountChannelID == "" {
		return
	}

	// Skip if victim has no Steam ID
	if killInfo.DiedSteamID == "" {
		log.Printf("⚠️ NPC kill skipped - no victim Steam ID")
		return
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠ [SEND NPC KILL COUNT] Panic recovered: %v", r)
		}
	}()

	// Format for ranking.go to parse:
	// NPC_KILL: NPCName
	// NPC_DEATH: SteamID Name: PlayerName
	msg := fmt.Sprintf("```\nNPC_KILL: %s\nNPC_DEATH: %s Name: %s\n```",
		killInfo.Killer,
		killInfo.DiedSteamID,
		killInfo.Died)

	_, err := SharedSession.ChannelMessageSend(Config.KillCountChannelID, msg)
	if err != nil {
		log.Printf("⚠️ Failed to send NPC kill to killcount channel: %v", err)
	} else {
		log.Printf("📤 Sent NPC kill to killcount: %s killed %s (%s)", killInfo.Killer, killInfo.Died, killInfo.DiedSteamID)
	}
}

// Process kill lines using optimized version with memory pool
func processKillLines(lines []string) {
	OptimizedProcessKillLines(lines)
}

// Local Log Processing for kill feed
func processKillFeedLocalLogs() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠ [KILLFEED LOCAL] Panic recovered: %v", r)
			UpdateSharedActivity()
		}
	}()

	latestFile := getKillFeedLatestLocalLogFile()
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
		killFeedOffsetMux.RLock()
		offset := killFeedFileOffsets[latestFile]
		killFeedOffsetMux.RUnlock()

		// For large files, use streaming
		if fileInfo.Size()-offset > 10*1024*1024 { // If remaining > 10MB
			log.Printf("📊 Large update detected in %s, using streaming", latestFile)

			// Create a temporary file starting from offset
			tempFile := latestFile + ".partial"
			err := extractPartialFile(latestFile, tempFile, offset)
			if err != nil {
				log.Printf("Error extracting partial file: %v", err)
				return
			}
			defer os.Remove(tempFile)

			err = killStreamProcessor.ProcessFile(tempFile, OptimizedProcessKillLines)
			if err != nil {
				log.Printf("Error processing kill feed file: %v", err)
				return
			}

			// Update offset
			killFeedOffsetMux.Lock()
			killFeedFileOffsets[latestFile] = fileInfo.Size()
			killFeedOffsetMux.Unlock()
			saveKillFeedFileOffsets()
		} else {
			// For smaller updates, use regular method
			lines, newOffset, err := readKillFeedFileFromOffset(latestFile, offset)
			if err != nil {
				log.Printf("Error reading kill feed file %s: %v", latestFile, err)
				return
			}

			if len(lines) > 0 {
				killFeedOffsetMux.Lock()
				killFeedFileOffsets[latestFile] = newOffset
				killFeedOffsetMux.Unlock()

				OptimizedProcessKillLines(lines)
				saveKillFeedFileOffsets()
			}
		}
	} else {
		// For large files without file watcher, use streaming
		if fileInfo.Size() > 10*1024*1024 { // If file > 10MB
			log.Printf("📊 Processing large kill feed file (%.2f MB) with streaming",
				float64(fileInfo.Size())/1024/1024)
			err := killStreamProcessor.ProcessFile(latestFile, OptimizedProcessKillLines)
			if err != nil {
				log.Printf("Error processing kill feed file: %v", err)
				return
			}
		} else {
			// Read entire file for smaller files
			lines, err := readKillFeedFileContent(latestFile)
			if err != nil {
				log.Printf("Error reading kill feed log file: %v", err)
				return
			}
			OptimizedProcessKillLines(lines)
		}
	}
}

// File Watcher for kill feed
func startKillFeedFileWatcher(ctx context.Context) {
	if !Config.UseLocalFiles || !Config.EnableFileWatcher {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠ [KILLFEED WATCHER] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go startKillFeedFileWatcher(ctx)
		}
	}()

	var err error
	killFeedFileWatcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Error creating kill feed file watcher: %v", err)
		return
	}

	if err := os.MkdirAll(Config.LocalLogsPath, 0755); err != nil {
		log.Printf("Warning: Could not create kill feed logs directory: %v", err)
	}

	err = killFeedFileWatcher.Add(Config.LocalLogsPath)
	if err != nil {
		log.Printf("Error watching kill feed logs directory: %v", err)
		return
	}

	go func() {
		defer killFeedFileWatcher.Close()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("⚠ [KILLFEED WATCHER LOOP] Panic recovered: %v", r)
			}
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-killFeedFileWatcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					filename := filepath.Base(event.Name)
					if isValidKillFile(filename) {
						UpdateSharedActivity()
						processKillFeedLocalLogs()
					}
				}
			case err, ok := <-killFeedFileWatcher.Errors:
				if !ok {
					return
				}
				log.Printf("Kill feed file watcher error: %v", err)
			}
		}
	}()

	log.Printf("✅ Kill feed file watcher started for directory: %s", Config.LocalLogsPath)
}

// Periodic Log Checking for kill feed
func killFeedPeriodicLogCheck(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠ [KILLFEED CHECK] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go killFeedPeriodicLogCheck(ctx)
		}
	}()

	// Use dedicated killfeed interval (faster than other modules)
	interval := Config.KillfeedCheckInterval
	if interval <= 0 {
		interval = 2 // Default 2 seconds
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	log.Printf("🎯 Kill feed checking every %d seconds", interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			UpdateSharedActivity()

			// Check memory before processing
			if !GlobalResourceManager.CheckMemory() {
				log.Printf("⚠️ Skipping kill feed check due to high memory usage")
				continue
			}

			if Config.UseRemote {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("⚠ [KILLFEED REMOTE] Panic during processing: %v", r)
						}
					}()
					processKillFeedRemoteLogs()
				}()
			}
			if Config.UseLocalFiles {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("⚠ [KILLFEED LOCAL] Panic during processing: %v", r)
						}
					}()
					processKillFeedLocalLogs()
				}()
			}

			UpdateSharedActivity()
		}
	}
}

// Bot commands for kill feed
func killFeedMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠ [KILLFEED MESSAGE] Panic recovered: %v", r)
		}
	}()

	// Check for commands
	if strings.HasPrefix(m.Content, "!weapons") {
		listWeaponsCommand(s, m)
	} else if strings.HasPrefix(m.Content, "!setweapon") {
		setWeaponCommand(s, m)
	} else if strings.HasPrefix(m.Content, "!testremote") {
		testRemoteCommand(s, m)
	} else if strings.HasPrefix(m.Content, "!killstats") {
		showKillStats(s, m)
	}
}

// New command to show kill feed statistics
func showKillStats(s *discordgo.Session, m *discordgo.MessageCreate) {
	stats := SentLogsCache.Stats()

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	embed := &discordgo.MessageEmbed{
		Title: "📊 Kill Feed Statistics",
		Color: 0x0099FF,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name: "Cache Stats",
				Value: fmt.Sprintf("Size: %d/%d\nHit Rate: %.2f%%\nHits: %d\nMisses: %d\nEvictions: %d",
					stats.Size, stats.Capacity, stats.HitRate, stats.Hits, stats.Misses, stats.Evictions),
				Inline: true,
			},
			{
				Name: "Memory Usage",
				Value: fmt.Sprintf("Allocated: %.2f MB\nSystem: %.2f MB\nGC Runs: %d",
					float64(memStats.Alloc)/1024/1024,
					float64(memStats.Sys)/1024/1024,
					memStats.NumGC),
				Inline: true,
			},
			{
				Name:   "Weapons Tracked",
				Value:  fmt.Sprintf("Total: %d weapons", len(weaponImages)),
				Inline: true,
			},
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	s.ChannelMessageSendEmbed(m.ChannelID, embed)
}

func listWeaponsCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	weaponImagesMux.RLock()
	defer weaponImagesMux.RUnlock()

	if len(weaponImages) == 0 {
		s.ChannelMessageSend(m.ChannelID, "ยังไม่มีรายการอาวุธที่บันทึกไว้")
		return
	}

	embed := &discordgo.MessageEmbed{
		Title:       "รายการอาวุธที่บันทึกไว้",
		Color:       0x0099FF,
		Description: fmt.Sprintf("จำนวนอาวุธทั้งหมด: %d", len(weaponImages)),
		Fields:      []*discordgo.MessageEmbedField{},
	}

	weaponsWithImages := []string{}
	weaponsWithoutImages := []string{}

	for weapon, image := range weaponImages {
		if image != "" {
			weaponsWithImages = append(weaponsWithImages, fmt.Sprintf("✅ %s -> %s", weapon, image))
		} else {
			weaponsWithoutImages = append(weaponsWithoutImages, fmt.Sprintf("❌ %s (ไม่มีรูปภาพ)", weapon))
		}
	}

	if len(weaponsWithImages) > 0 {
		text := strings.Join(weaponsWithImages[:min(25, len(weaponsWithImages))], "\n")
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "อาวุธที่มีรูปภาพ",
			Value:  text,
			Inline: false,
		})
		if len(weaponsWithImages) > 25 {
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
				Name:   "และอีก...",
				Value:  fmt.Sprintf("%d รายการ", len(weaponsWithImages)-25),
				Inline: false,
			})
		}
	}

	if len(weaponsWithoutImages) > 0 {
		text := strings.Join(weaponsWithoutImages[:min(25, len(weaponsWithoutImages))], "\n")
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "อาวุธที่ยังไม่มีรูปภาพ",
			Value:  text,
			Inline: false,
		})
		if len(weaponsWithoutImages) > 25 {
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
				Name:   "และอีก...",
				Value:  fmt.Sprintf("%d รายการ", len(weaponsWithoutImages)-25),
				Inline: false,
			})
		}
	}

	s.ChannelMessageSendEmbed(m.ChannelID, embed)
}

func setWeaponCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Parse command with support for quoted weapon names
	// Format: !setweapon "weapon name" image.png  OR  !setweapon weaponname image.png
	content := strings.TrimPrefix(m.Content, "!setweapon ")
	content = strings.TrimSpace(content)

	var weaponName, imageName string

	if strings.HasPrefix(content, "\"") {
		// Quoted weapon name: find closing quote
		// Example: "Weapon M16A4" M16A4.png
		//          0123456789...
		closingQuotePos := strings.Index(content[1:], "\"") + 1 // +1 to get position in original string
		if closingQuotePos == 0 {                               // Index returned -1, so -1+1=0
			s.ChannelMessageSend(m.ChannelID, "❌ รูปแบบไม่ถูกต้อง กรุณาปิด quote ให้ครบ\nตัวอย่าง: `!setweapon \"M1 Garand\" m1_garand.png`")
			return
		}
		// weaponName: between quotes (position 1 to closingQuotePos)
		// imageName: after closing quote and space
		weaponName = content[1:closingQuotePos]
		imageName = strings.TrimSpace(content[closingQuotePos+1:])
	} else {
		// No quotes: split by space (original behavior)
		parts := strings.SplitN(content, " ", 2)
		if len(parts) < 2 {
			s.ChannelMessageSend(m.ChannelID, "โปรดระบุชื่ออาวุธและชื่อไฟล์รูปภาพ\nตัวอย่าง: `!setweapon AK-47 ak47.png`\nหรือใช้ quote สำหรับชื่อที่มีเว้นวรรค: `!setweapon \"M1 Garand\" m1_garand.png`")
			return
		}
		weaponName = parts[0]
		imageName = parts[1]
	}

	// Validate inputs
	if weaponName == "" || imageName == "" {
		s.ChannelMessageSend(m.ChannelID, "❌ กรุณาระบุทั้งชื่ออาวุธและชื่อไฟล์รูปภาพ\nตัวอย่าง: `!setweapon \"M1 Garand\" m1_garand.png`")
		return
	}

	imagePath := filepath.Join(Config.WeaponsFolder, imageName)
	if _, err := os.Stat(imagePath); err != nil {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("ไม่พบไฟล์รูปภาพ `%s` ในโฟลเดอร์ `%s`", imageName, Config.WeaponsFolder))
		return
	}

	weaponImagesMux.Lock()
	weaponImages[weaponName] = imageName
	weaponImagesMux.Unlock()

	saveWeaponImages()

	file, err := os.Open(imagePath)
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, "ตั้งค่าสำเร็จแต่ไม่สามารถแสดงรูปภาพได้")
		return
	}
	defer file.Close()

	embed := &discordgo.MessageEmbed{
		Title:       "ตั้งค่ารูปภาพอาวุธสำเร็จ",
		Color:       0x00FF00,
		Description: fmt.Sprintf("อาวุธ `%s` ใช้รูปภาพ `%s`", weaponName, imageName),
		Thumbnail: &discordgo.MessageEmbedThumbnail{
			URL: fmt.Sprintf("attachment://%s", imageName),
		},
	}

	s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Embed: embed,
		Files: []*discordgo.File{
			{
				Name:   imageName,
				Reader: file,
			},
		},
	})
}

func testRemoteCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	s.ChannelMessageSend(m.ChannelID, "กำลังทดสอบการเชื่อมต่อ Remote...")

	if !Config.UseRemote {
		s.ChannelMessageSend(m.ChannelID, "❌ ไม่ได้กำหนดค่าการเชื่อมต่อ Remote")
		return
	}

	// Test connection
	err := TestRemoteConnection()
	if err != nil {
		embed := &discordgo.MessageEmbed{
			Title:       "❌ การทดสอบ Remote ล้มเหลว",
			Color:       0xFF0000,
			Description: fmt.Sprintf("ไม่สามารถเชื่อมต่อกับเซิร์ฟเวอร์ %s ได้", Config.ConnectionType),
			Fields: []*discordgo.MessageEmbedField{
				{
					Name:   "รายละเอียดข้อผิดพลาด",
					Value:  fmt.Sprintf("```%s```", err.Error()),
					Inline: false,
				},
				{
					Name:   "คำแนะนำ",
					Value:  "โปรดตรวจสอบการตั้งค่าใน .env file และยืนยันว่าเซิร์ฟเวอร์กำลังทำงานและเข้าถึงได้",
					Inline: false,
				},
			},
		}
		s.ChannelMessageSendEmbed(m.ChannelID, embed)
		return
	}

	// List files
	files, err := ListRemoteFiles()
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("❌ ไม่สามารถอ่านไดเรกทอรี: %v", err))
		return
	}

	fileNames := []string{}
	var totalSize int64
	killFileCount := 0

	for i, file := range files {
		if !file.IsDir {
			if isValidKillFile(file.Name) {
				killFileCount++
			}
			if i < 5 {
				fileNames = append(fileNames, fmt.Sprintf("%s (%.2f MB)", file.Name, float64(file.Size)/1024/1024))
			}
			totalSize += file.Size
		}
	}

	// Get cache stats
	cacheStats := SentLogsCache.Stats()

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("✅ การทดสอบ %s สำเร็จ!", Config.ConnectionType),
		Color:       0x00FF00,
		Description: fmt.Sprintf("สามารถเชื่อมต่อกับเซิร์ฟเวอร์ %s ได้", Config.ConnectionType),
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "รายละเอียดการเชื่อมต่อ",
				Value:  fmt.Sprintf("Protocol: %s\nHost: %s\nUser: %s\nPort: %s", Config.ConnectionType, Config.FTPHost, Config.FTPUser, Config.FTPPort),
				Inline: false,
			},
			{
				Name: "สถิติไฟล์",
				Value: fmt.Sprintf("พบไฟล์ทั้งหมด %d ไฟล์\nไฟล์ kill_ ที่ถูกต้อง: %d ไฟล์\nขนาดรวม: %.2f MB",
					len(files), killFileCount, float64(totalSize)/1024/1024),
				Inline: false,
			},
			{
				Name:   "ตัวอย่างไฟล์",
				Value:  strings.Join(fileNames, "\n"),
				Inline: false,
			},
			{
				Name:   "Connection Pool Status",
				Value:  "✅ Using connection pool for better performance",
				Inline: false,
			},
			{
				Name: "Cache Performance",
				Value: fmt.Sprintf("Hit Rate: %.2f%%\nCached Entries: %d/%d",
					cacheStats.HitRate, cacheStats.Size, cacheStats.Capacity),
				Inline: false,
			},
		},
	}

	s.ChannelMessageSendEmbed(m.ChannelID, embed)
}
