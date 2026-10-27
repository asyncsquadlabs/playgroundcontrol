//go:build windows

package windowsapp

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc/eventlog"
)

type serviceLogger struct {
	mu   sync.Mutex
	ev   *eventlog.Log
	file *os.File
}

func newServiceLogger() *serviceLogger {
	logger := &serviceLogger{}
	if ev, err := eventlog.Open(serviceName); err == nil {
		logger.ev = ev
	}

	if err := os.MkdirAll(dataDir(), 0o750); err == nil {
		path := filepath.Join(dataDir(), "service.log")
		if info, err := os.Stat(path); err == nil && info.Size() > 2<<20 {
			_ = os.Remove(path + ".1")
			_ = os.Rename(path, path+".1")
		}
		if file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
			logger.file = file
		}
	}
	return logger
}

func (l *serviceLogger) Infof(format string, args ...any) {
	l.write("INFO", format, args...)
	if l.ev != nil {
		l.mu.Lock()
		_ = l.ev.Info(1, fmt.Sprintf(format, args...))
		l.mu.Unlock()
	}
}

func (l *serviceLogger) Warningf(format string, args ...any) {
	l.write("WARN", format, args...)
	if l.ev != nil {
		l.mu.Lock()
		_ = l.ev.Warning(2, fmt.Sprintf(format, args...))
		l.mu.Unlock()
	}
}

func (l *serviceLogger) Errorf(format string, args ...any) {
	l.write("ERROR", format, args...)
	if l.ev != nil {
		l.mu.Lock()
		_ = l.ev.Error(3, fmt.Sprintf(format, args...))
		l.mu.Unlock()
	}
}

func (l *serviceLogger) write(level, format string, args ...any) {
	if l == nil || l.file == nil {
		return
	}
	line := time.Now().UTC().Format(time.RFC3339Nano) + " " + level + " " + fmt.Sprintf(format, args...) + "\n"
	l.mu.Lock()
	_, _ = l.file.WriteString(line)
	l.mu.Unlock()
}

func (l *serviceLogger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Sync()
		_ = l.file.Close()
		l.file = nil
	}
	if l.ev != nil {
		_ = l.ev.Close()
		l.ev = nil
	}
}
