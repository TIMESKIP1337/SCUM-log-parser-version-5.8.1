package app

import (
	"context"
	"log"
	"runtime"
	"runtime/debug"
	"sync"
	"time"
)

// MemoryManager จัดการการใช้ memory และป้องกัน leaks
type MemoryManager struct {
	maxMemoryMB       uint64
	warningThreshold  float64
	criticalThreshold float64
	checkInterval     time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Metrics
	mu              sync.RWMutex
	lastCheck       time.Time
	currentMemoryMB uint64
	peakMemoryMB    uint64
	gcCount         uint32
	forceGCCount    uint32
}

// NewMemoryManager สร้าง memory manager ใหม่
func NewMemoryManager(maxMemoryMB uint64) *MemoryManager {
	ctx, cancel := context.WithCancel(context.Background())

	mm := &MemoryManager{
		maxMemoryMB:       maxMemoryMB,
		warningThreshold:  0.70, // 70% ของ max
		criticalThreshold: 0.85, // 85% ของ max
		checkInterval:     30 * time.Second,
		ctx:               ctx,
		cancel:            cancel,
	}

	// ตั้งค่า GOGC ให้เหมาะสม
	debug.SetGCPercent(50) // ลด GC overhead

	// ตั้งค่า Memory Limit
	debug.SetMemoryLimit(int64(maxMemoryMB * 1024 * 1024))

	return mm
}

// Start เริ่มการ monitor memory
func (mm *MemoryManager) Start() {
	mm.wg.Add(1)
	go mm.monitorLoop()
	log.Printf("✅ Memory Manager started (Max: %d MB, Warning: %.0f%%, Critical: %.0f%%)",
		mm.maxMemoryMB, mm.warningThreshold*100, mm.criticalThreshold*100)
}

// Stop หยุดการ monitor
func (mm *MemoryManager) Stop() {
	mm.cancel()
	mm.wg.Wait()
	log.Println("🛑 Memory Manager stopped")
}

// monitorLoop ตรวจสอบ memory แบบ continuous
func (mm *MemoryManager) monitorLoop() {
	defer mm.wg.Done()

	ticker := time.NewTicker(mm.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-mm.ctx.Done():
			return
		case <-ticker.C:
			mm.checkAndAct()
		}
	}
}

// checkAndAct ตรวจสอบและดำเนินการตาม memory usage
func (mm *MemoryManager) checkAndAct() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	currentMB := m.Alloc / 1024 / 1024
	heapMB := m.HeapAlloc / 1024 / 1024
	sysMB := m.Sys / 1024 / 1024

	mm.mu.Lock()
	mm.currentMemoryMB = currentMB
	if currentMB > mm.peakMemoryMB {
		mm.peakMemoryMB = currentMB
	}
	mm.lastCheck = time.Now()
	mm.gcCount = m.NumGC
	mm.mu.Unlock()

	usage := float64(currentMB) / float64(mm.maxMemoryMB)

	// Log สถานะ memory
	log.Printf("📊 Memory: Alloc=%dMB, Heap=%dMB, Sys=%dMB, Usage=%.1f%%, GC=%d",
		currentMB, heapMB, sysMB, usage*100, m.NumGC)

	// ดำเนินการตามระดับ memory usage
	if usage >= mm.criticalThreshold {
		mm.handleCriticalMemory()
	} else if usage >= mm.warningThreshold {
		mm.handleWarningMemory()
	}
}

// handleWarningMemory จัดการเมื่อ memory ใกล้เต็ม
func (mm *MemoryManager) handleWarningMemory() {
	log.Printf("⚠️ Memory usage at warning level, running soft cleanup...")

	// 1. Force GC
	mm.forceGC()

	// 2. ลด cache size
	mm.cleanupCaches(false)

	// 3. Flush pending batches
	mm.flushBatches()
}

// handleCriticalMemory จัดการเมื่อ memory เกือบเต็ม
func (mm *MemoryManager) handleCriticalMemory() {
	log.Printf("🔴 CRITICAL: Memory usage very high! Running aggressive cleanup...")

	// 1. Force GC หลายครั้ง
	for i := 0; i < 3; i++ {
		mm.forceGC()
		time.Sleep(100 * time.Millisecond)
	}

	// 2. ลด cache แบบ aggressive
	mm.cleanupCaches(true)

	// 3. Flush ทุกอย่างทันที
	mm.flushBatches()

	// 4. ปิด idle connections
	mm.closeIdleConnections()

	// 5. Clear buffer pools
	mm.clearBufferPools()
}

// forceGC บังคับให้ทำ Garbage Collection
func (mm *MemoryManager) forceGC() {
	before := mm.currentMemoryMB

	runtime.GC()
	debug.FreeOSMemory()

	// อ่านค่าใหม่
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	after := m.Alloc / 1024 / 1024

	mm.mu.Lock()
	mm.forceGCCount++
	mm.mu.Unlock()

	freed := int64(before) - int64(after)
	if freed > 0 {
		log.Printf("🗑️ Force GC completed: Freed %d MB (Before: %d MB, After: %d MB)",
			freed, before, after)
	}
}

// cleanupCaches ลดขนาด cache
func (mm *MemoryManager) cleanupCaches(aggressive bool) {
	if SentLogsCache != nil {
		oldSize := SentLogsCache.Size()

		if aggressive {
			// ลบรายการเก่ากว่า 3 วัน
			removed := SentLogsCache.CleanOldEntries(3 * 24 * time.Hour)
			log.Printf("🧹 Aggressive cache cleanup: Removed %d old entries", removed)

			// ถ้ายังใหญ่เกินไป ลดเหลือ 50%
			if SentLogsCache.Size() > SentLogsCache.capacity/2 {
				target := SentLogsCache.capacity / 2
				toRemove := SentLogsCache.Size() - target
				for i := 0; i < toRemove; i++ {
					SentLogsCache.mu.Lock()
					SentLogsCache.evictOldest()
					SentLogsCache.mu.Unlock()
				}
				log.Printf("🧹 Reduced cache to 50%% capacity")
			}
		} else {
			// ลบรายการเก่ากว่า 7 วัน
			removed := SentLogsCache.CleanOldEntries(7 * 24 * time.Hour)
			if removed > 0 {
				log.Printf("🧹 Soft cache cleanup: Removed %d old entries", removed)
			}
		}

		newSize := SentLogsCache.Size()
		if oldSize > newSize {
			log.Printf("📉 Cache size reduced: %d → %d (%.1f%%)",
				oldSize, newSize, float64(newSize)/float64(oldSize)*100)
		}
	}
}

// flushBatches flush ข้อความที่รออยู่
func (mm *MemoryManager) flushBatches() {
	if GlobalBatchSender != nil {
		log.Printf("💾 Flushing message batches...")
		GlobalBatchSender.FlushAll()
	}

	if GlobalEmbedBatchSender != nil {
		log.Printf("💾 Flushing embed batches...")
		GlobalEmbedBatchSender.FlushAll()
	}
}

// closeIdleConnections ปิด connection ที่ไม่ได้ใช้งาน
func (mm *MemoryManager) closeIdleConnections() {
	log.Printf("🔌 Closing idle connections...")

	// ปิด FTP connection ที่ idle
	if globalFTPPool != nil {
		globalFTPPool.mu.Lock()
		if globalFTPPool.conn != nil {
			log.Printf("🔌 Closing idle FTP connection")
			globalFTPPool.closeConnection()
		}
		globalFTPPool.mu.Unlock()
	}

	// ปิด SFTP connection ที่ idle
	if globalSFTPPool != nil {
		globalSFTPPool.mu.Lock()
		if globalSFTPPool.conn != nil {
			log.Printf("🔌 Closing idle SFTP connection")
			globalSFTPPool.closeConnection()
		}
		globalSFTPPool.mu.Unlock()
	}
}

// clearBufferPools ล้าง buffer pools
func (mm *MemoryManager) clearBufferPools() {
	if GlobalBufferPool != nil {
		// ไม่ต้อง clear pool เพราะจะถูก GC เก็บเอง
		log.Printf("🧹 Buffer pools will be cleaned by GC")
	}
}

// GetStats ดึงสถิติ memory
func (mm *MemoryManager) GetStats() MemoryStats {
	mm.mu.RLock()
	defer mm.mu.RUnlock()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return MemoryStats{
		AllocMB:      m.Alloc / 1024 / 1024,
		TotalAllocMB: m.TotalAlloc / 1024 / 1024,
		SysMB:        m.Sys / 1024 / 1024,
		HeapMB:       m.HeapAlloc / 1024 / 1024,
		HeapSysMB:    m.HeapSys / 1024 / 1024,
		HeapIdleMB:   m.HeapIdle / 1024 / 1024,
		HeapInuseMB:  m.HeapInuse / 1024 / 1024,
		StackMB:      m.StackInuse / 1024 / 1024,
		NumGC:        m.NumGC,
		NumGoroutine: runtime.NumGoroutine(),

		CurrentMB:    mm.currentMemoryMB,
		PeakMB:       mm.peakMemoryMB,
		MaxMB:        mm.maxMemoryMB,
		UsagePercent: float64(mm.currentMemoryMB) / float64(mm.maxMemoryMB) * 100,
		ForceGCCount: mm.forceGCCount,
		LastCheck:    mm.lastCheck,
	}
}

// MemoryStats สถิติ memory
type MemoryStats struct {
	AllocMB      uint64
	TotalAllocMB uint64
	SysMB        uint64
	HeapMB       uint64
	HeapSysMB    uint64
	HeapIdleMB   uint64
	HeapInuseMB  uint64
	StackMB      uint64
	NumGC        uint32
	NumGoroutine int

	CurrentMB    uint64
	PeakMB       uint64
	MaxMB        uint64
	UsagePercent float64
	ForceGCCount uint32
	LastCheck    time.Time
}

// IsMemoryHealthy ตรวจสอบว่า memory ยังดีอยู่ไหม
func (mm *MemoryManager) IsMemoryHealthy() bool {
	mm.mu.RLock()
	defer mm.mu.RUnlock()

	usage := float64(mm.currentMemoryMB) / float64(mm.maxMemoryMB)
	return usage < mm.warningThreshold
}

// GetMemoryUsagePercent ดึง % การใช้ memory
func (mm *MemoryManager) GetMemoryUsagePercent() float64 {
	mm.mu.RLock()
	defer mm.mu.RUnlock()

	return float64(mm.currentMemoryMB) / float64(mm.maxMemoryMB) * 100
}

// Global Memory Manager
var GlobalMemoryManager *MemoryManager

// InitializeMemoryManager เริ่ม memory manager
func InitializeMemoryManager(maxMemoryMB uint64) {
	if maxMemoryMB == 0 {
		maxMemoryMB = 2048 // Default 2GB
	}

	GlobalMemoryManager = NewMemoryManager(maxMemoryMB)
	GlobalMemoryManager.Start()

	log.Printf("🚀 Global Memory Manager initialized with max %d MB", maxMemoryMB)
}

// StopMemoryManager หยุด memory manager
func StopMemoryManager() {
	if GlobalMemoryManager != nil {
		GlobalMemoryManager.Stop()
	}
}
