package windowsapp

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

const maxProgressLines = 500

// progressSnapshot is the immutable view consumed by the setup window.
type progressSnapshot struct {
	Version        string
	Lines          []string
	Status         string
	Done           int
	Total          int
	Terminal       bool
	Success        bool
	AwaitingChoice bool
}

// progressModel is the platform-independent state behind the installer UI.
// It is safe for concurrent use by the installer worker and the window thread.
type progressModel struct {
	mu       sync.Mutex
	version  string
	lines    []string
	status   string
	done     int
	total    int
	terminal bool
	success  bool
	awaiting bool
}

func newProgressModel(version string) *progressModel {
	return &progressModel{version: version}
}

func (m *progressModel) append(kind, text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	line := fmt.Sprintf("%s [%s] %s", time.Now().Format("15:04:05"), kind, text)
	m.lines = append(m.lines, line)
	if len(m.lines) > maxProgressLines {
		m.lines = append([]string(nil), m.lines[len(m.lines)-maxProgressLines:]...)
	}
}

func (m *progressModel) StartStep(text string) {
	m.append(" .. ", text)
	m.mu.Lock()
	m.status = text
	m.mu.Unlock()
}

func (m *progressModel) CompleteStep(text string) {
	m.append(" OK ", text)
	m.mu.Lock()
	m.status = text
	m.mu.Unlock()
}

func (m *progressModel) Info(text string) {
	m.append("INFO", text)
}

func (m *progressModel) Failure(text string) {
	m.append("FAIL", text)
	m.mu.Lock()
	m.status = text
	m.mu.Unlock()
}

func (m *progressModel) ServiceLine(text string) {
	m.append("SVC ", strings.TrimSpace(text))
}

func (m *progressModel) SetStatus(text string) {
	m.mu.Lock()
	m.status = text
	m.mu.Unlock()
}

func (m *progressModel) SetProgress(done, total int) {
	if done < 0 {
		done = 0
	}
	if total < 0 {
		total = 0
	}
	if done > total {
		done = total
	}
	m.mu.Lock()
	m.done = done
	m.total = total
	m.mu.Unlock()
}

func (m *progressModel) MarkSuccess() {
	m.mu.Lock()
	m.terminal = true
	m.success = true
	m.awaiting = false
	m.mu.Unlock()
}

func (m *progressModel) MarkFailure() {
	m.mu.Lock()
	m.terminal = true
	m.success = false
	m.awaiting = false
	m.mu.Unlock()
}

func (m *progressModel) MarkAwaitingChoice() {
	m.mu.Lock()
	m.terminal = true
	m.success = false
	m.awaiting = true
	m.mu.Unlock()
}

func (m *progressModel) ClearTerminal() {
	m.mu.Lock()
	m.terminal = false
	m.success = false
	m.awaiting = false
	m.mu.Unlock()
}

func (m *progressModel) Snapshot() progressSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	lines := append([]string(nil), m.lines...)
	return progressSnapshot{
		Version:        m.version,
		Lines:          lines,
		Status:         m.status,
		Done:           m.done,
		Total:          m.total,
		Terminal:       m.terminal,
		Success:        m.success,
		AwaitingChoice: m.awaiting,
	}
}

// installReporter is implemented by the native setup window and by the
// logging wrapper that persists the transcript.
type installReporter interface {
	StartStep(text string)
	CompleteStep(text string)
	Info(text string)
	Failure(text string)
	ServiceLine(text string)
	SetStatus(text string)
	SetProgress(done, total int)
}

type noopReporter struct{}

func (noopReporter) StartStep(string)    {}
func (noopReporter) CompleteStep(string) {}
func (noopReporter) Info(string)         {}
func (noopReporter) Failure(string)      {}
func (noopReporter) ServiceLine(string)  {}
func (noopReporter) SetStatus(string)    {}
func (noopReporter) SetProgress(int, int) {
}
