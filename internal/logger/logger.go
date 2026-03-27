package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// ANSI color codes
const (
	Reset   = "\033[0m"
	Red     = "\033[31m"
	Green   = "\033[32m"
	Yellow  = "\033[33m"
	Blue    = "\033[34m"
	Magenta = "\033[35m"
	Cyan    = "\033[36m"
	White   = "\033[37m"
	Gray    = "\033[90m"

	// Bold variants
	BoldRed     = "\033[1;31m"
	BoldGreen   = "\033[1;32m"
	BoldYellow  = "\033[1;33m"
	BoldBlue    = "\033[1;34m"
	BoldMagenta = "\033[1;35m"
	BoldCyan    = "\033[1;36m"
	BoldWhite   = "\033[1;37m"
)

// ColoredWriter wraps an io.Writer and adds colors based on content
type ColoredWriter struct {
	out io.Writer
	mu  sync.Mutex
}

var (
	initialized   bool
	initMu        sync.Mutex
	defaultLogger *ColoredWriter
)

// Init enables ANSI colors on Windows console and sets up colored logging
func Init() {
	initMu.Lock()
	defer initMu.Unlock()

	if initialized {
		return
	}

	// Enable virtual terminal processing on Windows
	stdout := windows.Handle(os.Stdout.Fd())
	var mode uint32
	windows.GetConsoleMode(stdout, &mode)
	windows.SetConsoleMode(stdout, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)

	stderr := windows.Handle(os.Stderr.Fd())
	windows.GetConsoleMode(stderr, &mode)
	windows.SetConsoleMode(stderr, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)

	// Create colored writer and set as default log output
	defaultLogger = &ColoredWriter{out: os.Stdout}
	log.SetOutput(defaultLogger)
	log.SetFlags(0) // We'll handle timestamp ourselves

	initialized = true
}

// Write implements io.Writer with color support
func (w *ColoredWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	msg := string(p)
	timestamp := time.Now().Format("2006/01/02 15:04:05")

	// Determine prefix and color based on content
	var prefix, color string

	switch {
	// Status reports (check BEFORE errors to avoid matching "Failed: 0")
	case contains(msg, "Status -", "Internal Stats -"):
		prefix = "[STATUS]"
		color = Cyan

	// Errors
	case contains(msg, "ERROR", "error", "Error", "failed", "Failed", "FAILED", "cannot", "Cannot"):
		prefix = "[ERROR]"
		color = Red
	case contains(msg, "FATAL", "fatal", "Fatal"):
		prefix = "[FATAL]"
		color = BoldRed
	case contains(msg, "panic", "Panic", "PANIC"):
		prefix = "[PANIC]"
		color = BoldRed

	// Warnings
	case contains(msg, "WARN", "warn", "Warning", "warning", "⚠"):
		prefix = "[WARN]"
		color = Yellow

	// Success
	case contains(msg, "✅", "success", "Success", "SUCCESS", "completed", "Completed", "started", "Started", "Active", "active", "loaded", "Loaded", "initialized", "Initialized", "enabled", "Enabled", "verified", "Verified", "valid", "Valid"):
		prefix = "[OK]"
		color = Green

	// Database
	case contains(msg, "database", "Database", "DB", "PostgreSQL", "SQLite", "query", "Query"):
		prefix = "[DB]"
		color = BoldCyan

	// Discord
	case contains(msg, "Discord", "discord", "Bot is ready", "bot is ready"):
		prefix = "[DC]"
		color = BoldBlue

	// Game
	case contains(msg, "SCUM", "game", "Game", "window", "Window", "HWND", "command", "Command", "🎮"):
		prefix = "[GAME]"
		color = Blue

	// Security
	case contains(msg, "License", "license", "HWID", "hwid", "security", "Security", "Protection", "protection", "🛡"):
		prefix = "[SEC]"
		color = BoldMagenta

	// Trading/Economy
	case contains(msg, "trade", "Trade", "coin", "Coin", "gold", "Gold", "exchange", "Exchange", "shop", "Shop", "💰"):
		prefix = "[TRADE]"
		color = BoldYellow

	// Player
	case contains(msg, "player", "Player", "Steam", "steam", "whitelist", "Whitelist", "register", "Register", "👤"):
		prefix = "[PLAYER]"
		color = BoldGreen

	// System
	case contains(msg, "system", "System", "Starting", "starting", "Initializing", "initializing", "Loading", "loading", "🚀", "📊", "📋"):
		prefix = "[SYS]"
		color = Magenta

	// Network
	case contains(msg, "HTTP", "http", "API", "api", "connection", "Connection", "SFTP", "sftp", "FTP", "ftp", "📡"):
		prefix = "[NET]"
		color = Cyan

	// Debug
	case contains(msg, "debug", "Debug", "DEBUG"):
		prefix = "[DEBUG]"
		color = Gray

	// Default info
	default:
		prefix = "[INFO]"
		color = Cyan
	}

	// Clean up the message (remove existing emoji prefixes for cleaner output)
	cleanMsg := cleanMessage(msg)

	// Format: timestamp [COLORED PREFIX] message (message in magenta/pink)
	formatted := fmt.Sprintf("%s%s %s%-7s%s %s%s\n", Magenta, timestamp, color, prefix, Magenta, cleanMsg, Reset)

	return w.out.Write([]byte(formatted))
}

// contains checks if the message contains any of the given substrings
func contains(msg string, substrs ...string) bool {
	for _, s := range substrs {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// cleanMessage removes common prefixes, emojis, and cleans up the message
func cleanMessage(msg string) string {
	// Remove trailing newline
	msg = strings.TrimSuffix(msg, "\n")

	// Remove ALL emojis and special characters from the message
	emojis := []string{
		// Common emojis
		"✅", "❌", "⚠️", "⚠", "🎮", "📡", "🔑", "🛡️", "🛡", "📊", "📋", "🚀", "💰", "👤",
		"🧹", "🔄", "⭐", "✓", "🎁", "🔒", "💎", "🏆", "📦", "🎯", "🔧", "💀", "⏰", "🌟",
		"🎲", "💡", "📝", "🔥", "🎉", "🎊", "💥", "⚡", "🌐", "🖥️", "🖥", "💻", "📱", "🔔",
		"🔕", "✨", "💫", "🌈", "🏅", "🎖️", "🎖", "🥇", "🥈", "🥉", "💵", "💴", "💶", "💷",
		"🪙", "💳", "🏦", "📈", "📉", "🔐", "🔓", "🗝️", "🗝", "⏱️", "⏱", "⏲️", "⏲",
		"🕐", "🕑", "🕒", "🕓", "🕔", "🕕", "🕖", "🕗", "🕘", "🕙", "🕚", "🕛", "📅", "📆",
		"🗓️", "🗓", "➡️", "➡", "⬅️", "⬅", "⬆️", "⬆", "⬇️", "⬇", "↗️", "↗", "↘️", "↘",
		"↙️", "↙", "↖️", "↖", "↕️", "↕", "↔️", "↔", "🔃", "🔀", "🔁", "🔂", "▶️", "▶",
		"⏸️", "⏸", "⏹️", "⏹", "⏺️", "⏺", "⏭️", "⏭", "⏮️", "⏮", "⏩", "⏪", "🔼", "🔽",
		"ℹ️", "ℹ", "❓", "❔", "❗", "❕", "‼️", "‼", "⁉️", "⁉", "🔴", "🟠", "🟡", "🟢",
		"🔵", "🟣", "🟤", "⚫", "⚪", "🟥", "🟧", "🟨", "🟩", "🟦", "🟪", "🟫", "⬛", "⬜",
		"◼️", "◼", "◻️", "◻", "◾", "◽", "▪️", "▪", "▫️", "▫", "🔶", "🔷", "🔸", "🔹",
		"🔺", "🔻", "💠", "🔘", "🔳", "🔲",
		// Block characters and bars
		"▓▓", "▓", "░░", "░", "█", "▄", "▀", "▌", "▐", "│", "┃", "┆", "┇", "┊", "┋",
		"─", "━", "┄", "┅", "┈", "┉", "╌", "╍", "═", "║", "╒", "╓", "╔", "╕", "╖", "╗",
		"╘", "╙", "╚", "╛", "╜", "╝", "╞", "╟", "╠", "╡", "╢", "╣", "╤", "╥", "╦", "╧",
		"╨", "╩", "╪", "╫", "╬", "╭", "╮", "╯", "╰", "╱", "╲", "╳", "╴", "╵", "╶", "╷",
		"┌", "┍", "┎", "┏", "┐", "┑", "┒", "┓", "└", "┕", "┖", "┗", "┘", "┙", "┚", "┛",
		"├", "┝", "┞", "┟", "┠", "┡", "┢", "┣", "┤", "┥", "┦", "┧", "┨", "┩", "┪", "┫",
		"┬", "┭", "┮", "┯", "┰", "┱", "┲", "┳", "┴", "┵", "┶", "┷", "┸", "┹", "┺", "┻",
		"┼", "┽", "┾", "┿", "╀", "╁", "╂", "╃", "╄", "╅", "╆", "╇", "╈", "╉", "╊", "╋",
		// Additional symbols
		"•", "◦", "‣", "⁃", "◘", "○", "◌", "◍", "◎", "●", "◐", "◑", "◒", "◓", "◔", "◕",
		"◖", "◗", "◙", "◚", "◛", "◜", "◝", "◞", "◟", "◠", "◡", "◢", "◣", "◤", "◥",
		"◧", "◨", "◩", "◪", "◫", "◬", "◭", "◮", "◯", "◰", "◱", "◲", "◳", "◴", "◵", "◶", "◷",
		"★", "☆", "✦", "✧", "✩", "✪", "✫", "✬", "✭", "✮", "✯", "✰",
		"♠", "♡", "♢", "♣", "♤", "♥", "♦", "♧",
		"☐", "☑", "☒", "✔", "✕", "✖", "✗", "✘",
		"♩", "♪", "♫", "♬", "♭", "♮", "♯",
		"☀", "☁", "☂", "☃", "☄", "★", "☆", "☇", "☈", "☉", "☊", "☋", "☌", "☍",
		"→", "←", "↑", "↓", "↔", "↕", "↖", "↗", "↘", "↙", "↚", "↛", "↜", "↝", "↞", "↟",
		"↠", "↡", "↢", "↣", "↤", "↥", "↦", "↧", "↨", "↩", "↪", "↫", "↬", "↭", "↮", "↯",
		"⇐", "⇑", "⇒", "⇓", "⇔", "⇕", "⇖", "⇗", "⇘", "⇙", "⇚", "⇛",
		"∞", "≈", "≠", "≤", "≥", "±", "×", "÷", "√", "∑", "∏", "∫", "∂", "∆", "∇",
		// More emojis from logs
		"💜", "💙", "💚", "💛", "🧡", "❤️", "❤", "🖤", "🤍", "🤎", "💔", "❣️", "❣", "💕", "💞", "💓", "💗", "💖", "💘", "💝",
		"🎰", "🎯", "🕹️", "🕹", "🃏", "🀄", "🎴", "🎭", "🎪",
		"📌", "📍", "📎", "🖇️", "🖇", "📏", "📐", "🔖", "🏷️", "🏷",
		"🔫", "💣", "🔪", "🗡️", "🗡", "⚔️", "⚔", "🏹", "⚒️", "⚒", "🔨", "⛏️", "⛏", "🪓", "🔩", "⚙️", "⚙",
		"🚩", "🏴", "🏳️", "🏳", "🎌", "🏁", "🛸", "🛩️", "🛩", "✈️", "✈", "🚁", "🛶", "⛵", "🚤", "🛥️", "🛥",
		"🏝️", "🏝", "🏖️", "🏖", "🏕️", "🏕", "⛺", "🏠", "🏡", "🏢", "🏣", "🏤", "🏥", "🏨", "🏩", "🏪", "🏫", "🏬",
		"☠️", "☠", "👻", "👽", "👾", "🤖", "🎃", "😈", "👿", "👹", "👺", "🙈", "🙉", "🙊",
		"🐾", "🦴", "🐕", "🐈", "🐀", "🐁", "🐿️", "🐿", "🦔", "🦇", "🐻", "🐨", "🐼", "🦁", "🐯", "🐮", "🐷", "🐸", "🐵",
		"🌍", "🌎", "🌏", "🗺️", "🗺", "🧭", "🏔️", "🏔", "⛰️", "⛰", "🌋", "🗻", "🏜️", "🏜", "🏞️", "🏞",
		"🔊", "🔉", "🔈", "🔇", "📢", "📣", "📯", "🎵", "🎶", "🎼", "🎤", "🎧", "📻", "🎷", "🎸", "🎹", "🎺", "🎻",
		"⏳", "⌛", "🕰️", "🕰", "🌡️", "🌡", "🧪", "🧫", "🧬", "🔬", "🔭",
		"💉", "🩸", "💊", "🩹", "🩺", "🚑",
		"🔋", "🔌", "🔦", "🕯️", "🕯", "🧯", "🛢️", "🛢", "💸", "🧾",
		"⚗️", "⚗", "🧲", "🧰", "🧱", "⛓️", "⛓", "🧨", "🪤", "🪣", "🫧",
		"📲", "☎️", "☎", "📞", "📟", "📠", "🖨️", "🖨", "⌨️", "⌨", "🖱️", "🖱", "🖲️", "🖲",
		"💿", "📀", "🧮", "🎥", "🎞️", "🎞", "📽️", "📽", "🎬", "📺", "📷", "📸", "📹", "📼", "🔍", "🔎", "🕵️", "🕵",
		"🌀", "🌊", "🌙", "🌛", "🌜", "🌝", "🌞", "☀️", "🌤️", "🌤", "⛅", "🌥️", "🌥", "☁️",
		"🌦️", "🌦", "🌧️", "🌧", "⛈️", "⛈", "🌩️", "🌩", "🌨️", "🌨", "❄️", "❄", "☃️", "⛄", "🌬️", "🌬", "💨", "🌪️", "🌪",
		"🌫️", "🌫", "☔", "💧", "💦",
		"🎱", "🎳", "🏀", "⚽", "🏈", "⚾", "🎾", "🏐", "🏉", "🥏", "🎿", "⛷️", "⛷", "🏂", "⛸️", "⛸", "🥌", "🛷",
		"🏋️", "🏋", "🤸", "⛹️", "⛹", "🤾", "🏌️", "🏌", "🧘", "🧗", "🚴", "🚵", "🏇", "🎽",
		"♻️", "♻", "⚜️", "⚜", "🔱", "📛", "🔰", "⭕", "✅", "☑️", "✔️", "❌", "❎", "➕", "➖", "➗", "➰", "➿", "〽️", "✳️", "✴️", "❇️",
		"©️", "©", "®️", "®", "™️", "™",
		// Additional from user
		"🚗", "🚕", "🚙", "🚌", "🚎", "🏎️", "🏎", "🚓", "🚑", "🚒", "🚐", "🛻", "🚚", "🚛", "🚜", "🛵", "🏍️", "🏍", "🛺", "🚲", "🛴", "🛹",
		"🆕", "🆓", "🆙", "🆗", "🆒", "🆖", "🆔", "🆚", "🈁", "🈂️", "🈷️", "🈶", "🈯", "🉐", "🈹", "🈚", "🈲", "🉑", "🈸", "🈴", "🈳", "㊗️", "㊙️", "🈺", "🈵",
		"✏️", "✏", "✒️", "✒", "🖋️", "🖋", "🖊️", "🖊", "🖌️", "🖌", "🖍️", "🖍", "📝", "✍️", "✍",
		"🛍️", "🛍", "🛒", "🎒", "👜", "👝", "🧳", "👛", "👞", "👟", "🥾", "🥿", "👠", "👡", "🩰", "👢", "👑", "👒", "🎩", "🎓", "🧢", "⛑️", "⛑",
		"💄", "💍", "💼", "🌂", "☂️", "☂",
		"🗺️", "🗺",
		"💱", "💲", "🪬", "🧿", "🔮", "🪄", "🎀", "🎗️", "🎗", "🎟️", "🎟", "🎫", "🧧",
		"📖", "📗", "📘", "📙", "📚", "📓", "📒", "📃", "📜", "📄", "📰", "🗞️", "🗞", "📑", "🔖", "🏷️", "🏷",
		"🚫", "⛔", "🚳", "🚭", "🚯", "🚱", "🚷", "📵", "🔞", "☢️", "☢", "☣️", "☣", "🛑",
		"🚨", "🚧", "🔺", "🔻", "🔸", "🔹", "🔶", "🔷",
		"🗑️", "🗑", "🗃️", "🗃", "🗄️", "🗄", "🗂️", "🗂",
		"📥", "📤", "📨", "📩", "📧", "💌", "📮", "🗳️", "🗳",
		"👁️", "👁", "👀", "👃", "👂", "🦻", "👅", "👄", "🫦", "🦷", "🦴",
		"📁", "📂", "🗂️", "🗂", "🗃️", "🗃", "🗄️", "🗄", "🗑️", "🗑",
		"🔗", "⛓️", "⛓", "🔗️",
		// Log parser specific
		"💓", "🧠", "♻️", "🔌", "🤖", "👮", "🔓", "🎫", "👋",
	}

	for _, e := range emojis {
		msg = strings.ReplaceAll(msg, e+" ", "")
		msg = strings.ReplaceAll(msg, e, "")
	}

	// Clean up multiple spaces
	for strings.Contains(msg, "  ") {
		msg = strings.ReplaceAll(msg, "  ", " ")
	}

	return strings.TrimSpace(msg)
}

// Helper functions for direct logging (optional use)

// Info logs an info message
func Info(format string, args ...interface{}) {
	log.Printf(format, args...)
}

// Success logs a success message
func Success(format string, args ...interface{}) {
	log.Printf("✅ "+format, args...)
}

// Warn logs a warning message
func Warn(format string, args ...interface{}) {
	log.Printf("⚠️ "+format, args...)
}

// Error logs an error message
func Error(format string, args ...interface{}) {
	log.Printf("❌ ERROR: "+format, args...)
}

// Fatal logs a fatal message and exits
func Fatal(format string, args ...interface{}) {
	log.Fatalf("FATAL: "+format, args...)
}

// Startup prints a fancy startup banner
func Startup(botName, version string) {
	Init()
	fmt.Println()
	fmt.Printf("%s╔══════════════════════════════════════════════════════════╗%s\n", Cyan, Reset)
	fmt.Printf("%s║%s  %s%-56s%s  %s║%s\n", Cyan, Reset, BoldWhite, botName, Reset, Cyan, Reset)
	fmt.Printf("%s║%s  %s%-56s%s  %s║%s\n", Cyan, Reset, Gray, "Version: "+version, Reset, Cyan, Reset)
	fmt.Printf("%s╚══════════════════════════════════════════════════════════╝%s\n", Cyan, Reset)
	fmt.Println()
}

// Section prints a section header
func Section(title string) {
	fmt.Printf("\n%s═══════════════════════════════════════════════════════════%s\n", Cyan, Reset)
	fmt.Printf("%s  %s%s\n", BoldWhite, title, Reset)
	fmt.Printf("%s═══════════════════════════════════════════════════════════%s\n\n", Cyan, Reset)
}
