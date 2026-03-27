package app

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ==========================================
// TRAP LOG PARSER
// ==========================================
// Parse [LogTrap] entries and send to Discord channel

// TrapLog represents a parsed trap log entry
type TrapLog struct {
	Date         time.Time `json:"date"`
	Action       string    `json:"action"` // "Armed" or "Triggered"
	User         string    `json:"user"`
	UserID       string    `json:"user_id"`
	SteamID      string    `json:"steam_id"`
	TrapName     string    `json:"trap_name"`
	Owner        string    `json:"owner,omitempty"`         // Only for Triggered
	OwnerID      string    `json:"owner_id,omitempty"`      // Only for Triggered
	OwnerSteamID string    `json:"owner_steam_id,omitempty"` // Only for Triggered
	Location     Location  `json:"location"`
}

// ParseTrapLog parses a trap log line
func ParseTrapLog(line string) *TrapLog {
	if line == "" || !strings.Contains(line, "[LogTrap]") {
		return nil
	}

	// Pattern for Armed: [LogTrap] Armed. User: USERNAME (ID, STEAMID). Trap name: TRAPNAME. Location: X=... Y=... Z=...
	armedPattern := `(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): \[LogTrap\] Armed\. User: (.+?) \((\d+), (\d+)\)\. Trap name: (.+?)\. Location: X=([\d.-]+) Y=([\d.-]+) Z=([\d.-]+)`

	// Pattern for Triggered: [LogTrap] Triggered. User: USERNAME (ID, STEAMID). Trap name: TRAPNAME. Owner: OWNERNAME (ID, STEAMID). Location: X=... Y=... Z=...
	triggeredPattern := `(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}): \[LogTrap\] Triggered\. User: (.+?) \((\d+), (\d+)\)\. Trap name: (.+?)\. Owner: (.+?) \((\d+), (\d+)\)\. Location: X=([\d.-]+) Y=([\d.-]+) Z=([\d.-]+)`

	// Try Armed pattern first
	re := regexp.MustCompile(armedPattern)
	matches := re.FindStringSubmatch(line)

	if len(matches) >= 9 {
		date, err := time.Parse("2006.01.02-15.04.05", matches[1])
		if err != nil {
			return nil
		}

		x, _ := strconv.ParseFloat(matches[6], 64)
		y, _ := strconv.ParseFloat(matches[7], 64)
		z, _ := strconv.ParseFloat(matches[8], 64)

		return &TrapLog{
			Date:     date.UTC(),
			Action:   "Armed",
			User:     strings.TrimSpace(matches[2]),
			UserID:   strings.TrimSpace(matches[3]),
			SteamID:  strings.TrimSpace(matches[4]),
			TrapName: strings.TrimSpace(matches[5]),
			Location: Location{X: x, Y: y, Z: z},
		}
	}

	// Try Triggered pattern
	re = regexp.MustCompile(triggeredPattern)
	matches = re.FindStringSubmatch(line)

	if len(matches) >= 12 {
		date, err := time.Parse("2006.01.02-15.04.05", matches[1])
		if err != nil {
			return nil
		}

		x, _ := strconv.ParseFloat(matches[9], 64)
		y, _ := strconv.ParseFloat(matches[10], 64)
		z, _ := strconv.ParseFloat(matches[11], 64)

		return &TrapLog{
			Date:         date.UTC(),
			Action:       "Triggered",
			User:         strings.TrimSpace(matches[2]),
			UserID:       strings.TrimSpace(matches[3]),
			SteamID:      strings.TrimSpace(matches[4]),
			TrapName:     strings.TrimSpace(matches[5]),
			Owner:        strings.TrimSpace(matches[6]),
			OwnerID:      strings.TrimSpace(matches[7]),
			OwnerSteamID: strings.TrimSpace(matches[8]),
			Location:     Location{X: x, Y: y, Z: z},
		}
	}

	return nil
}

// sendTrapLog sends trap log to Discord channel
func sendTrapLog(info *TrapLog) error {
	if info == nil || Config.TrapChannelID == "" {
		return nil
	}

	var msg string
	if info.Action == "Armed" {
		// Armed trap - green diff
		msg = fmt.Sprintf("```diff\n+ [TRAP ARMED]\nUser: %s (%s)\nTrap: %s\nLocation: X=%.3f Y=%.3f Z=%.3f\n```",
			info.User, info.SteamID, info.TrapName,
			info.Location.X, info.Location.Y, info.Location.Z)
	} else {
		// Triggered trap - red diff
		msg = fmt.Sprintf("```diff\n- [TRAP TRIGGERED]\nTriggered by: %s (%s)\nTrap: %s\nOwner: %s (%s)\nLocation: X=%.3f Y=%.3f Z=%.3f\n```",
			info.User, info.SteamID, info.TrapName,
			info.Owner, info.OwnerSteamID,
			info.Location.X, info.Location.Y, info.Location.Z)
	}

	_, err := SharedSession.ChannelMessageSend(Config.TrapChannelID, msg)
	if err != nil {
		log.Printf("ERROR: Failed to send trap log to Discord: %v", err)
		return err
	}

	log.Printf("✅ Sent trap log: %s by %s", info.Action, info.User)
	UpdateSharedActivity()
	time.Sleep(100 * time.Millisecond)
	return nil
}

// ProcessTrapLogLine processes a single trap log line and sends to Discord
func ProcessTrapLogLine(line string) {
	trapLog := ParseTrapLog(line)
	if trapLog == nil {
		return
	}

	if err := sendTrapLog(trapLog); err != nil {
		log.Printf("Error sending trap log: %v", err)
	}
}

// processTrapLines processes multiple trap log lines
func processTrapLines(lines []string) {
	log.Printf("DEBUG: Processing %d lines for trap logs", len(lines))

	parsedCount := 0
	cachedCount := 0

	for _, line := range lines {
		if !strings.Contains(line, "[LogTrap]") {
			continue
		}

		logKey := line + "_trap"
		if !IsLogSentCached(logKey) {
			info := ParseTrapLog(line)
			if info != nil {
				parsedCount++
				if err := sendTrapLog(info); err != nil {
					log.Printf("Error sending trap log: %v", err)
				} else {
					MarkLogAsSentCached(logKey)
				}
			}
		} else {
			cachedCount++
		}
	}

	if parsedCount > 0 || cachedCount > 0 {
		log.Printf("DEBUG: Parsed %d new trap logs, %d cached", parsedCount, cachedCount)
	}
}
