//go:build windows

package windowsapp

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// installLog persists the installer transcript while forwarding every event to
// the live setup UI. It is intentionally separate from service.log so a
// failure before the service starts still leaves evidence.
type installLog struct {
	mu   sync.Mutex
	ui   setupUI
	file *os.File
	path string
}

func newInstallLog(ui setupUI) *installLog {
	logger := &installLog{ui: ui}
	path := filepath.Join(dataDir(), "install.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		path = filepath.Join(os.TempDir(), "PlaygroundControl-install.log")
	}
	if info, err := os.Stat(path); err == nil && info.Size() > 2<<20 {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	logger.open(path)
	logger.write("INFO", fmt.Sprintf("Playground Control %s setup started", setupVersion()))
	return logger
}

func (l *installLog) open(path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		path = filepath.Join(os.TempDir(), "PlaygroundControl-install.log")
		_ = os.MkdirAll(filepath.Dir(path), 0o750)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		path = filepath.Join(os.TempDir(), "PlaygroundControl-install.log")
		file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	}
	if err == nil {
		l.mu.Lock()
		l.file = file
		l.path = path
		l.mu.Unlock()
	}
}

func (l *installLog) write(kind, text string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	line := time.Now().UTC().Format(time.RFC3339) + " [" + kind + "] " + strings.TrimSpace(text) + "\n"
	_, _ = l.file.WriteString(line)
	_ = l.file.Sync()
}

func (l *installLog) StartStep(text string) {
	if l == nil {
		return
	}
	l.write(" .. ", text)
	if l.ui != nil {
		l.ui.StartStep(text)
	}
}

func (l *installLog) CompleteStep(text string) {
	if l == nil {
		return
	}
	l.write(" OK ", text)
	if l.ui != nil {
		l.ui.CompleteStep(text)
	}
}

func (l *installLog) Info(text string) {
	if l == nil {
		return
	}
	l.write("INFO", text)
	if l.ui != nil {
		l.ui.Info(text)
	}
}

func (l *installLog) Failure(text string) {
	if l == nil {
		return
	}
	l.write("FAIL", text)
	if l.ui != nil {
		l.ui.Failure(text)
	}
}

func (l *installLog) ServiceLine(text string) {
	if l == nil {
		return
	}
	l.write("SVC ", text)
	if l.ui != nil {
		l.ui.ServiceLine(text)
	}
}

func (l *installLog) SetStatus(text string) {
	if l == nil {
		return
	}
	if l.ui != nil {
		l.ui.SetStatus(text)
	}
}

func (l *installLog) SetProgress(done, total int) {
	if l == nil {
		return
	}
	if l.ui != nil {
		l.ui.SetProgress(done, total)
	}
}

func (l *installLog) MarkSuccess() {
	if l != nil && l.ui != nil {
		l.ui.MarkSuccess()
	}
}

func (l *installLog) MarkFailure() {
	if l != nil && l.ui != nil {
		l.ui.MarkFailure()
	}
}

func (l *installLog) ClearTerminal() {
	if l != nil && l.ui != nil {
		l.ui.ClearTerminal()
	}
}

func (l *installLog) AskRecovery(reason string) recoveryAction {
	if l == nil || l.ui == nil {
		return recoveryExit
	}
	l.write("INFO", "Recovery choice requested: "+reason)
	return l.ui.AskRecovery(reason)
}

func (l *installLog) ConfirmFullReset() bool {
	if l == nil || l.ui == nil {
		return false
	}
	return l.ui.ConfirmFullReset()
}

func (l *installLog) WaitForFinish() {
	if l != nil && l.ui != nil {
		l.ui.WaitForFinish()
	}
}

func (l *installLog) Path() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.path
}

// Preserve copies the current transcript to a timestamped file before a full
// reset deletes ProgramData, then keeps logging to the original path.
func (l *installLog) Preserve() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	path := l.path
	l.mu.Unlock()
	if path == "" {
		return ""
	}
	if err := os.MkdirAll(os.TempDir(), 0o750); err != nil {
		return ""
	}
	target := filepath.Join(os.TempDir(), "PlaygroundControl-install-"+time.Now().UTC().Format("20060102T150405Z")+".log")
	source, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer source.Close()
	destination, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return ""
	}
	if _, err := io.Copy(destination, source); err != nil {
		_ = destination.Close()
		_ = os.Remove(target)
		return ""
	}
	if err := destination.Close(); err != nil {
		_ = os.Remove(target)
		return ""
	}
	return target
}

func (l *installLog) Close() {
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
}

// Reopen starts a new transcript after a full reset deleted ProgramData and
// the previous log file. The preserved copy remains in %TEMP%.
func (l *installLog) Reopen() {
	if l == nil {
		return
	}
	l.mu.Lock()
	open := l.file == nil
	l.mu.Unlock()
	if !open {
		return
	}
	l.open(filepath.Join(dataDir(), "install.log"))
	l.write("INFO", "Setup continued after a full reset")
}

var _ setupUI = (*installLog)(nil)
