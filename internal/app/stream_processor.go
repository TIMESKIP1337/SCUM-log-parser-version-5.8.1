package app

import (
	"bufio"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// StreamingFileProcessor handles large file processing with progress tracking
type StreamingFileProcessor struct {
	bufferSize   int
	maxBatchSize int
	rateLimiter  chan struct{}
	progressChan chan float64
}

// NewStreamingFileProcessor creates a new streaming file processor
func NewStreamingFileProcessor(bufferSize, maxBatchSize, rateLimit int) *StreamingFileProcessor {
	return &StreamingFileProcessor{
		bufferSize:   bufferSize,
		maxBatchSize: maxBatchSize,
		rateLimiter:  make(chan struct{}, rateLimit),
		progressChan: make(chan float64, 100),
	}
}

// ProcessLargeFile processes a large file in streaming mode
func (p *StreamingFileProcessor) ProcessLargeFile(filePath string, processFunc func([]string)) error {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return err
	}

	totalSize := fileInfo.Size()
	if totalSize > 100*1024*1024 { // If file > 100MB
		log.Printf("⚠️ Large file detected (%.2f MB), using streaming mode", float64(totalSize)/1024/1024)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	// Get buffer from pool
	buffer := GlobalBufferPool.Get()
	defer GlobalBufferPool.Put(buffer)

	// Try UTF-16 LE first
	decoder := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
	reader := transform.NewReader(file, decoder)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(buffer, len(buffer))

	// Get batch from pool
	batch := GetBatch()
	defer PutBatch(batch)

	var processedBytes int64
	lastProgressReport := time.Now()
	lineCount := 0

	// Process lines
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			*batch = append(*batch, line)
			processedBytes += int64(len(line)) + 1 // +1 for newline
			lineCount++

			// Report progress
			if time.Since(lastProgressReport) > time.Second {
				progress := float64(processedBytes) / float64(totalSize) * 100
				select {
				case p.progressChan <- progress:
				default:
				}
				lastProgressReport = time.Now()
				log.Printf("📊 Streaming: %.1f%% complete (%d lines processed)", progress, lineCount)
				UpdateSharedActivity()
			}

			// Process batch when full
			if len(*batch) >= p.maxBatchSize {
				p.processBatchWithRateLimit(*batch, processFunc)
				*batch = (*batch)[:0] // Clear batch
			}
		}
	}

	// Check for UTF-16 error and retry with UTF-8
	if err := scanner.Err(); err != nil {
		log.Printf("⚠️ UTF-16 decoding failed, retrying with UTF-8")

		// Reset file position
		file.Seek(0, 0)
		scanner = bufio.NewScanner(file)
		scanner.Buffer(buffer, len(buffer))

		// Clear batch and counters
		*batch = (*batch)[:0]
		processedBytes = 0
		lineCount = 0

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				*batch = append(*batch, line)
				processedBytes += int64(len(line)) + 1
				lineCount++

				// Report progress
				if time.Since(lastProgressReport) > time.Second {
					progress := float64(processedBytes) / float64(totalSize) * 100
					select {
					case p.progressChan <- progress:
					default:
					}
					lastProgressReport = time.Now()
					log.Printf("📊 Streaming: %.1f%% complete (%d lines processed)", progress, lineCount)
					UpdateSharedActivity()
				}

				// Process batch when full
				if len(*batch) >= p.maxBatchSize {
					p.processBatchWithRateLimit(*batch, processFunc)
					*batch = (*batch)[:0]
				}
			}
		}

		if err := scanner.Err(); err != nil {
			return err
		}
	}

	// Process remaining items
	if len(*batch) > 0 {
		p.processBatchWithRateLimit(*batch, processFunc)
	}

	log.Printf("✅ Streaming completed: %d lines processed from %.2f MB file",
		lineCount, float64(totalSize)/1024/1024)

	return nil
}

// processBatchWithRateLimit processes a batch with rate limiting
func (p *StreamingFileProcessor) processBatchWithRateLimit(lines []string, processFunc func([]string)) {
	if len(lines) == 0 {
		return
	}

	// Rate limiting
	p.rateLimiter <- struct{}{}
	defer func() { <-p.rateLimiter }()

	// Check memory before processing
	if !GlobalResourceManager.CheckMemory() {
		log.Printf("⚠️ Skipping batch due to high memory usage")
		return
	}

	// Process with resource management
	GlobalResourceManager.AcquireGoroutine()
	defer GlobalResourceManager.ReleaseGoroutine()

	processFunc(lines)
}

// StreamReadLines reads lines from a file with streaming and encoding detection
func StreamReadLines(filePath string, callback func(string) error) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	// Get file size for progress tracking
	fileInfo, err := file.Stat()
	if err != nil {
		return err
	}
	totalSize := fileInfo.Size()

	// Get buffer from pool
	buffer := GlobalBufferPool.Get()
	defer GlobalBufferPool.Put(buffer)

	// Try UTF-16 LE first
	decoder := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
	reader := transform.NewReader(file, decoder)

	return streamWithProgress(reader, buffer, totalSize, callback)
}

// streamWithProgress reads from reader with progress tracking
func streamWithProgress(reader io.Reader, buffer []byte, totalSize int64, callback func(string) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(buffer, len(buffer))

	var processedBytes int64
	lastProgressReport := time.Now()
	lineCount := 0

	for scanner.Scan() {
		line := scanner.Text()
		processedBytes += int64(len(line)) + 1
		lineCount++

		// Report progress periodically
		if time.Since(lastProgressReport) > 2*time.Second {
			progress := float64(processedBytes) / float64(totalSize) * 100
			log.Printf("📊 Reading file: %.1f%% complete (%d lines)", progress, lineCount)
			lastProgressReport = time.Now()
			UpdateSharedActivity()
		}

		if err := callback(line); err != nil {
			return err
		}
	}

	return scanner.Err()
}

// BatchProcessor processes items in configurable batches
type BatchProcessor struct {
	batchSize     int
	flushInterval time.Duration
	processFunc   func([]string) error
	items         []string
	lastFlush     time.Time
}

// NewBatchProcessor creates a new batch processor
func NewBatchProcessor(batchSize int, flushInterval time.Duration, processFunc func([]string) error) *BatchProcessor {
	return &BatchProcessor{
		batchSize:     batchSize,
		flushInterval: flushInterval,
		processFunc:   processFunc,
		items:         make([]string, 0, batchSize),
		lastFlush:     time.Now(),
	}
}

// Add adds an item to the batch
func (bp *BatchProcessor) Add(item string) error {
	bp.items = append(bp.items, item)

	// Process if batch is full or interval exceeded
	if len(bp.items) >= bp.batchSize || time.Since(bp.lastFlush) > bp.flushInterval {
		return bp.Flush()
	}

	return nil
}

// Flush processes all pending items
func (bp *BatchProcessor) Flush() error {
	if len(bp.items) == 0 {
		return nil
	}

	// Process items
	if err := bp.processFunc(bp.items); err != nil {
		return err
	}

	// Clear items
	bp.items = bp.items[:0]
	bp.lastFlush = time.Now()

	return nil
}

// ProgressTracker tracks and reports progress
type ProgressTracker struct {
	total          int64
	current        int64
	lastReport     time.Time
	reportInterval time.Duration
}

// NewProgressTracker creates a new progress tracker
func NewProgressTracker(total int64, reportInterval time.Duration) *ProgressTracker {
	return &ProgressTracker{
		total:          total,
		reportInterval: reportInterval,
		lastReport:     time.Now(),
	}
}

// Update updates progress and reports if needed
func (pt *ProgressTracker) Update(delta int64) {
	pt.current += delta

	if time.Since(pt.lastReport) > pt.reportInterval {
		percentage := float64(pt.current) / float64(pt.total) * 100
		log.Printf("📊 Progress: %.1f%% (%d/%d)", percentage, pt.current, pt.total)
		pt.lastReport = time.Now()
		UpdateSharedActivity()
	}
}

// GetProgress returns current progress percentage
func (pt *ProgressTracker) GetProgress() float64 {
	if pt.total == 0 {
		return 0
	}
	return float64(pt.current) / float64(pt.total) * 100
}
