package app

import (
	"context"
	"log"
	"time"
)

// UnifiedLogScheduler runs all log processing modules
// V9 Style: Each module runs independently without blocking others
type UnifiedLogScheduler struct {
	ctx                    context.Context
	checkInterval          time.Duration
	killfeedInterval       time.Duration
	playersOnlineInterval  time.Duration
}

// NewUnifiedLogScheduler creates a new unified scheduler
func NewUnifiedLogScheduler(ctx context.Context) *UnifiedLogScheduler {
	return &UnifiedLogScheduler{
		ctx:                   ctx,
		checkInterval:         time.Duration(Config.CheckInterval) * time.Second,
		killfeedInterval:      time.Duration(Config.KillfeedCheckInterval) * time.Second,
		playersOnlineInterval: time.Duration(Config.PlayersOnlineCheckInterval) * time.Second,
	}
}

// Start begins all module schedulers independently (V9 style)
func (s *UnifiedLogScheduler) Start() {
	log.Printf("🚀 Starting Log Scheduler (V9 style - independent modules)")
	log.Printf("   Check interval: %v, Killfeed interval: %v", s.checkInterval, s.killfeedInterval)

	// Start file list cache refresher (avoids slow 30+ second SFTP ReadDir)
	if Config.UseRemote {
		StartFileListCacheRefresher(s.ctx)
	}

	// Each module runs in its own goroutine with its own ticker
	// They don't wait for each other - just like V9

	// Killfeed - fastest check
	go s.runKillfeedLoop()

	// Economy - separate fast loop (important for trade logs)
	go s.runEconomyLoop()

	// Logs module (login, kill, gameplay) - economy is now separate
	go s.runLogsLoop()

	// Paste module (economy, vehicle_destruction, chest_ownership, gameplay, violations)
	go s.runPasteLoop()

	// Chat module
	go s.runChatLoop()

	// Lockpicking module
	go s.runLockpickingLoop()

	// Admin module
	go s.runAdminLoop()

	// Players Online module
	if isPlayersOnlineModuleActive() {
		go s.runPlayersOnlineLoop()
	}
}

// runEconomyLoop - economy logs at FAST interval (same as killfeed for quick trade detection)
func (s *UnifiedLogScheduler) runEconomyLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ [ECONOMY] Panic recovered: %v", r)
			time.Sleep(2 * time.Second)
			go s.runEconomyLoop()
		}
	}()

	// Process immediately on start, then use ticker
	processEconomy := func() {
		if Config.UseRemote {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("⚠️ [ECONOMY REMOTE] Panic: %v", r)
					}
				}()
				processLogsRemoteLogs("economy")
			}()
		}
		if Config.UseLocalFiles {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("⚠️ [ECONOMY LOCAL] Panic: %v", r)
					}
				}()
				processLogsLocalLogs("economy")
			}()
		}
	}

	// Run immediately first time
	log.Printf("🚀 [ECONOMY] Starting economy loop with %v interval", s.killfeedInterval)
	processEconomy()

	// Use killfeedInterval for FAST economy checking (same speed as killfeed)
	ticker := time.NewTicker(s.killfeedInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			processEconomy()
		}
	}
}

// runKillfeedLoop - killfeed at faster interval
func (s *UnifiedLogScheduler) runKillfeedLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ [KILLFEED] Panic recovered: %v", r)
			time.Sleep(2 * time.Second)
			go s.runKillfeedLoop()
		}
	}()

	ticker := time.NewTicker(s.killfeedInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if Config.UseRemote {
				processKillFeedRemoteLogs()
			}
			if Config.UseLocalFiles {
				processKillFeedLocalLogs()
			}
		}
	}
}

// runLogsLoop - logs module (login, kill, gameplay) - economy is separate
func (s *UnifiedLogScheduler) runLogsLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ [LOGS] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go s.runLogsLoop()
		}
	}()

	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()

	// Economy is now in separate loop for faster processing
	logTypes := []string{"login", "kill", "gameplay"}

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			UpdateSharedActivity()

			if !GlobalResourceManager.CheckMemory() {
				log.Printf("⚠️ Skipping logs check due to high memory usage")
				continue
			}

			for _, logType := range logTypes {
				if logType == "kill" && Config.KillCountChannelID == "" {
					continue
				}

				if Config.UseRemote {
					func() {
						defer func() {
							if r := recover(); r != nil {
								log.Printf("⚠️ [LOGS %s] Panic: %v", logType, r)
							}
						}()
						processLogsRemoteLogs(logType)
					}()
				}
				if Config.UseLocalFiles {
					func() {
						defer func() {
							if r := recover(); r != nil {
								log.Printf("⚠️ [LOGS LOCAL %s] Panic: %v", logType, r)
							}
						}()
						processLogsLocalLogs(logType)
					}()
				}
			}
		}
	}
}

// runPasteLoop - paste module (economy, vehicle_destruction, etc.)
func (s *UnifiedLogScheduler) runPasteLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ [PASTE] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go s.runPasteLoop()
		}
	}()

	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()

	pasteTypes := []string{"economy", "vehicle_destruction", "chest_ownership", "gameplay", "violations"}

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if !GlobalResourceManager.CheckMemory() {
				continue
			}

			for _, logType := range pasteTypes {
				if Config.UseRemote {
					func() {
						defer func() {
							if r := recover(); r != nil {
								log.Printf("⚠️ [PASTE %s] Panic: %v", logType, r)
							}
						}()
						processPasteRemoteLogs(logType)
					}()
				}
				if Config.UseLocalFiles {
					func() {
						defer func() {
							if r := recover(); r != nil {
								log.Printf("⚠️ [PASTE LOCAL %s] Panic: %v", logType, r)
							}
						}()
						processPasteLocalLogs(logType)
					}()
				}
			}
		}
	}
}

// runChatLoop - chat module
func (s *UnifiedLogScheduler) runChatLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ [CHAT] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go s.runChatLoop()
		}
	}()

	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if Config.UseRemote {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("⚠️ [CHAT REMOTE] Panic: %v", r)
						}
					}()
					processChatRemoteLogs()
				}()
			}
			if Config.UseLocalFiles {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("⚠️ [CHAT LOCAL] Panic: %v", r)
						}
					}()
					processChatLocalLogs()
				}()
			}
		}
	}
}

// runLockpickingLoop - lockpicking module
func (s *UnifiedLogScheduler) runLockpickingLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ [LOCKPICKING] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go s.runLockpickingLoop()
		}
	}()

	// Start ranking system (runs in background)
	go startLockpickingRankingSystem(s.ctx)

	// Start weekly auto-reset scheduler
	go startLockpickingWeeklyReset(s.ctx)

	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if Config.UseRemote {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("⚠️ [LOCKPICKING REMOTE] Panic: %v", r)
						}
					}()
					processLockpickingRemoteLogs()
				}()
			}
			if Config.UseLocalFiles {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("⚠️ [LOCKPICKING LOCAL] Panic: %v", r)
						}
					}()
					processLockpickingLocalLogs()
				}()
			}
		}
	}
}

// runAdminLoop - admin module
func (s *UnifiedLogScheduler) runAdminLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ [ADMIN] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go s.runAdminLoop()
		}
	}()

	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if Config.UseRemote {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("⚠️ [ADMIN REMOTE] Panic: %v", r)
						}
					}()
					processAdminRemoteLogs()
				}()
			}
			if Config.UseLocalFiles {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("⚠️ [ADMIN LOCAL] Panic: %v", r)
						}
					}()
					processAdminLocalLogs()
				}()
			}
		}
	}
}

// runPlayersOnlineLoop - players online module with dedicated interval
func (s *UnifiedLogScheduler) runPlayersOnlineLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ [PLAYERS ONLINE] Panic recovered: %v", r)
			time.Sleep(5 * time.Second)
			go s.runPlayersOnlineLoop()
		}
	}()

	log.Printf("🚀 [PLAYERS ONLINE] Starting loop with %v interval", s.playersOnlineInterval)

	// Run immediately first time
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("⚠️ [PLAYERS ONLINE] Initial run panic: %v", r)
			}
		}()
		processPlayersOnline()
	}()

	ticker := time.NewTicker(s.playersOnlineInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("⚠️ [PLAYERS ONLINE] Panic: %v", r)
					}
				}()
				processPlayersOnline()
			}()
		}
	}
}
