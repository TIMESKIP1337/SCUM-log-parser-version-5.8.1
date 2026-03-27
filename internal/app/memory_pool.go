package app

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ObjectPool manages reusable objects to reduce GC pressure
var (
	// Pool for log entry strings
	stringPool = sync.Pool{
		New: func() interface{} {
			s := make([]byte, 0, 512)
			return &s
		},
	}

	// Pool for KillInfo objects
	killInfoPool = sync.Pool{
		New: func() interface{} {
			return &KillInfo{}
		},
	}

	// Pool for message embeds
	embedPool = sync.Pool{
		New: func() interface{} {
			return &discordgo.MessageEmbed{
				Fields: make([]*discordgo.MessageEmbedField, 0, 4),
			}
		},
	}

	// Pool for batch slices
	batchPool = sync.Pool{
		New: func() interface{} {
			batch := make([]string, 0, 100)
			return &batch
		},
	}
)

// GetStringBuffer gets a reusable string buffer
func GetStringBuffer() *[]byte {
	return stringPool.Get().(*[]byte)
}

// PutStringBuffer returns a string buffer to the pool
func PutStringBuffer(buf *[]byte) {
	if buf != nil && cap(*buf) <= 4096 { // Don't pool huge buffers
		*buf = (*buf)[:0]
		stringPool.Put(buf)
	}
}

// GetKillInfo gets a reusable KillInfo object
func GetKillInfo() *KillInfo {
	return killInfoPool.Get().(*KillInfo)
}

// PutKillInfo returns a KillInfo object to the pool
func PutKillInfo(info *KillInfo) {
	if info != nil {
		// Clear the object
		info.Date = ""
		info.Killer = ""
		info.KillerSteamID = ""
		info.Died = ""
		info.DiedSteamID = ""
		info.Weapon = ""
		info.Distance = ""
		info.IsNPC = false
		killInfoPool.Put(info)
	}
}

// GetBatch gets a reusable batch slice
func GetBatch() *[]string {
	return batchPool.Get().(*[]string)
}

// PutBatch returns a batch slice to the pool
func PutBatch(batch *[]string) {
	if batch != nil && cap(*batch) <= 1000 {
		*batch = (*batch)[:0]
		batchPool.Put(batch)
	}
}

// cleanNPCName cleans NPC names to make them more readable
// Example: BP_Drifter_Lvl_4_C_2147257905 -> Drifter Lvl 4
func cleanNPCName(name string) string {
	if name == "" {
		return name
	}

	// Remove BP_ prefix
	name = strings.TrimPrefix(name, "BP_")

	// Remove _C suffix and any trailing numbers/underscores
	// Handles: _C_2147257905, _C2147257905, _C
	name = regexp.MustCompile(`_C.*$`).ReplaceAllString(name, "")

	// Replace underscores with spaces
	name = strings.ReplaceAll(name, "_", " ")

	// Clean up extra spaces
	name = strings.TrimSpace(name)

	return name
}

// OptimizedKillLogParser parses kill logs with object pooling
func OptimizedParseKillLog(line string) *KillInfo {
	if line == "" {
		return nil
	}

	// Updated pattern to support both player kills and NPC kills
	// Now captures Steam IDs/NPC identifier in parentheses
	pattern := `(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): Died: ([^\(]+)\s*\(([^\)]+)\),\s*Killer: ([^\(]+)\s*\(([^\)]+)\)\s*Weapon: ([^\[]+\[[^\]]+\])\s*.*Distance: ([\d.]+ m)`
	re := regexp.MustCompile(pattern)
	matches := re.FindStringSubmatch(line)

	if len(matches) != 8 {
		return nil
	}

	// Get pooled object
	info := GetKillInfo()

	// Populate fields
	info.Date = matches[1]
	info.Died = strings.TrimSpace(matches[2])
	info.DiedSteamID = strings.TrimSpace(matches[3]) // Steam ID of victim

	// Check if killer is NPC by looking for (NPC) in the line
	info.IsNPC = strings.Contains(line, "(NPC)")

	// Extract killer's Steam ID (if not NPC)
	killerID := strings.TrimSpace(matches[5])
	if killerID != "NPC" {
		info.KillerSteamID = killerID
	}

	// Clean killer name (handle NPC names)
	killerRaw := strings.TrimSpace(matches[4])
	info.Killer = cleanNPCName(killerRaw)

	// Clean weapon name
	weapon := strings.TrimSpace(matches[6])
	rawWeapon := weapon // Keep raw weapon for distance calculation
	weapon = regexp.MustCompile(`_C\d*`).ReplaceAllString(weapon, "")
	weapon = strings.ReplaceAll(weapon, " [Projectile]", "")
	weapon = regexp.MustCompile(`_C_\d+\s*\[Melee\]`).ReplaceAllString(weapon, "")
	weapon = regexp.MustCompile(`\d+\s*\[Melee\]`).ReplaceAllString(weapon, "")
	weapon = regexp.MustCompile(`_\d+\s+\[Explosion\]`).ReplaceAllString(weapon, "")
	weapon = strings.ReplaceAll(weapon, "_", "")

	if weapon == "Fists/Legs [Melee]" {
		weapon = "Muaythai"
	}
	weapon = strings.TrimPrefix(weapon, "Weapon_")
	info.Weapon = strings.TrimSpace(weapon)

	// Process distance - remove decimals and check for mines/explosions/melee
	distanceStr := strings.TrimSpace(matches[7])

	// Check if weapon is a mine or explosion-based (should be 0 m)
	isMineOrExplosion := strings.Contains(rawWeapon, "Mine") ||
		strings.Contains(rawWeapon, "[Explosion]") ||
		strings.Contains(strings.ToLower(rawWeapon), "mine")

	// Check if weapon is Melee (close range combat)
	isMelee := strings.Contains(rawWeapon, "[Melee]")

	if isMineOrExplosion {
		info.Distance = "0 m"
	} else if isMelee {
		// For melee, if distance < 1m, show as 0 m
		if strings.HasSuffix(distanceStr, " m") {
			numStr := strings.TrimSuffix(distanceStr, " m")
			if dist, err := strconv.ParseFloat(numStr, 64); err == nil {
				if dist < 1.0 {
					info.Distance = "0 m"
				} else {
					info.Distance = fmt.Sprintf("%d m", int(dist))
				}
			} else {
				info.Distance = distanceStr
			}
		} else {
			info.Distance = distanceStr
		}
	} else {
		// Parse distance and remove decimals (e.g., 100.55 m -> 100 m)
		if strings.HasSuffix(distanceStr, " m") {
			numStr := strings.TrimSuffix(distanceStr, " m")
			if dist, err := strconv.ParseFloat(numStr, 64); err == nil {
				info.Distance = fmt.Sprintf("%d m", int(dist))
			} else {
				info.Distance = distanceStr
			}
		} else {
			info.Distance = distanceStr
		}
	}

	return info
}

// BufferPool manages reusable buffers for file reading
type BufferPool struct {
	pool sync.Pool
	size int
}

// NewBufferPool creates a new buffer pool
func NewBufferPool(bufferSize int) *BufferPool {
	return &BufferPool{
		size: bufferSize,
		pool: sync.Pool{
			New: func() interface{} {
				return make([]byte, bufferSize)
			},
		},
	}
}

// Get gets a buffer from the pool
func (bp *BufferPool) Get() []byte {
	return bp.pool.Get().([]byte)
}

// Put returns a buffer to the pool
func (bp *BufferPool) Put(buf []byte) {
	if len(buf) == bp.size {
		bp.pool.Put(buf)
	}
}

// Global buffer pool
var GlobalBufferPool = NewBufferPool(32 * 1024) // 32KB buffers

// MemoryEfficientFileReader reads files with pooled buffers
type MemoryEfficientFileReader struct {
	file       *os.File
	buffer     []byte
	bufferPool *BufferPool
}

// NewMemoryEfficientFileReader creates a new memory efficient reader
func NewMemoryEfficientFileReader(filePath string) (*MemoryEfficientFileReader, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	return &MemoryEfficientFileReader{
		file:       file,
		bufferPool: GlobalBufferPool,
	}, nil
}

// ReadLines reads lines efficiently
func (r *MemoryEfficientFileReader) ReadLines(callback func(string) error) error {
	defer r.Close()

	// Get buffer from pool
	buffer := r.bufferPool.Get()
	defer r.bufferPool.Put(buffer)

	scanner := bufio.NewScanner(r.file)
	scanner.Buffer(buffer, len(buffer))

	for scanner.Scan() {
		if err := callback(scanner.Text()); err != nil {
			return err
		}
	}

	return scanner.Err()
}

// Close closes the reader
func (r *MemoryEfficientFileReader) Close() error {
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

// ResourceManager manages system resources
type ResourceManager struct {
	maxMemoryMB   uint64
	maxGoroutines int
	semaphore     chan struct{}
}

// NewResourceManager creates a resource manager
func NewResourceManager(maxMemoryMB uint64, maxGoroutines int) *ResourceManager {
	return &ResourceManager{
		maxMemoryMB:   maxMemoryMB,
		maxGoroutines: maxGoroutines,
		semaphore:     make(chan struct{}, maxGoroutines),
	}
}

// AcquireGoroutine acquires a goroutine slot
func (rm *ResourceManager) AcquireGoroutine() {
	rm.semaphore <- struct{}{}
}

// ReleaseGoroutine releases a goroutine slot
func (rm *ResourceManager) ReleaseGoroutine() {
	<-rm.semaphore
}

// CheckMemory checks if memory usage is within limits
func (rm *ResourceManager) CheckMemory() bool {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	currentMB := m.Alloc / 1024 / 1024
	if currentMB > rm.maxMemoryMB {
		log.Printf("⚠️ Memory limit exceeded: %d MB > %d MB", currentMB, rm.maxMemoryMB)
		runtime.GC()
		return false
	}

	return true
}

// Global resource manager
var GlobalResourceManager = NewResourceManager(500, runtime.NumCPU()*2)

// ==========================================
// ✨ แก้ไขฟังก์ชันหลัก OptimizedProcessKillLines
// ==========================================
func OptimizedProcessKillLines(lines []string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [PROCESS KILL LINES] Panic recovered: %v", r)
		}
	}()

	// Check memory before processing
	if !GlobalResourceManager.CheckMemory() {
		log.Printf("⚠️ Skipping batch due to high memory usage")
		return
	}

	newLogs := false
	processedCount := 0
	totalLines := len(lines)

	// 🚀 ปรับ maxProcessPerBatch จาก env variable หรือใช้ค่าเริ่มต้น
	maxProcessPerBatch := getEnvInt("MAX_PROCESS_PER_BATCH", 2000) // เพิ่มจาก 500 เป็น 2000

	// ✨ Adaptive Batching - ปรับขนาด batch ตามจำนวนข้อมูล
	adaptiveBatching := getEnvBool("ENABLE_ADAPTIVE_BATCHING", true)

	var batchSize int
	if adaptiveBatching {
		if totalLines <= 100 {
			batchSize = totalLines // ประมวลผลทั้งหมด
		} else if totalLines <= 1000 {
			batchSize = getEnvInt("BATCH_SIZE", 100) // ใช้ค่าจาก config
		} else {
			// สำหรับข้อมูลจำนวนมาก ใช้ batch เล็ก
			batchSize = 50
			log.Printf("📊 Large dataset detected (%d items), using small batches for stability", totalLines)
		}
	} else {
		batchSize = getEnvInt("BATCH_SIZE", 100)
	}

	log.Printf("📊 Processing %d kill feed items (max: %d per cycle, batch size: %d)",
		totalLines, maxProcessPerBatch, batchSize)

	// Get batch from pool
	batch := GetBatch()
	defer PutBatch(batch)

	// ประมวลผลไม่เกิน maxProcessPerBatch รายการต่อรอบ
	processLimit := totalLines
	if processLimit > maxProcessPerBatch {
		processLimit = maxProcessPerBatch
		log.Printf("⚡ Processing limited to %d items this cycle, %d remaining for next cycle",
			maxProcessPerBatch, totalLines-maxProcessPerBatch)
	}

	// ประมวลผลเป็น batch
	for i := 0; i < processLimit; i += batchSize {
		// ตรวจสอบหน่วยความจำก่อนประมวลผลแต่ละ batch
		if !GlobalResourceManager.CheckMemory() {
			log.Printf("⚠️ Memory limit reached, stopping at %d/%d items", i, processLimit)
			break
		}

		end := i + batchSize
		if end > processLimit {
			end = processLimit
		}

		// ประมวลผล batch ปัจจุบัน
		batchProcessed := 0
		for j := i; j < end; j++ {
			normalizedLine := strings.TrimSpace(lines[j])
			logKey := normalizedLine + "_killfeed"

			if !IsLogSentCached(logKey) {
				// Use pooled parser
				killInfo := OptimizedParseKillLog(normalizedLine)
				if killInfo != nil {
					// Process with goroutine limit
					GlobalResourceManager.AcquireGoroutine()
					go func(info *KillInfo) {
						defer GlobalResourceManager.ReleaseGoroutine()
						defer PutKillInfo(info) // Return to pool

						if err := sendKillLog(info); err != nil {
							log.Printf("Error sending kill log: %v", err)
						}
					}(killInfo)

					MarkLogAsSentCached(logKey)
					newLogs = true
					processedCount++
					batchProcessed++
				}
			}
		}

		// รายงานความคืบหน้าทุก 500 รายการ
		if i > 0 && i%500 == 0 {
			progress := float64(i) / float64(processLimit) * 100
			log.Printf("📊 Kill feed progress: %.1f%% (%d/%d processed)", progress, i, processLimit)
		}

		// หน่วงเวลาเล็กน้อยเพื่อไม่ให้ระบบทำงานหนักเกินไป
		if i+batchSize < processLimit && batchProcessed > 0 {
			messageDelay := getEnvInt("MESSAGE_DELAY_MS", 100)
			time.Sleep(time.Duration(messageDelay) * time.Millisecond)
		}
	}

	// รายงานผลสรุป
	if newLogs {
		if processLimit < totalLines {
			log.Printf("✅ Processed %d/%d kill feed logs this cycle (%d remaining)",
				processedCount, totalLines, totalLines-processLimit)
		} else {
			log.Printf("✅ Processed %d/%d kill feed logs successfully", processedCount, totalLines)
		}
	} else if totalLines > 0 {
		log.Printf("ℹ️ No new kill feed logs to process (%d items checked)", totalLines)
	}
}

// ==========================================
// ✨ เพิ่มฟังก์ชันใหม่สำหรับข้อมูลขนาดใหญ่มาก
// ==========================================
func ProcessLargeKillFeedBatch(lines []string) {
	totalLines := len(lines)

	if totalLines <= 1000 {
		// ใช้วิธีปกติ
		OptimizedProcessKillLines(lines)
		return
	}

	log.Printf("📊 Processing very large kill feed batch (%d items) with advanced streaming", totalLines)

	// แบ่งเป็น chunks เล็กๆ
	chunkSize := getEnvInt("LARGE_BATCH_CHUNK_SIZE", 50)
	processedCount := 0
	maxProcess := getEnvInt("MAX_PROCESS_PER_BATCH", 2000)

	// จำกัดการประมวลผลไม่เกิน maxProcess
	processLimit := totalLines
	if processLimit > maxProcess {
		processLimit = maxProcess
	}

	for i := 0; i < processLimit; i += chunkSize {
		// ตรวจสอบทรัพยากรระบบ
		if !GlobalResourceManager.CheckMemory() {
			log.Printf("⚠️ Memory exhausted, processed %d/%d items", processedCount, totalLines)
			break
		}

		end := i + chunkSize
		if end > processLimit {
			end = processLimit
		}

		// ประมวลผล chunk ปัจจุบัน
		chunk := lines[i:end]

		// ใช้ goroutine แยกสำหรับ chunk นี้
		GlobalResourceManager.AcquireGoroutine()
		go func(chunkData []string, chunkIndex int, startIndex int) {
			defer GlobalResourceManager.ReleaseGoroutine()

			processed := processKillFeedChunk(chunkData, chunkIndex)
			processedCount += processed

			// รายงานความคืบหน้า
			if startIndex > 0 && startIndex%500 == 0 {
				progress := float64(startIndex) / float64(processLimit) * 100
				log.Printf("📊 Advanced streaming: %.1f%% complete", progress)
			}
		}(chunk, i/chunkSize, i)

		// หน่วงเวลาเพื่อควบคุม load
		time.Sleep(5 * time.Millisecond)
	}

	log.Printf("✅ Advanced streaming completed for %d/%d items", processLimit, totalLines)
}

func processKillFeedChunk(lines []string, chunkIndex int) int {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [CHUNK %d] Panic recovered: %v", chunkIndex, r)
		}
	}()

	processed := 0
	for _, line := range lines {
		normalizedLine := strings.TrimSpace(line)
		logKey := normalizedLine + "_killfeed"

		if !IsLogSentCached(logKey) {
			killInfo := OptimizedParseKillLog(normalizedLine)
			if killInfo != nil {
				// ส่งข้อมูลทันที
				if err := sendKillLog(killInfo); err != nil {
					log.Printf("Error in chunk %d: %v", chunkIndex, err)
				} else {
					MarkLogAsSentCached(logKey)
					processed++
				}

				// คืนอ็อบเจ็กต์กลับ pool
				PutKillInfo(killInfo)
			}
		}
	}

	return processed
}

// CircularBuffer implements a fixed-size circular buffer
type CircularBuffer struct {
	buffer []string
	size   int
	head   int
	tail   int
	count  int
	mu     sync.RWMutex
}

// NewCircularBuffer creates a new circular buffer
func NewCircularBuffer(size int) *CircularBuffer {
	return &CircularBuffer{
		buffer: make([]string, size),
		size:   size,
	}
}

// Add adds an item to the buffer
func (cb *CircularBuffer) Add(item string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.buffer[cb.tail] = item
	cb.tail = (cb.tail + 1) % cb.size

	if cb.count < cb.size {
		cb.count++
	} else {
		cb.head = (cb.head + 1) % cb.size
	}
}

// GetAll returns all items in the buffer
func (cb *CircularBuffer) GetAll() []string {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	result := make([]string, cb.count)
	for i := 0; i < cb.count; i++ {
		idx := (cb.head + i) % cb.size
		result[i] = cb.buffer[idx]
	}

	return result
}

// RecentLogsBuffer keeps track of recent logs for deduplication
var RecentLogsBuffer = NewCircularBuffer(1000)
