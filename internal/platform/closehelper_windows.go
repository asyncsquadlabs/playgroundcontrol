//go:build windows

package platform

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/windows"
)

const wmClose = 0x0010

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	procPostMessageW = user32.NewProc("PostMessageW")
)

// RunCloseHelper is executed inside the interactive user session. It posts a
// normal close request to each visible top-level window owned by the requested
// processes. It deliberately has no privileged control functionality.
func RunCloseHelper(pids []uint32) error {
	if len(pids) == 0 {
		return errors.New("no process IDs supplied")
	}
	wanted := make(map[uint32]struct{}, len(pids))
	for _, pid := range pids {
		wanted[pid] = struct{}{}
	}

	posted := 0
	callback := syscall.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
		var pid uint32
		if _, err := windows.GetWindowThreadProcessId(hwnd, &pid); err != nil {
			return 1
		}
		if _, ok := wanted[pid]; !ok {
			return 1
		}
		if !windows.IsWindowVisible(hwnd) {
			return 1
		}
		result, _, _ := procPostMessageW.Call(uintptr(hwnd), uintptr(wmClose), 0, 0)
		if result != 0 {
			posted++
		}
		return 1
	})

	if err := windows.EnumWindows(callback, nil); err != nil {
		return fmt.Errorf("enumerate game windows: %w", err)
	}
	if posted == 0 {
		return errors.New("no visible top-level window accepted a close request")
	}
	return nil
}
