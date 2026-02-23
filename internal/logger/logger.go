// Package logger provides a thin structured logging wrapper around log/slog.
// All Lambda functions call Init at startup to attach component and region
// labels to every log line. CloudWatch Logs Insights can then filter and
// aggregate by these fields.
package logger

import (
	"log/slog"
	"os"
)

// defaultLogger is initialized with a plain JSON handler so callers that
// forget to call Init still produce valid (though unlabeled) JSON output.
var defaultLogger = slog.New(slog.NewJSONHandler(os.Stdout, nil))

// Init configures the package-level logger with component and region labels
// attached to every subsequent log entry. Call once at Lambda startup.
func Init(component, region string) {
	defaultLogger = slog.New(slog.NewJSONHandler(os.Stdout, nil)).
		With("component", component, "region", region)
}

// Info logs a message at INFO level with optional key-value pairs.
func Info(msg string, args ...any) { defaultLogger.Info(msg, args...) }

// Warn logs a message at WARN level with optional key-value pairs.
func Warn(msg string, args ...any) { defaultLogger.Warn(msg, args...) }

// Error logs a message at ERROR level with optional key-value pairs.
func Error(msg string, args ...any) { defaultLogger.Error(msg, args...) }
