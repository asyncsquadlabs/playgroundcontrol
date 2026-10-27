//go:build windows

package platform

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/enforce"
)

func TestSourceHelperProcess(t *testing.T) {
	if os.Getenv("PLAYGROUND_SOURCE_HELPER") != "1" {
		return
	}
	time.Sleep(60 * time.Second)
}

func TestSourceListsAndKillsProcessWithIdentityCheck(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestSourceHelperProcess", "-test.count=1")
	cmd.Env = append(os.Environ(), "PLAYGROUND_SOURCE_HELPER=1")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	source, err := NewSource()
	if err != nil {
		t.Fatal(err)
	}
	var target enforce.Process
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		processes, err := source.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, process := range processes {
			if process.PID == uint32(cmd.Process.Pid) {
				target = process
				break
			}
		}
		if target.PID != 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if target.PID == 0 {
		t.Fatal("child process was not found")
	}
	if target.CreatedAt.IsZero() {
		t.Fatal("process creation time was not populated")
	}
	if target.ImagePath == "" {
		t.Fatal("process image path was not populated")
	}

	wrongIdentity := target
	wrongIdentity.CreatedAt = wrongIdentity.CreatedAt.Add(time.Second)
	if err := source.Kill(wrongIdentity); !errors.Is(err, enforce.ErrProcessGone) {
		t.Fatalf("wrong identity kill error = %v", err)
	}
	if err := source.Kill(target); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case <-waitDone:
	case <-time.After(10 * time.Second):
		t.Fatal("child process did not exit after Kill")
	}
}

func TestRunCloseHelperClosesGUIProcess(t *testing.T) {
	cmd := exec.Command("notepad.exe")
	if err := cmd.Start(); err != nil {
		t.Skipf("notepad is unavailable: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	var lastErr error
	closeDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(closeDeadline) {
		lastErr = RunCloseHelper([]uint32{uint32(cmd.Process.Pid)})
		if lastErr == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("RunCloseHelper: %v", lastErr)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case <-waitDone:
	case <-time.After(10 * time.Second):
		t.Fatal("GUI process did not exit after the close request")
	}
}
