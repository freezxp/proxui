// Package logging builds the process-wide structured logger.
package logging

import (
	"os"
	"time"

	"github.com/rs/zerolog"
)

// New returns a logger writing structured JSON to stdout (or human-readable
// console output in development).
func New(level, format, version string) zerolog.Logger {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil || lvl == zerolog.NoLevel {
		lvl = zerolog.InfoLevel
	}
	zerolog.TimeFieldFormat = time.RFC3339Nano

	var logger zerolog.Logger
	if format == "console" {
		logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: "15:04:05"})
	} else {
		logger = zerolog.New(os.Stdout)
	}
	return logger.Level(lvl).With().
		Timestamp().
		Str("service", "proxui").
		Str("version", version).
		Logger()
}
