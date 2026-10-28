//go:build windows

package windowsapp

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

type scriptedQuerier struct {
	statuses []svc.Status
	index    int
}

func (q *scriptedQuerier) QueryStatus() (svc.Status, error) {
	if len(q.statuses) == 0 {
		return svc.Status{}, nil
	}
	if q.index >= len(q.statuses) {
		return q.statuses[len(q.statuses)-1], nil
	}
	status := q.statuses[q.index]
	q.index++
	return status, nil
}

func TestWaitForStateRunning(t *testing.T) {
	query := &scriptedQuerier{statuses: []svc.Status{
		{State: svc.StartPending, CheckPoint: startupPhaseDatabase},
		{State: svc.StartPending, CheckPoint: startupPhaseGuard},
		{State: svc.Running},
	}}
	var checkpoints []uint32
	err := waitForState(query, svc.Running, time.Second, func(status svc.Status) {
		if status.CheckPoint != 0 {
			checkpoints = append(checkpoints, status.CheckPoint)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 2 {
		t.Fatalf("checkpoints = %v, want two progress updates", checkpoints)
	}
}

func TestWaitForStateStoppedReturnsExitCode(t *testing.T) {
	query := &scriptedQuerier{statuses: []svc.Status{
		{State: svc.StartPending, CheckPoint: startupPhaseDatabase},
		{State: svc.Stopped, Win32ExitCode: 1066, ServiceSpecificExitCode: 1},
	}}
	err := waitForState(query, svc.Running, time.Second, nil)
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, fragment := range []string{"stopped before reaching Running", "1066", "service-specific error 1"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error %q does not contain %q", err, fragment)
		}
	}
}

func TestWaitForStateTimeoutReportsStateAndCheckpoint(t *testing.T) {
	query := &scriptedQuerier{statuses: []svc.Status{
		{State: svc.StartPending, CheckPoint: startupPhaseGuard},
	}}
	err := waitForState(query, svc.Running, 10*time.Millisecond, nil)
	if err == nil {
		t.Fatal("expected timeout")
	}
	for _, fragment := range []string{"did not reach Running", "StartPending", "checkpoint 4"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error %q does not contain %q", err, fragment)
		}
	}
}
