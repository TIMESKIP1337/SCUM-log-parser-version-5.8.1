package app

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/jlaffaye/ftp"
	_ "modernc.org/sqlite"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// ============================================================
// Players Online Module
// 3 sources for accurate player counting:
//   1. login_*.log (primary, most accurate)
//   2. SCUM.log (secondary, real-time server status)
//   3. SCUM.db (cross-reference, catches edge cases)
// Server start time from login_*.log filename to filter stale data.
// ============================================================

const (
	poStatusMessageIDFile = "status_message_id.txt"
	poPage2MessageIDFile  = "page2_message_id.txt"
	poPage3MessageIDFile  = "page3_message_id.txt"
	poPlayersPerPage      = 40
	poLocalDBFile         = "SCUM_online.db"
)

// OnlinePlayer holds parsed player info
type OnlinePlayer struct {
	SteamID  string
	Name     string
	IsDrone  bool
	IsFromDB bool
}

// PlayerDBDetails holds additional info from SCUM.db
type PlayerDBDetails struct {
	Name     string
	Money    float64
	Fame     int
	PlayTime float64
	Attrs    PlayerAttributes
}

// PlayerAttributes holds STR/CON/DEX/INT
type PlayerAttributes struct {
	STR int
	CON int
	DEX int
	INT int
}

// Module state
var (
	poStatusMessageID string
	poPage2MessageID  string
	poPage3MessageID  string
	poMux             sync.Mutex

	poCurrentFakeOffset    int
	poLastOffsetChangeTime time.Time
	poServerWasDown        bool
	poLoopIteration        int
	poDBRefreshCounter     int

	// Server start time (detected from login_*.log filename)
	poServerStartTime *time.Time

	// Compiled regex patterns
	poLoginPattern  *regexp.Regexp // SCUM.log login
	poLogoutPattern *regexp.Regexp // SCUM.log logout

	// Login log patterns (login_*.log format is different from SCUM.log)
	poLoginLogLoginPattern  *regexp.Regexp
	poLoginLogLogoutPattern *regexp.Regexp
	poLoginLogTimestampLine *regexp.Regexp // detect timestamp-starting lines
	poLoginLogFilenamePattern *regexp.Regexp

	// Attribute regex patterns for template_xml
	poAttrSTRPattern *regexp.Regexp
	poAttrCONPattern *regexp.Regexp
	poAttrDEXPattern *regexp.Regexp
	poAttrINTPattern *regexp.Regexp
)

// ============================================================
// Initialization
// ============================================================

func InitializePlayersOnline() error {
	log.Println("👥 Initializing Players Online module...")

	// SCUM.log patterns
	poLoginPattern = regexp.MustCompile(`LogSCUM:\s*'[\d.]+ (\d+):([^'(]+)\(\d+\)'\s*logged in`)
	poLogoutPattern = regexp.MustCompile(`LogSCUM:\s*'[\d.]+ (\d+):([^'(]+)\(\d+\)'\s*logged out`)

	// login_*.log patterns
	// Format: 2025.01.15-09.05.32: '124.122.220.126 76561199241138283:LAMAD(27)' logged in at: X=...
	poLoginLogLoginPattern = regexp.MustCompile(`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}):\s*'[\d.]+ (\d+):([^']+)\(\d+\)'\s*logged in`)
	poLoginLogLogoutPattern = regexp.MustCompile(`(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}):\s*'[\d.]+ (\d+):([^']+)\(\d+\)'\s*logged out`)
	poLoginLogTimestampLine = regexp.MustCompile(`^\s*\d{4}\.\d{2}\.\d{2}`)
	poLoginLogFilenamePattern = regexp.MustCompile(`login_(\d{4})(\d{2})(\d{2})(\d{2})(\d{2})(\d{2})\.log`)

	// Attribute patterns
	poAttrSTRPattern = regexp.MustCompile(`Strength="(\d+)"`)
	poAttrCONPattern = regexp.MustCompile(`Constitution="(\d+)"`)
	poAttrDEXPattern = regexp.MustCompile(`Dexterity="(\d+)"`)
	poAttrINTPattern = regexp.MustCompile(`Intelligence="(\d+)"`)

	// Load persisted message IDs
	poLoadMessageIDs()

	// Initialize fake offset
	if Config.FakePlayerMax > Config.FakePlayerMin {
		poCurrentFakeOffset = Config.FakePlayerMin + rand.Intn(Config.FakePlayerMax-Config.FakePlayerMin+1)
	} else {
		poCurrentFakeOffset = Config.FakePlayerMin
	}
	poLastOffsetChangeTime = time.Now()

	// Clean up leftover DB
	os.Remove(poLocalDBFile)

	log.Printf("👥 Players Online module initialized")
	log.Printf("   Channel: %s, MaxSlot: %d, Fake: %d-%d, Interval: %ds",
		Config.PlayersOnlineChannelID, Config.MaxPlayerSlot,
		Config.FakePlayerMin, Config.FakePlayerMax, Config.PlayersOnlineCheckInterval)
	log.Printf("   SCUM.log: %s", Config.ScumLogPath)
	log.Printf("   SCUM.db: %s", Config.ScumDBPath)
	log.Printf("   Login Log Dir: %s", Config.ScumLoginLogDir)

	return nil
}

// ============================================================
// Message ID persistence
// ============================================================

func poLoadMessageIDs() {
	poStatusMessageID = poLoadMessageIDFromFile(poStatusMessageIDFile)
	poPage2MessageID = poLoadMessageIDFromFile(poPage2MessageIDFile)
	poPage3MessageID = poLoadMessageIDFromFile(poPage3MessageIDFile)

	if poStatusMessageID != "" {
		log.Printf("👥 Loaded Status Message ID: %s", poStatusMessageID)
	}
	if poPage2MessageID != "" {
		log.Printf("👥 Loaded Page 2 Message ID: %s", poPage2MessageID)
	}
	if poPage3MessageID != "" {
		log.Printf("👥 Loaded Page 3 Message ID: %s", poPage3MessageID)
	}
}

func poLoadMessageIDFromFile(filename string) string {
	data, err := os.ReadFile(filename)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func poSaveMessageID(filename string, id string) {
	if id == "" {
		os.Remove(filename)
		return
	}
	if err := os.WriteFile(filename, []byte(id), 0644); err != nil {
		log.Printf("⚠️ [PLAYERS ONLINE] Failed to save message ID to %s: %v", filename, err)
	}
}

// ============================================================
// Fake player offset
// ============================================================

func poUpdateFakeOffset() int {
	if Config.FakePlayerMin == 0 && Config.FakePlayerMax == 0 {
		return 0
	}

	now := time.Now()
	if now.Sub(poLastOffsetChangeTime).Seconds() >= float64(Config.FakePlayerChangeInterval) {
		oldOffset := poCurrentFakeOffset
		if Config.FakePlayerMax > Config.FakePlayerMin {
			poCurrentFakeOffset = Config.FakePlayerMin + rand.Intn(Config.FakePlayerMax-Config.FakePlayerMin+1)
		} else {
			poCurrentFakeOffset = Config.FakePlayerMin
		}
		poLastOffsetChangeTime = now
		log.Printf("👥 Fake offset changed from %d to %d", oldOffset, poCurrentFakeOffset)
	}

	return poCurrentFakeOffset
}

// ============================================================
// Remote IO helpers (protocol-agnostic: FTP or SFTP)
// ============================================================

// poRemoteDirEntry is a minimal directory entry used by Players Online module
type poRemoteDirEntry struct {
	Name  string
	IsDir bool
}

// poReadRemoteFile reads an entire remote file via FTP or SFTP based on ConnectionType
func poReadRemoteFile(remotePath string) ([]byte, error) {
	switch Config.ConnectionType {
	case "FTP":
		conn, release, err := GetMultiFTPConnection()
		if err != nil {
			return nil, fmt.Errorf("FTP connection failed: %w", err)
		}
		defer release()
		resp, err := conn.Retr(remotePath)
		if err != nil {
			return nil, fmt.Errorf("failed to open remote file: %w", err)
		}
		defer resp.Close()
		return io.ReadAll(resp)

	case "SFTP":
		sftpClient, err := GetSFTPConnection()
		if err != nil {
			return nil, fmt.Errorf("SFTP connection failed: %w", err)
		}
		file, err := sftpClient.Open(remotePath)
		if err != nil {
			return nil, fmt.Errorf("failed to open remote file: %w", err)
		}
		defer file.Close()
		return io.ReadAll(file)

	default:
		return nil, fmt.Errorf("no remote connection configured")
	}
}

// poListRemoteDir lists a remote directory via FTP or SFTP
func poListRemoteDir(remotePath string) ([]poRemoteDirEntry, error) {
	switch Config.ConnectionType {
	case "FTP":
		conn, release, err := GetMultiFTPConnection()
		if err != nil {
			return nil, fmt.Errorf("FTP connection failed: %w", err)
		}
		defer release()
		entries, err := conn.List(remotePath)
		if err != nil {
			return nil, err
		}
		result := make([]poRemoteDirEntry, 0, len(entries))
		for _, e := range entries {
			result = append(result, poRemoteDirEntry{
				Name:  e.Name,
				IsDir: e.Type == ftp.EntryTypeFolder,
			})
		}
		return result, nil

	case "SFTP":
		sftpClient, err := GetSFTPConnection()
		if err != nil {
			return nil, fmt.Errorf("SFTP connection failed: %w", err)
		}
		infos, err := sftpClient.ReadDir(remotePath)
		if err != nil {
			return nil, err
		}
		result := make([]poRemoteDirEntry, 0, len(infos))
		for _, info := range infos {
			result = append(result, poRemoteDirEntry{
				Name:  info.Name(),
				IsDir: info.IsDir(),
			})
		}
		return result, nil

	default:
		return nil, fmt.Errorf("no remote connection configured")
	}
}

// ============================================================
// Read SCUM.log
// ============================================================

func poReadScumLogViaSFTP() (string, error) {
	if Config.ScumLogPath == "" {
		return "", fmt.Errorf("SCUM_LOG_PATH not configured")
	}

	data, err := poReadRemoteFile(Config.ScumLogPath)
	if err != nil {
		return "", fmt.Errorf("failed to read SCUM.log: %w", err)
	}

	// Decode UTF-16 LE (SCUM game log format) → UTF-8
	return poDecodeBytes(data), nil
}

// ============================================================
// Server Start Time from login_*.log filename
// ============================================================

// poDetectServerStartTime finds the latest login_*.log and extracts server start time
func poDetectServerStartTime() (*time.Time, string) {
	if Config.ScumLoginLogDir == "" {
		return nil, ""
	}

	files, err := poListRemoteDir(Config.ScumLoginLogDir)
	if err != nil {
		log.Printf("⚠️ [PLAYERS ONLINE] Failed to list login log dir: %v", err)
		return nil, ""
	}

	// Filter and sort login_*.log files
	var loginLogs []string
	for _, f := range files {
		name := f.Name
		if strings.Contains(strings.ToLower(name), "login_") && strings.HasSuffix(name, ".log") {
			loginLogs = append(loginLogs, name)
		}
	}

	if len(loginLogs) == 0 {
		return nil, ""
	}

	sort.Strings(loginLogs)
	latest := loginLogs[len(loginLogs)-1]

	// Extract timestamp from filename: login_20251227090532.log
	match := poLoginLogFilenamePattern.FindStringSubmatch(latest)
	if match == nil {
		return nil, latest
	}

	bangkokTZ := time.FixedZone("ICT", 7*3600)
	var year, month, day, hour, minute, second int
	fmt.Sscanf(match[1], "%d", &year)
	fmt.Sscanf(match[2], "%d", &month)
	fmt.Sscanf(match[3], "%d", &day)
	fmt.Sscanf(match[4], "%d", &hour)
	fmt.Sscanf(match[5], "%d", &minute)
	fmt.Sscanf(match[6], "%d", &second)

	t := time.Date(year, time.Month(month), day, hour, minute, second, 0, bangkokTZ)
	log.Printf("👥 Server Start Time: %s (from %s)", t.Format("2006-01-02 15:04:05"), latest)

	return &t, latest
}

// ============================================================
// Read login_*.log (primary, most accurate source)
// ============================================================

// poReadLoginLogPlayers reads the latest login_*.log file and returns online players
func poReadLoginLogPlayers() []OnlinePlayer {
	if Config.ScumLoginLogDir == "" {
		return nil
	}

	// Find latest login_*.log
	files, err := poListRemoteDir(Config.ScumLoginLogDir)
	if err != nil {
		log.Printf("⚠️ [PLAYERS ONLINE] Failed to list login log dir: %v", err)
		return nil
	}

	var loginLogs []string
	for _, f := range files {
		name := f.Name
		if strings.Contains(strings.ToLower(name), "login_") && strings.HasSuffix(name, ".log") {
			loginLogs = append(loginLogs, name)
		}
	}

	if len(loginLogs) == 0 {
		return nil
	}

	sort.Strings(loginLogs)
	latestLog := loginLogs[len(loginLogs)-1]
	logPath := Config.ScumLoginLogDir + "/" + latestLog

	// Read file
	data, err := poReadRemoteFile(logPath)
	if err != nil {
		log.Printf("⚠️ [PLAYERS ONLINE] Failed to read login log %s: %v", logPath, err)
		return nil
	}

	// Decode UTF-16 LE (SCUM game log format) → UTF-8
	content := poDecodeBytes(data)

	// Preprocess: join lines that don't start with timestamp (multi-line names)
	rawLines := strings.Split(content, "\n")
	var processedLines []string
	for _, line := range rawLines {
		if poLoginLogTimestampLine.MatchString(line) || len(processedLines) == 0 {
			processedLines = append(processedLines, line)
		} else {
			// Continuation of previous line (multi-line player name)
			if len(processedLines) > 0 {
				processedLines[len(processedLines)-1] += " " + strings.TrimSpace(line)
			}
		}
	}

	// Parse login/logout events
	bangkokTZ := time.FixedZone("ICT", 7*3600)

	type loginState struct {
		steamID   string
		name      string
		loginTime time.Time
	}
	onlineMap := make(map[string]*loginState)

	for _, line := range processedLines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Check login
		if m := poLoginLogLoginPattern.FindStringSubmatch(line); m != nil {
			timeStr, steamID, name := m[1], m[2], strings.TrimSpace(m[3])
			loginDT, err := time.Parse("2006.01.02-15.04.05", timeStr)
			if err != nil {
				continue
			}
			loginDT = loginDT.In(bangkokTZ)
			onlineMap[steamID] = &loginState{
				steamID:   steamID,
				name:      name,
				loginTime: loginDT,
			}
			continue
		}

		// Check logout
		if m := poLoginLogLogoutPattern.FindStringSubmatch(line); m != nil {
			steamID := m[2]
			delete(onlineMap, steamID)
			continue
		}
	}

	// Convert to result
	result := make([]OnlinePlayer, 0, len(onlineMap))
	for _, p := range onlineMap {
		result = append(result, OnlinePlayer{
			SteamID: p.steamID,
			Name:    p.name,
			IsDrone: false, // login_*.log doesn't track drone status
		})
	}

	log.Printf("👥 Login Log: %d players online (from %s)", len(result), latestLog)
	return result
}

// ============================================================
// Download SCUM.db
// ============================================================

func poDownloadScumDB() error {
	if Config.ScumDBPath == "" {
		return fmt.Errorf("SCUM_DB_PATH not configured")
	}

	if err := DownloadFileRemote(Config.ScumDBPath, poLocalDBFile, 3); err != nil {
		os.Remove(poLocalDBFile)
		return fmt.Errorf("failed to download SCUM.db: %w", err)
	}

	if info, err := os.Stat(poLocalDBFile); err == nil {
		log.Printf("👥 Downloaded SCUM.db (%d bytes)", info.Size())
	}
	return nil
}

// ============================================================
// SQLite: Query online players from SCUM.db
// ============================================================

func poGetOnlinePlayersFromDB(serverStartTime *time.Time) []OnlinePlayer {
	if !poFileExists(poLocalDBFile) {
		return nil
	}

	db, err := sql.Open("sqlite", poLocalDBFile+"?_pragma=journal_mode(WAL)")
	if err != nil {
		log.Printf("⚠️ [PLAYERS ONLINE] Failed to open SCUM.db: %v", err)
		return nil
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT user_id, name, last_login_time, last_logout_time
		FROM user_profile
		WHERE last_login_time IS NOT NULL
	`)
	if err != nil {
		log.Printf("⚠️ [PLAYERS ONLINE] DB query failed: %v", err)
		return nil
	}
	defer rows.Close()

	bangkokTZ := time.FixedZone("ICT", 7*3600)
	var onlinePlayers []OnlinePlayer

	for rows.Next() {
		var userID, name string
		var loginTimeStr sql.NullString
		var logoutTimeStr sql.NullString

		if err := rows.Scan(&userID, &name, &loginTimeStr, &logoutTimeStr); err != nil {
			continue
		}

		if !loginTimeStr.Valid || loginTimeStr.String == "" {
			continue
		}

		// Parse login time
		loginDT := poParseDBTime(loginTimeStr.String, bangkokTZ)
		if loginDT == nil {
			continue
		}

		// Filter: skip players who logged in before server restart
		if serverStartTime != nil && loginDT.Before(*serverStartTime) {
			continue
		}

		isOnline := true

		// Check logout
		if logoutTimeStr.Valid && logoutTimeStr.String != "" {
			logoutDT := poParseDBTime(logoutTimeStr.String, bangkokTZ)
			if logoutDT != nil && logoutDT.After(*loginDT) {
				isOnline = false
			}
		}

		if isOnline {
			playerName := name
			if playerName == "" {
				playerName = "Unknown"
			}
			onlinePlayers = append(onlinePlayers, OnlinePlayer{
				SteamID:  userID,
				Name:     playerName,
				IsDrone:  false,
				IsFromDB: true,
			})
		}
	}

	return onlinePlayers
}

// poParseDBTime parses DB time formats (UTC) and converts to target timezone
func poParseDBTime(s string, tz *time.Location) *time.Time {
	formats := []string{
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05.00Z",
		"2006-01-02T15:04:05.0Z",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			result := t.In(tz)
			return &result
		}
	}
	return nil
}

// poGetPlayerDetailsFromDB queries SCUM.db for player details
func poGetPlayerDetailsFromDB(steamIDs []string) map[string]*PlayerDBDetails {
	details := make(map[string]*PlayerDBDetails)

	if !poFileExists(poLocalDBFile) || len(steamIDs) == 0 {
		return details
	}

	db, err := sql.Open("sqlite", poLocalDBFile+"?_pragma=journal_mode(WAL)")
	if err != nil {
		return details
	}
	defer db.Close()

	placeholders := make([]string, len(steamIDs))
	args := make([]interface{}, len(steamIDs))
	for i, id := range steamIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := fmt.Sprintf(`
		SELECT user_id, name, money_balance, fame_points, template_xml, play_time
		FROM user_profile
		WHERE user_id IN (%s)
	`, strings.Join(placeholders, ","))

	rows, err := db.Query(query, args...)
	if err != nil {
		return details
	}
	defer rows.Close()

	for rows.Next() {
		var userID string
		var name sql.NullString
		var money sql.NullFloat64
		var fame sql.NullInt64
		var templateXML sql.NullString
		var playTime sql.NullFloat64

		if err := rows.Scan(&userID, &name, &money, &fame, &templateXML, &playTime); err != nil {
			continue
		}

		d := &PlayerDBDetails{}
		if name.Valid {
			d.Name = name.String
		}
		if money.Valid {
			d.Money = money.Float64
		}
		if fame.Valid {
			d.Fame = int(fame.Int64)
		}
		if playTime.Valid {
			d.PlayTime = playTime.Float64
		}

		if templateXML.Valid && templateXML.String != "" {
			if m := poAttrSTRPattern.FindStringSubmatch(templateXML.String); m != nil {
				fmt.Sscanf(m[1], "%d", &d.Attrs.STR)
			}
			if m := poAttrCONPattern.FindStringSubmatch(templateXML.String); m != nil {
				fmt.Sscanf(m[1], "%d", &d.Attrs.CON)
			}
			if m := poAttrDEXPattern.FindStringSubmatch(templateXML.String); m != nil {
				fmt.Sscanf(m[1], "%d", &d.Attrs.DEX)
			}
			if m := poAttrINTPattern.FindStringSubmatch(templateXML.String); m != nil {
				fmt.Sscanf(m[1], "%d", &d.Attrs.INT)
			}
		}

		details[userID] = d
	}

	return details
}

// poTruncateRunes truncates a string to maxRunes characters (not bytes)
// Safe for Thai, Chinese, Japanese and other multi-byte characters
func poTruncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return s
}

// poPadRight pads a string with spaces to a fixed width based on rune count (not bytes)
// This ensures correct alignment for Thai, Chinese, Japanese etc.
func poPadRight(s string, width int) string {
	runeLen := len([]rune(s))
	if runeLen >= width {
		return s
	}
	return s + strings.Repeat(" ", width-runeLen)
}

func poFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// poDecodeBytes tries UTF-16 LE first, falls back to UTF-8
// SCUM game logs are typically UTF-16 LE encoded
func poDecodeBytes(data []byte) string {
	// Try UTF-16 LE decode first
	decoder := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
	decoded, err := io.ReadAll(transform.NewReader(bytes.NewReader(data), decoder))
	if err == nil && utf8.Valid(decoded) {
		return string(decoded)
	}

	// Check for BOM and strip it
	if len(data) >= 2 {
		if data[0] == 0xFF && data[1] == 0xFE {
			// UTF-16 LE BOM - decode without BOM bytes
			decoder := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
			decoded, err := io.ReadAll(transform.NewReader(bytes.NewReader(data[2:]), decoder))
			if err == nil && utf8.Valid(decoded) {
				return string(decoded)
			}
		} else if data[0] == 0xFE && data[1] == 0xFF {
			// UTF-16 BE BOM
			decoder := unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewDecoder()
			decoded, err := io.ReadAll(transform.NewReader(bytes.NewReader(data[2:]), decoder))
			if err == nil && utf8.Valid(decoded) {
				return string(decoded)
			}
		}
	}

	// Fall back to UTF-8 (raw bytes as string)
	return SafeUTF8String(string(data))
}

// ============================================================
// Server shutdown detection (SCUM.log)
// ============================================================

func poCheckServerShutdown(content string) bool {
	lines := strings.Split(content, "\n")
	startIdx := len(lines) - 50
	if startIdx < 0 {
		startIdx = 0
	}
	lastLines := strings.Join(lines[startIdx:], "\n")
	return strings.Contains(lastLines, "[UConZGameInstance::ShutDown]")
}

// ============================================================
// SCUM.log parser (secondary source - also gives drone info)
// ============================================================

func poParseOnlinePlayersFromScumLog(content string) []OnlinePlayer {
	type playerState struct {
		steamID string
		name    string
		isDrone bool
	}
	onlineMap := make(map[string]*playerState)

	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if loginMatch := poLoginPattern.FindStringSubmatch(line); loginMatch != nil {
			steamID := loginMatch[1]
			name := strings.TrimSpace(loginMatch[2])
			isDrone := strings.Contains(line, "(as drone)")

			onlineMap[steamID] = &playerState{
				steamID: steamID,
				name:    name,
				isDrone: isDrone,
			}
			continue
		}

		if logoutMatch := poLogoutPattern.FindStringSubmatch(line); logoutMatch != nil {
			steamID := logoutMatch[1]
			delete(onlineMap, steamID)
			continue
		}
	}

	result := make([]OnlinePlayer, 0, len(onlineMap))
	for _, p := range onlineMap {
		result = append(result, OnlinePlayer{
			SteamID: p.steamID,
			Name:    p.name,
			IsDrone: p.isDrone,
		})
	}

	return result
}

// ============================================================
// Cross-reference: merge all 3 sources
// ============================================================

// poMergeAllSources merges players from login_log, SCUM.log, and DB
// Priority: login_log > SCUM.log > DB
// Drone status comes from SCUM.log (only source that has it)
func poMergeAllSources(loginLogPlayers, scumLogPlayers, dbPlayers []OnlinePlayer) []OnlinePlayer {
	merged := make(map[string]OnlinePlayer)

	// 1. Start with login_log (primary, most accurate)
	for _, p := range loginLogPlayers {
		merged[p.SteamID] = p
	}

	// 2. Add/enrich from SCUM.log (has drone info)
	for _, p := range scumLogPlayers {
		if existing, exists := merged[p.SteamID]; exists {
			// Update drone status from SCUM.log (only source that tracks it)
			existing.IsDrone = p.IsDrone
			// Use SCUM.log name if login_log name is empty
			if existing.Name == "" || existing.Name == "Unknown" {
				existing.Name = p.Name
			}
			merged[p.SteamID] = existing
		} else {
			// Player in SCUM.log but not in login_log - add it
			merged[p.SteamID] = p
		}
	}

	// 3. Add from DB (catches any remaining edge cases)
	dbAdded := 0
	for _, p := range dbPlayers {
		if _, exists := merged[p.SteamID]; !exists {
			merged[p.SteamID] = p
			dbAdded++
		}
	}

	if dbAdded > 0 {
		log.Printf("👥 Cross-reference: +%d players from DB (not in logs)", dbAdded)
	}

	result := make([]OnlinePlayer, 0, len(merged))
	for _, p := range merged {
		result = append(result, p)
	}

	return result
}

// ============================================================
// Discord message building
// ============================================================

func poBuildStatusMessages(players []OnlinePlayer, fakeOffset int) (page1, page2, page3 string) {
	playerCount := 0
	droneCount := 0
	for _, p := range players {
		if p.IsDrone {
			droneCount++
		} else {
			playerCount++
		}
	}

	displayCount := len(players) + fakeOffset
	if displayCount > Config.MaxPlayerSlot {
		displayCount = Config.MaxPlayerSlot
	}
	if displayCount < 0 {
		displayCount = 0
	}

	now := time.Now().Format("15:04:05")

	// Helper: build one player row with rune-safe padding
	buildPlayerRow := func(num int, p OnlinePlayer) string {
		tag := "Player"
		if p.IsDrone {
			tag = "Admin "
		}
		name := poPadRight(poTruncateRunes(p.Name, 16), 16)
		return fmt.Sprintf(" %-3d | %-6s | %s | %s\n", num, tag, name, p.SteamID)
	}

	// Header line for codeblock
	header := " #   | Type   | Name             | Steam ID\n" +
		"-----+--------+------------------+--------------------\n"

	// Page 1
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("```\n"+
		"==============================================\n"+
		"   Players Online Status  [%s]\n"+
		"==============================================\n"+
		"   Players: %-4d  Admin: %-4d  Total: %d/%d\n"+
		"==============================================\n",
		now, playerCount, droneCount, displayCount, Config.MaxPlayerSlot))
	sb.WriteString(header)

	end := poPlayersPerPage
	if end > len(players) {
		end = len(players)
	}
	for i, p := range players[:end] {
		sb.WriteString(buildPlayerRow(i+1, p))
	}
	sb.WriteString("```")
	page1 = sb.String()

	// Page 2
	if len(players) > poPlayersPerPage {
		var sb2 strings.Builder
		sb2.WriteString("```\n")
		sb2.WriteString("--- Page 2 ---\n")
		sb2.WriteString(header)

		end2 := poPlayersPerPage * 2
		if end2 > len(players) {
			end2 = len(players)
		}
		// Re-number from page offset
		for i, p := range players[poPlayersPerPage:end2] {
			sb2.WriteString(buildPlayerRow(poPlayersPerPage+i+1, p))
		}
		sb2.WriteString("```")
		page2 = sb2.String()
	}

	// Page 3
	if len(players) > poPlayersPerPage*2 {
		var sb3 strings.Builder
		sb3.WriteString("```\n")
		sb3.WriteString("--- Page 3 ---\n")
		sb3.WriteString(header)

		end3 := poPlayersPerPage * 3
		if end3 > len(players) {
			end3 = len(players)
		}
		for i, p := range players[poPlayersPerPage*2 : end3] {
			sb3.WriteString(buildPlayerRow(poPlayersPerPage*2+i+1, p))
		}
		sb3.WriteString("```")
		if len(players) > poPlayersPerPage*3 {
			sb3.WriteString(fmt.Sprintf("\n... and %d more", len(players)-poPlayersPerPage*3))
		}
		page3 = sb3.String()
	}

	return
}

// ============================================================
// Discord message update
// ============================================================

func poUpdateDiscordMessage(session *discordgo.Session, channelID string, messageID string, content string) string {
	if content == "" {
		if messageID != "" {
			if err := session.ChannelMessageDelete(channelID, messageID); err != nil {
				log.Printf("⚠️ [PLAYERS ONLINE] Failed to delete message: %v", err)
			}
		}
		return ""
	}

	if len(content) > 2000 {
		content = content[:1900] + "\n... (ตัดบางส่วน)"
	}

	if messageID != "" {
		_, err := session.ChannelMessageEdit(channelID, messageID, content)
		if err == nil {
			return messageID
		}
		log.Printf("⚠️ [PLAYERS ONLINE] Could not edit message %s: %v (will create new)", messageID, err)
	}

	msg, err := session.ChannelMessageSend(channelID, content)
	if err != nil {
		log.Printf("❌ [PLAYERS ONLINE] Failed to send message: %v", err)
		return messageID
	}
	return msg.ID
}

// ============================================================
// Bot presence
// ============================================================

func poUpdateBotPresence(session *discordgo.Session, displayCount int) {
	err := session.UpdateStatusComplex(discordgo.UpdateStatusData{
		Activities: []*discordgo.Activity{
			{
				Name: fmt.Sprintf("%d Players online", displayCount),
				Type: discordgo.ActivityTypeWatching,
			},
		},
	})
	if err != nil {
		log.Printf("⚠️ [PLAYERS ONLINE] Failed to update bot presence: %v", err)
	}
}

// ============================================================
// Main processing function (called by scheduler)
// ============================================================

func processPlayersOnline() {
	poMux.Lock()
	defer poMux.Unlock()

	poLoopIteration++

	session := SharedSession
	if session == nil {
		return
	}

	channelID := Config.PlayersOnlineChannelID
	if channelID == "" {
		return
	}

	// ---- Step 1: Read SCUM.log (needed for shutdown detection + drone info) ----
	scumLogContent, err := poReadScumLogViaSFTP()
	if err != nil {
		log.Printf("⚠️ [PLAYERS ONLINE] Failed to read SCUM.log: %v", err)
		return
	}

	// ---- Step 2: Check server shutdown ----
	if poCheckServerShutdown(scumLogContent) {
		if !poServerWasDown {
			poServerWasDown = true
			poServerStartTime = nil // reset server start time
			log.Printf("⚠️ [PLAYERS ONLINE] Server shutdown detected")

			poUpdateBotPresence(session, 0)

			msg := "🔴 **เซิร์ฟเวอร์ปิดอยู่** - กำลังรอ Restart..."
			poStatusMessageID = poUpdateDiscordMessage(session, channelID, poStatusMessageID, msg)
			poSaveMessageID(poStatusMessageIDFile, poStatusMessageID)

			if poPage2MessageID != "" {
				poPage2MessageID = poUpdateDiscordMessage(session, channelID, poPage2MessageID, "")
				poSaveMessageID(poPage2MessageIDFile, poPage2MessageID)
			}
			if poPage3MessageID != "" {
				poPage3MessageID = poUpdateDiscordMessage(session, channelID, poPage3MessageID, "")
				poSaveMessageID(poPage3MessageIDFile, poPage3MessageID)
			}
		}
		return
	}

	// Server is back online
	if poServerWasDown {
		poServerWasDown = false
		poServerStartTime = nil // force re-detect
		log.Printf("✅ [PLAYERS ONLINE] Server is back online")
		session.ChannelMessageSend(channelID, "🟢 **เซิร์ฟเวอร์กลับมาออนไลน์**")
	}

	// ---- Step 3: Detect server start time (once, or after restart) ----
	if poServerStartTime == nil {
		startTime, _ := poDetectServerStartTime()
		poServerStartTime = startTime
	}

	// ---- Step 4: Read login_*.log (primary source, most accurate) ----
	loginLogPlayers := poReadLoginLogPlayers()

	// ---- Step 5: Parse SCUM.log (secondary source, has drone detection) ----
	scumLogPlayers := poParseOnlinePlayersFromScumLog(scumLogContent)

	// ---- Step 6: Download SCUM.db and cross-reference (every ~60s) ----
	var dbPlayers []OnlinePlayer
	poDBRefreshCounter++
	interval := Config.PlayersOnlineCheckInterval
	if interval < 1 {
		interval = 10
	}
	shouldRefreshDB := poDBRefreshCounter >= (60 / interval)

	if shouldRefreshDB && Config.ScumDBPath != "" {
		poDBRefreshCounter = 0
		if err := poDownloadScumDB(); err != nil {
			log.Printf("⚠️ [PLAYERS ONLINE] Failed to download SCUM.db: %v", err)
		} else {
			dbPlayers = poGetOnlinePlayersFromDB(poServerStartTime)
			os.Remove(poLocalDBFile)
		}
	}

	// ---- Step 7: Merge all sources for accurate count ----
	players := poMergeAllSources(loginLogPlayers, scumLogPlayers, dbPlayers)

	// ---- Step 8: Update fake offset ----
	fakeOffset := poUpdateFakeOffset()

	// ---- Step 9: Build messages ----
	page1, page2, page3 := poBuildStatusMessages(players, fakeOffset)

	// ---- Step 10: Update Discord messages ----
	newStatusID := poUpdateDiscordMessage(session, channelID, poStatusMessageID, page1)
	if newStatusID != poStatusMessageID {
		poStatusMessageID = newStatusID
		poSaveMessageID(poStatusMessageIDFile, poStatusMessageID)
	}

	newPage2ID := poUpdateDiscordMessage(session, channelID, poPage2MessageID, page2)
	if newPage2ID != poPage2MessageID {
		poPage2MessageID = newPage2ID
		poSaveMessageID(poPage2MessageIDFile, poPage2MessageID)
	}

	newPage3ID := poUpdateDiscordMessage(session, channelID, poPage3MessageID, page3)
	if newPage3ID != poPage3MessageID {
		poPage3MessageID = newPage3ID
		poSaveMessageID(poPage3MessageIDFile, poPage3MessageID)
	}

	// ---- Step 11: Update bot presence ----
	displayCount := len(players) + fakeOffset
	if displayCount > Config.MaxPlayerSlot {
		displayCount = Config.MaxPlayerSlot
	}
	if displayCount < 0 {
		displayCount = 0
	}
	poUpdateBotPresence(session, displayCount)

	// ---- Log periodically ----
	if poLoopIteration%10 == 0 {
		playerCount := 0
		droneCount := 0
		for _, p := range players {
			if p.IsDrone {
				droneCount++
			} else {
				playerCount++
			}
		}
		srcLogin := len(loginLogPlayers)
		srcScum := len(scumLogPlayers)
		srcDB := len(dbPlayers)
		log.Printf("👥 [PLAYERS ONLINE] Loop #%d: %d players + %d drones = %d total (display: %d) | Sources: login_log=%d, scum_log=%d, db=%d",
			poLoopIteration, playerCount, droneCount, len(players), displayCount, srcLogin, srcScum, srcDB)
	}
}
