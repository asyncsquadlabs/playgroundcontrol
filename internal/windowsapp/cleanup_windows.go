//go:build windows

package windowsapp

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/eventlog"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/platform"
)

type windowsCleanupOps struct{}

func (windowsCleanupOps) StopService() error {
	return stopService()
}

func (windowsCleanupOps) DeleteService() error {
	return deleteService()
}

func (windowsCleanupOps) RemoveRobloxGuard() error {
	return platform.RemoveRobloxGuard()
}

func (windowsCleanupOps) RemoveFirewallRule() error {
	return removeFirewallRule()
}

func (windowsCleanupOps) RemoveUninstallEntry() error {
	return removeUninstallEntry()
}

func (windowsCleanupOps) RemoveEventLogSource() error {
	err := eventlog.Remove(serviceName)
	if err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return fmt.Errorf("remove Event Log source: %w", err)
	}
	return nil
}

func (windowsCleanupOps) RemoveInstallFiles() error {
	return removeInstallFiles()
}

func (windowsCleanupOps) RemoveDataDirectory() error {
	var lastErr error
	for i := 0; i < 20; i++ {
		if err := os.RemoveAll(dataDir()); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("remove application data: %w", lastErr)
}
