package app

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ==========================================
// 🚀 OPTIMIZED ECONOMY PROCESSING
// ==========================================

// FastEconomyProcessor - ประมวลผล Economy แบบ High-Speed
type FastEconomyProcessor struct {
	session       *discordgo.Session
	channelID     string
	batchSize     int
	flushInterval time.Duration

	// Aggregation maps
	sellersMap map[string]*EconomyLog
	mu         sync.RWMutex

	// Processing queue
	queue    chan string
	stopChan chan struct{}
	wg       sync.WaitGroup
}

// NewFastEconomyProcessor สร้าง fast processor
func NewFastEconomyProcessor(session *discordgo.Session, channelID string) *FastEconomyProcessor {
	fep := &FastEconomyProcessor{
		session:       session,
		channelID:     channelID,
		batchSize:     getEnvInt("ECONOMY_BATCH_SIZE", 50), // ประมวลผลครั้งละ 50 รายการ
		flushInterval: time.Duration(getEnvInt("ECONOMY_FLUSH_INTERVAL", 3)) * time.Second,
		sellersMap:    make(map[string]*EconomyLog),
		queue:         make(chan string, 1000), // Buffer สูงสุด 1000 items
		stopChan:      make(chan struct{}),
	}

	// เริ่ม worker goroutines (หลายตัวเพื่อความเร็ว)
	workerCount := getEnvInt("ECONOMY_WORKERS", 3)
	for i := 0; i < workerCount; i++ {
		fep.wg.Add(1)
		go fep.worker(i)
	}

	// Auto-flush goroutine
	fep.wg.Add(1)
	go fep.autoFlush()

	log.Printf("⚡ Fast Economy Processor initialized (%d workers, batch: %d, flush: %v)",
		workerCount, fep.batchSize, fep.flushInterval)

	return fep
}

// AddLine เพิ่ม log line เข้า queue
func (fep *FastEconomyProcessor) AddLine(line string) {
	select {
	case fep.queue <- line:
		// Added successfully
	default:
		// Queue full, process immediately to avoid blocking
		log.Printf("⚠️ Economy queue full, processing directly")
		go fep.processLine(line)
	}
}

// worker ประมวลผล lines จาก queue
func (fep *FastEconomyProcessor) worker(id int) {
	defer fep.wg.Done()

	for {
		select {
		case <-fep.stopChan:
			return
		case line := <-fep.queue:
			fep.processLine(line)
		}
	}
}

// processLine ประมวลผล single line
func (fep *FastEconomyProcessor) processLine(line string) {
	// Parse ด้วย optimized parser
	info := fastParseEconomyLog(line)
	if info == nil {
		return
	}

	// Aggregate ตาม seller
	fep.mu.Lock()
	sellerKey := fmt.Sprintf("%s_%s", info.Seller, info.SellerSteamID)

	if existing, exists := fep.sellersMap[sellerKey]; exists {
		// รวม items เข้ากับของเดิม
		for itemName, itemDetail := range info.Items {
			if existingItem, itemExists := existing.Items[itemName]; itemExists {
				existingItem.Quantity += itemDetail.Quantity
				existingItem.TotalPrice += itemDetail.TotalPrice
				existingItem.TransactionCount += 1
				existing.Items[itemName] = existingItem
			} else {
				existing.Items[itemName] = itemDetail
			}
		}
		existing.TotalPrice += info.TotalPrice
	} else {
		fep.sellersMap[sellerKey] = info
	}
	fep.mu.Unlock()
}

// autoFlush ส่งข้อมูลที่รวมแล้วไปยัง Discord
func (fep *FastEconomyProcessor) autoFlush() {
	defer fep.wg.Done()

	ticker := time.NewTicker(fep.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-fep.stopChan:
			fep.Flush()
			return
		case <-ticker.C:
			fep.Flush()
		}
	}
}

// Flush ส่งข้อความทั้งหมดที่ aggregate แล้ว
func (fep *FastEconomyProcessor) Flush() {
	fep.mu.Lock()
	if len(fep.sellersMap) == 0 {
		fep.mu.Unlock()
		return
	}

	// คัดลอกและล้าง map
	toSend := make(map[string]*EconomyLog, len(fep.sellersMap))
	for k, v := range fep.sellersMap {
		toSend[k] = v
	}
	fep.sellersMap = make(map[string]*EconomyLog)
	fep.mu.Unlock()

	// ส่งข้อความ (concurrent)
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 5) // จำกัด concurrent sends

	for _, data := range toSend {
		wg.Add(1)
		go func(info *EconomyLog) {
			defer wg.Done()

			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			if err := fep.sendEconomyLog(info); err != nil {
				log.Printf("❌ Error sending economy log: %v", err)
			}
		}(data)
	}

	wg.Wait()
	log.Printf("✅ Flushed %d aggregated economy logs", len(toSend))
}

// sendEconomyLog ส่ง single economy log
func (fep *FastEconomyProcessor) sendEconomyLog(data *EconomyLog) error {
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

	_, err := fep.session.ChannelMessageSend(fep.channelID, msg)
	if err != nil {
		return err
	}

	UpdateSharedActivity()
	return nil
}

// Stop หยุดการทำงาน
func (fep *FastEconomyProcessor) Stop() {
	close(fep.stopChan)
	fep.wg.Wait()
	log.Println("✅ Fast Economy Processor stopped")
}

// ==========================================
// 🚀 OPTIMIZED PARSING - ใช้ Compiled Regex
// ==========================================

var (
	// Pre-compiled regex patterns
	economySoldPattern      *regexp.Regexp
	economyPurchasedPattern *regexp.Regexp
	loginPattern            *regexp.Regexp

	regexOnce sync.Once
)

func initRegexPatterns() {
	regexOnce.Do(func() {
		// Economy patterns
		economySoldPattern = regexp.MustCompile(
			`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): \[(Trade|Purchase)\] Tradeable \((.+?) \(health: ([\d.]+)(?:, uses: (\d+))?\)\) (sold|bought) by (.+?)\((\d+)\) for (\d+) \((\d+) \+ \d+ worth of contained items\) to trader (.+?), old amount in store is ([\d-]+), new amount is ([\d-]+)`)

		economyPurchasedPattern = regexp.MustCompile(
			`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): \[(Trade|Purchase)\] Tradeable \((.+?) \(x(\d+)\)\) purchased by (.+?)\((\d+)\) for (\d+) money from trader (.+?), old amount in store was ([\d-]+), new amount is ([\d-]+)`)

		// Login pattern
		loginPattern = regexp.MustCompile(
			`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): '(\d+\.\d+\.\d+\.\d+) (\d+):([^(]+)\((\d+)\)' (logged in|logged out) at: X=([\d.-]+) Y=([\d.-]+) Z=([\d.-]+)(?: \((as drone)\))?`)

		log.Println("✅ Regex patterns pre-compiled")
	})
}

// fastParseEconomyLog - Optimized parsing
func fastParseEconomyLog(line string) *EconomyLog {
	if line == "" || !strings.Contains(line, "Tradeable") {
		return nil
	}

	// ข้าม Before/After
	if strings.Contains(line, "Before") || strings.Contains(line, "After") {
		return nil
	}

	initRegexPatterns()

	// ลองแต่ละ pattern
	matches := economySoldPattern.FindStringSubmatch(line)
	if len(matches) >= 13 {
		quantity := 1
		if matches[5] != "" {
			quantity, _ = strconv.Atoi(matches[5])
		}
		totalPrice, _ := strconv.Atoi(matches[9])

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
			UsersOnline: extractUsersOnline(line),
		}
	}

	matches = economyPurchasedPattern.FindStringSubmatch(line)
	if len(matches) >= 10 {
		quantity, _ := strconv.Atoi(matches[4])
		totalPrice, _ := strconv.Atoi(matches[7])

		return &EconomyLog{
			Date:          matches[1],
			TradeType:     "Purchase",
			Seller:        strings.TrimSpace(matches[5]),
			SellerSteamID: matches[6],
			Buyer:         strings.TrimSpace(matches[8]),
			TotalPrice:    totalPrice,
			Items: map[string]Item{
				strings.TrimSpace(matches[3]): {
					Quantity:         quantity,
					TotalPrice:       totalPrice,
					TransactionCount: 1,
				},
			},
			UsersOnline: extractUsersOnline(line),
		}
	}

	return nil
}

// extractUsersOnline - Helper function
func extractUsersOnline(line string) string {
	if !strings.Contains(line, "and effective users online:") {
		return "0"
	}

	pattern := regexp.MustCompile(`and effective users online: (\d+)`)
	match := pattern.FindStringSubmatch(line)
	if len(match) >= 2 {
		return match[1]
	}
	return "0"
}

// fastParseLoginLog - Optimized login parsing
func fastParseLoginLog(line string) *LoginLog {
	if line == "" {
		return nil
	}

	initRegexPatterns()

	matches := loginPattern.FindStringSubmatch(line)
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
		Date:     date.UTC(),
		IP:       matches[2],
		SteamID:  matches[3],
		Name:     matches[4],
		Action:   matches[6],
		Location: Location{X: x, Y: y, Z: z},
		AsDrone:  len(matches) > 10 && matches[10] == "as drone",
	}
}

// ==========================================
// 🚀 FAST LOGIN PROCESSOR
// ==========================================

type FastLoginProcessor struct {
	session    *discordgo.Session
	channelID  string
	queue      chan *LoginLog
	stopChan   chan struct{}
	wg         sync.WaitGroup
	loginTimes map[string]time.Time
	loginMux   sync.RWMutex
}

func NewFastLoginProcessor(session *discordgo.Session, channelID string) *FastLoginProcessor {
	flp := &FastLoginProcessor{
		session:    session,
		channelID:  channelID,
		queue:      make(chan *LoginLog, 500),
		stopChan:   make(chan struct{}),
		loginTimes: make(map[string]time.Time),
	}

	// Worker goroutines
	workerCount := getEnvInt("LOGIN_WORKERS", 2)
	for i := 0; i < workerCount; i++ {
		flp.wg.Add(1)
		go flp.worker()
	}

	log.Printf("⚡ Fast Login Processor initialized (%d workers)", workerCount)
	return flp
}

func (flp *FastLoginProcessor) AddLog(info *LoginLog) {
	select {
	case flp.queue <- info:
	default:
		log.Printf("⚠️ Login queue full")
		go flp.processLogin(info) // Process directly
	}
}

func (flp *FastLoginProcessor) worker() {
	defer flp.wg.Done()

	for {
		select {
		case <-flp.stopChan:
			return
		case info := <-flp.queue:
			flp.processLogin(info)
		}
	}
}

func (flp *FastLoginProcessor) processLogin(info *LoginLog) {
	var msg string

	if info.Action == "logged in" {
		flp.loginMux.Lock()
		flp.loginTimes[info.SteamID] = info.Date
		flp.loginMux.Unlock()

		msg = fmt.Sprintf("```diff\n+login ID: %s IP: %s Name: %s",
			info.SteamID, info.IP, info.Name)
		if info.AsDrone {
			msg += " (as drone)"
		}
		msg += "\n```"
	} else {
		flp.loginMux.Lock()
		loginTime, exists := flp.loginTimes[info.SteamID]
		delete(flp.loginTimes, info.SteamID)
		flp.loginMux.Unlock()

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

	flp.session.ChannelMessageSend(flp.channelID, msg)
	UpdateSharedActivity()
}

func (flp *FastLoginProcessor) Stop() {
	close(flp.stopChan)
	flp.wg.Wait()
	log.Println("✅ Fast Login Processor stopped")
}

// ==========================================
// 🚀 OPTIMIZED PROCESS FUNCTIONS
// ==========================================

// Global processors
var (
	GlobalEconomyProcessor *FastEconomyProcessor
	GlobalLoginProcessor   *FastLoginProcessor
)

// InitializeFastProcessors - เรียกใน main.go
func InitializeFastProcessors(session *discordgo.Session) {
	if Config.EconomyChannelID != "" {
		GlobalEconomyProcessor = NewFastEconomyProcessor(session, Config.EconomyChannelID)
	}

	if Config.LoginChannelID != "" {
		GlobalLoginProcessor = NewFastLoginProcessor(session, Config.LoginChannelID)
	}

	log.Println("🚀 Fast Processors initialized")
}

// StopFastProcessors - เรียกเมื่อปิดโปรแกรม
func StopFastProcessors() {
	if GlobalEconomyProcessor != nil {
		GlobalEconomyProcessor.Stop()
	}
	if GlobalLoginProcessor != nil {
		GlobalLoginProcessor.Stop()
	}
}

// processEconomyLines - เวอร์ชันใหม่ที่เร็วกว่า
func processEconomyLinesOptimized(lines []string) {
	if GlobalEconomyProcessor == nil {
		// Fallback to old method
		processEconomyLines(lines)
		return
	}

	log.Printf("⚡ Processing %d economy lines with fast processor", len(lines))

	for _, line := range lines {
		normalizedLine := strings.TrimSpace(line)
		logKey := normalizedLine + "_economy"

		if !IsLogSentCached(logKey) {
			GlobalEconomyProcessor.AddLine(normalizedLine)
			MarkLogAsSentCached(logKey)
		}
	}

	// Force flush ถ้าจำนวนเยอะ
	if len(lines) > 100 {
		GlobalEconomyProcessor.Flush()
	}
}

// processLoginLines - เวอร์ชันใหม่ที่เร็วกว่า
func processLoginLinesOptimized(lines []string) {
	if GlobalLoginProcessor == nil {
		processLoginLines(lines)
		return
	}

	log.Printf("⚡ Processing %d login lines with fast processor", len(lines))

	for _, line := range lines {
		logKey := line + "_login"
		if !IsLogSentCached(logKey) {
			info := fastParseLoginLog(line)
			if info != nil {
				GlobalLoginProcessor.AddLog(info)
				MarkLogAsSentCached(logKey)
			}
		}
	}
}
