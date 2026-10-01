// Package logger provides the one standard structured-JSON logger construction
// shared by every service: dual output (stdout for 12-factor / container
// log collection, plus a daily-rotated local file for debugging), buffered file
// writes for throughput, and a consistent field encoding so log shippers don't
// need per-service parsing rules.
package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	defaultLogsDir         = "logs"
	defaultLogBufSize      = 64 * 1024
	defaultFlushIntervalMs = 500
	dateFormat             = "2006-01-02"
)

// Config controls logger construction. Zero values fall back to defaults.
type Config struct {
	// ServiceName is stamped by callers via zap.String("service", ...) at each
	// call site (or via New's returned logger.With(...)) — this package does not
	// bake it into the encoder so a single process can log for more than one
	// logical component if needed.
	Dir             string
	BufSizeBytes    int
	FlushIntervalMs int
}

func (c Config) dir() string {
	if c.Dir != "" {
		return c.Dir
	}
	return defaultLogsDir
}

func (c Config) bufSize() int {
	if c.BufSizeBytes > 0 {
		return c.BufSizeBytes
	}
	return defaultLogBufSize
}

func (c Config) flushInterval() time.Duration {
	if c.FlushIntervalMs > 0 {
		return time.Duration(c.FlushIntervalMs) * time.Millisecond
	}
	return time.Duration(defaultFlushIntervalMs) * time.Millisecond
}

// New creates a structured JSON logger that writes to both stdout and a
// daily-rotated file under cfg.Dir. level is a zap level name ("debug",
// "info", "warn", "error"); an unrecognized value defaults to debug.
func New(level string, cfg Config) *zap.Logger {
	zapLevel := parseLevel(level)
	encoderCfg := newEncoderConfig()

	stdoutCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		zapcore.Lock(os.Stdout),
		zapLevel,
	)

	fileWriter := newDailyWriter(cfg)
	fileCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		zapcore.AddSync(fileWriter),
		zapLevel,
	)

	core := zapcore.NewTee(stdoutCore, fileCore)
	return zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))
}

// NewAccess creates a logger for request/access logs (same dual output as
// New), always accepting DebugLevel — callers that build access-log
// middleware (like this module's accesslog package) decide the logical level
// per request, so the underlying core must not filter anything out first.
func NewAccess(cfg Config) *zap.Logger {
	encoderCfg := newEncoderConfig()

	stdoutCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		zapcore.Lock(os.Stdout),
		zapcore.DebugLevel,
	)

	fileWriter := newDailyWriter(cfg)
	fileCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		zapcore.AddSync(fileWriter),
		zapcore.DebugLevel,
	)

	core := zapcore.NewTee(stdoutCore, fileCore)
	return zap.New(core)
}

// LogFilePath returns today's log file path under cfg.Dir, for display purposes.
func LogFilePath(cfg Config) string {
	return filepath.Join(cfg.dir(), fmt.Sprintf("log-%s.log", time.Now().Format(dateFormat)))
}

func parseLevel(level string) zapcore.Level {
	var zapLevel zapcore.Level
	if err := zapLevel.UnmarshalText([]byte(strings.TrimSpace(level))); err != nil {
		zapLevel = zapcore.DebugLevel
	}
	return zapLevel
}

func newEncoderConfig() zapcore.EncoderConfig {
	return zapcore.EncoderConfig{
		TimeKey:        "timestamp",
		LevelKey:       "level",
		MessageKey:     "msg",
		CallerKey:      "caller",
		StacktraceKey:  "stacktrace",
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}
}

// ─── Daily Log Writer ────────────────────────────────────────────────────────

// dailyWriter is an io.Writer that rotates to a new file when the date
// changes, buffering writes with a periodic flush for high throughput.
type dailyWriter struct {
	mu      sync.Mutex
	current *os.File
	today   string
	buf     []byte
	bufSize int
	dir     string
}

func newDailyWriter(cfg Config) *dailyWriter {
	dir := cfg.dir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "[logger] failed to create logs directory: %v (falling back to stdout only)\n", err)
	}

	w := &dailyWriter{
		buf:     make([]byte, 0, cfg.bufSize()),
		bufSize: cfg.bufSize(),
		dir:     dir,
	}
	if err := w.rotate(); err != nil {
		fmt.Fprintf(os.Stderr, "[logger] failed to open initial log file: %v\n", err)
	}

	go w.periodicFlush(cfg.flushInterval())

	return w
}

func (w *dailyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	today := time.Now().Format(dateFormat)
	if today != w.today {
		w.flushLocked()
		if err := w.rotate(); err != nil {
			return os.Stdout.Write(p)
		}
	}

	w.buf = append(w.buf, p...)
	if len(w.buf) >= w.bufSize {
		w.flushLocked()
	}

	return len(p), nil
}

// Sync flushes the buffer and syncs the underlying file.
func (w *dailyWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLocked()
	if w.current != nil {
		return w.current.Sync()
	}
	return nil
}

func (w *dailyWriter) flushLocked() {
	if len(w.buf) == 0 {
		return
	}
	if w.current != nil {
		_, _ = w.current.Write(w.buf)
	}
	w.buf = w.buf[:0]
}

func (w *dailyWriter) periodicFlush(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		w.mu.Lock()
		w.flushLocked()
		w.mu.Unlock()
	}
}

func (w *dailyWriter) rotate() error {
	if w.current != nil {
		_ = w.current.Close()
	}

	w.today = time.Now().Format(dateFormat)
	filename := filepath.Join(w.dir, fmt.Sprintf("log-%s.log", w.today))

	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		w.current = nil
		return err
	}
	w.current = file
	return nil
}
