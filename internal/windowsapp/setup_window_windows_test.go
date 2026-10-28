//go:build windows

package windowsapp

import (
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

var procTestGetDlgItem = windows.NewLazySystemDLL("user32.dll").NewProc("GetDlgItem")

func TestSetupWindowButtonActions(t *testing.T) {
	window := &setupWindow{action: make(chan recoveryAction, 1)}
	window.handleCommand(idRepair, bnClicked)
	if action := <-window.action; action != recoveryRepair {
		t.Fatalf("repair action = %v", action)
	}
	window.handleCommand(idFullReset, bnClicked)
	if action := <-window.action; action != recoveryReset {
		t.Fatalf("full reset action = %v", action)
	}
}

func TestSetupWindowControlsHaveCommandIDs(t *testing.T) {
	// The window and message loop must live on one OS thread. This test only
	// creates and destroys the window, but the thread affinity still matters
	// for DestroyWindow.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	window, err := newSetupWindow()
	if err != nil {
		t.Fatal(err)
	}
	defer procDestroyWindow.Call(uintptr(window.hwnd))

	expected := []struct {
		id   uintptr
		hwnd windows.HWND
	}{
		{idCopyDetails, window.copy},
		{idRepair, window.repair},
		{idFullReset, window.reset},
		{idExit, window.exit},
		{idFinish, window.finish},
	}
	for _, item := range expected {
		got, _, _ := procTestGetDlgItem.Call(uintptr(window.hwnd), item.id)
		if windows.HWND(got) != item.hwnd {
			t.Fatalf("control ID %d points to %#x, want %#x", item.id, got, item.hwnd)
		}
	}
}
