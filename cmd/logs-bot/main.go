package main

import (
	"github.com/TIMESKIP1337/Logs-version-8.0.0/internal/app"
	"github.com/TIMESKIP1337/Logs-version-8.0.0/internal/logger"
)

func main() {
	// Initialize colored logger with emoji removal
	logger.Init()

	// Run the full application with all modules
	app.Run()
}
