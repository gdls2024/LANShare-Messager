package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Log is the global structured logger instance.
var Log *slog.Logger

// logLevelVar allows changing the log level at runtime.
var logLevelVar slog.LevelVar

// InitLogger initializes the global structured logger.
// level: "error" (default), "info", or "debug".
// Returns the log file handle (caller should defer Close) and any error.
func InitLogger(level string) (*os.File, error) {
	setLogLevelVar(level)

	// Log file: ~/.lanshare/logs/lanshare_YYYYMMDD.log
	logDir := DataPath("logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, err
	}
	logFile := filepath.Join(logDir, "lanshare_"+time.Now().Format("20060102_150405")+".log")

	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}

	Log = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: &logLevelVar}))
	return f, nil
}

// SetLogLevel changes the log level at runtime without restarting.
func SetLogLevel(level string) {
	setLogLevelVar(level)
}

// GetLogLevel returns the current log level as a string.
func GetLogLevel() string {
	switch logLevelVar.Level() {
	case slog.LevelDebug:
		return "debug"
	case slog.LevelInfo:
		return "info"
	default:
		return "error"
	}
}

// LogDir returns the path to the log directory.
func LogDir() string {
	return DataPath("logs")
}

func setLogLevelVar(level string) {
	switch strings.ToLower(level) {
	case "debug":
		logLevelVar.Set(slog.LevelDebug)
	case "info":
		logLevelVar.Set(slog.LevelInfo)
	default:
		logLevelVar.Set(slog.LevelError)
	}
}
