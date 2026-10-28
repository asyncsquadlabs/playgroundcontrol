//go:build windows

package windowsapp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/buildinfo"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/store"
)

// commandDenyLaunch is invoked by Windows through the IFEO Debugger entry. It
// does not start the original image, which is exactly what makes the block
// happen before Roblox reaches process creation. While play is blocked, the
// launch is redirected to a fixed study-guide search instead of a warning.
func commandDenyLaunch() {
	if err := openStudySearch(); err != nil {
		showStudySearchFailure()
	}
}

func commandInteractive() {
	if isInstalled() && serviceHealthy() {
		installed := installedVersion()
		if installed != "" && (strings.EqualFold(installed, buildinfo.Version) || strings.EqualFold(buildinfo.Version, "dev")) {
			finishInstalledFlow()
			return
		}
	}
	if !isElevated() {
		if err := elevateSelf("--install"); err != nil {
			showError("Administrator approval is required to install or repair Playground Control.\n\n" + err.Error())
		}
		return
	}
	commandInstall()
}

func commandInstall() {
	if !isElevated() {
		if err := elevateSelf("--install"); err != nil {
			showError("Administrator approval is required to install Playground Control.\n\n" + err.Error())
		}
		return
	}
	if handled, err := relaunchSetupFromTemp(modeAuto); err != nil {
		showError("Could not start the setup process from a temporary location.\n\n" + err.Error())
		return
	} else if handled {
		return
	}
	outcome, err := runSetupWorkflow(modeAuto)
	if err != nil {
		showError("Setup could not start.\n\n" + err.Error())
		return
	}
	if outcome == outcomeInstalled || outcome == outcomeAlreadyRunning {
		finishInstalledFlow()
	}
}

func commandRepair() {
	if !isElevated() {
		if err := elevateSelf("--repair"); err != nil {
			showError("Administrator approval is required to repair Playground Control.\n\n" + err.Error())
		}
		return
	}
	if handled, err := relaunchSetupFromTemp(modeRepair); err != nil {
		showError("Could not start the repair process from a temporary location.\n\n" + err.Error())
		return
	} else if handled {
		return
	}
	outcome, err := runSetupWorkflow(modeRepair)
	if err != nil {
		showError("Repair could not start.\n\n" + err.Error())
		return
	}
	if outcome == outcomeInstalled || outcome == outcomeAlreadyRunning {
		finishInstalledFlow()
	}
}

func commandReinstall() {
	if !isElevated() {
		if err := elevateSelf("--reinstall"); err != nil {
			showError("Administrator approval is required to reinstall Playground Control.\n\n" + err.Error())
		}
		return
	}
	if handled, err := relaunchSetupFromTemp(modeFullReset); err != nil {
		showError("Could not start the reinstall process from a temporary location.\n\n" + err.Error())
		return
	} else if handled {
		return
	}
	outcome, err := runSetupWorkflow(modeFullReset)
	if err != nil {
		showError("Reinstall could not start.\n\n" + err.Error())
		return
	}
	if outcome == outcomeInstalled || outcome == outcomeAlreadyRunning {
		finishInstalledFlow()
	}
}

func runSetupWorkflow(force installMode) (setupOutcome, error) {
	var outcome setupOutcome
	err := runSetupUI(func(ui setupUI) {
		log := newInstallLog(ui)
		defer log.Close()
		backend := &windowsInstallBackend{log: log}
		outcome = runInstallFlow(backend, log, force)
	})
	if exe, exeErr := selfExe(); exeErr == nil && pathInside(exe, os.TempDir()) && strings.Contains(filepath.Base(exe), "PlaygroundControl-uninstall-") {
		_ = scheduleDeleteOnReboot(exe)
	}
	return outcome, err
}

func relaunchSetupFromTemp(mode installMode) (bool, error) {
	exe, err := selfExe()
	if err != nil {
		return false, err
	}
	if !pathInside(exe, installDir()) {
		return false, nil
	}
	if mode == modeAuto && serviceHealthy() {
		installed := installedVersion()
		if installed != "" && (strings.EqualFold(installed, buildinfo.Version) || strings.EqualFold(buildinfo.Version, "dev")) {
			return false, nil
		}
	}
	tempExe, err := copySelfToTemp()
	if err != nil {
		return false, fmt.Errorf("copy setup executable: %w", err)
	}
	arg := "--install"
	switch mode {
	case modeRepair:
		arg = "--repair"
	case modeFullReset:
		arg = "--reinstall"
	}
	cmd := exec.Command(tempExe, arg)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(tempExe)
		return false, fmt.Errorf("start temporary setup executable: %w", err)
	}
	return true, nil
}

func commandStart() {
	if !isElevated() {
		if err := elevateSelf("--start"); err != nil {
			showError("Administrator approval is required to start the service.\n\n" + err.Error())
		}
		return
	}
	if !isInstalled() {
		showError("Playground Control is not installed. Run the installer again.")
		return
	}
	exists, err := serviceExists()
	if err != nil {
		showError("Could not inspect the Playground Control service.\n\n" + err.Error())
		return
	}
	if !exists {
		commandInstall()
		return
	} else {
		if err := startService(); err != nil {
			showError("Could not start the Playground Control service.\n\n" + err.Error())
			return
		}
		if err := waitForHealth(20 * time.Second); err != nil {
			showError("The service started but the dashboard did not respond.\n\n" + err.Error())
			return
		}
	}
	finishInstalledFlow()
}

func finishInstalledFlow() {
	info, err := fetchBootstrap()
	if err != nil {
		showError("The Playground Control service is not ready.\n\n" + err.Error())
		return
	}
	if info.SetupRequired {
		if !isElevated() {
			if err := elevateSelf("--setup"); err != nil {
				showError("Administrator approval is required to create the first setup link.\n\n" + err.Error())
			}
			return
		}
		if err := resetPINAndOpenSetup(false); err != nil {
			showError("The setup page could not be prepared.\n\n" + err.Error())
		}
		return
	}
	if err := openURL("http://127.0.0.1:8080/"); err != nil {
		showError("Could not open the dashboard in the default browser.\n\nOpen http://127.0.0.1:8080/ manually.\n\n" + err.Error())
	}
}

func commandSetup() {
	if !isElevated() {
		if err := elevateSelf("--setup"); err != nil {
			showError("Administrator approval is required to create a setup link.\n\n" + err.Error())
		}
		return
	}
	if !isInstalled() {
		showError("Playground Control is not installed. Run the installer first.")
		return
	}
	if !serviceHealthy() {
		if err := startService(); err != nil {
			showError("Could not start the Playground Control service.\n\n" + err.Error())
			return
		}
		if err := waitForHealth(20 * time.Second); err != nil {
			showError("The service did not become ready.\n\n" + err.Error())
			return
		}
	}
	info, err := fetchBootstrap()
	if err != nil {
		showError("Could not read the service setup state.\n\n" + err.Error())
		return
	}
	if !info.SetupRequired {
		showInfo("The administrator PIN is already configured.\n\nUse --reset-admin if you need to reset a forgotten PIN.")
		if err := openURL("http://127.0.0.1:8080/"); err != nil {
			showError("Could not open the dashboard in the default browser.\n\nOpen http://127.0.0.1:8080/ manually.")
		}
		return
	}
	if err := resetPINAndOpenSetup(false); err != nil {
		showError("The setup page could not be prepared.\n\n" + err.Error())
	}
}

func commandResetAdmin() {
	if !isElevated() {
		if err := elevateSelf("--reset-admin"); err != nil {
			showError("Administrator approval is required to reset the administrator PIN.\n\n" + err.Error())
		}
		return
	}
	if !isInstalled() {
		showError("Playground Control is not installed.")
		return
	}
	if !confirm("Reset administrator PIN?", "Reset the Playground Control administrator PIN?\n\nThe game list, play permission, and activity history are preserved. You will create a new PIN on the local setup page. Anyone with access to this Windows administrator session can reset the PIN.") {
		return
	}
	if err := resetPINAndOpenSetup(true); err != nil {
		showError("The administrator PIN could not be reset.\n\n" + err.Error())
	}
}

func resetPINAndOpenSetup(resetAdmin bool) error {
	exists, err := serviceExists()
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("the Playground Control service is not registered; reinstall the application")
	}
	if err := stopService(); err != nil {
		return fmt.Errorf("stop service before reset: %w", err)
	}
	if err := ensurePrivateDataDir(); err != nil {
		return fmt.Errorf("secure data directory before reset: %w", err)
	}
	st, err := store.Open(dataDir(), time.Now)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	if err := st.ClearAuthRecord(); err != nil {
		_ = st.Close()
		return fmt.Errorf("remove current PIN: %w", err)
	}
	if err := os.Remove(setupTokenPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = st.Close()
		return fmt.Errorf("remove stale setup token: %w", err)
	}
	message := "A fresh first-run setup link was requested locally."
	if resetAdmin {
		message = "Administrator PIN was reset by a local administrator."
	}
	_ = st.AddEvent(model.AuditEvent{
		Type:    model.EventAuth,
		Message: message,
		Result:  "reset",
	})
	if err := st.Close(); err != nil {
		return fmt.Errorf("close database after reset: %w", err)
	}
	if err := startService(); err != nil {
		return fmt.Errorf("start service after reset: %w", err)
	}
	if err := waitForHealth(20 * time.Second); err != nil {
		return err
	}
	token, err := readSetupToken(15 * time.Second)
	if err != nil {
		return err
	}
	target := setupURL(token)
	if err := openURL(target); err != nil {
		showInfo("The local setup page is ready, but the browser could not be opened automatically.\n\nOpen this local URL:\n" + target)
		return nil
	}
	return nil
}

func commandUninstall() {
	if !isElevated() {
		if err := elevateSelf("--uninstall"); err != nil {
			showError("Administrator approval is required to uninstall Playground Control.\n\n" + err.Error())
		}
		return
	}
	if !confirm("Uninstall Playground Control?", "Uninstall Playground Control?\n\nThis removes the Windows service, firewall rule, administrator PIN, game list, and activity history. The current play permission is also removed.") {
		return
	}

	exe, err := selfExe()
	if err != nil {
		showError("Could not locate the uninstaller.\n\n" + err.Error())
		return
	}
	if pathInside(exe, installDir()) {
		cleanupExe, copyErr := copySelfToTemp()
		if copyErr == nil {
			cmd := exec.Command(cleanupExe, "--uninstall-cleanup")
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
			if startErr := cmd.Start(); startErr == nil {
				return
			}
			_ = os.Remove(cleanupExe)
		}
		// Fall back to immediate removal and a reboot-time delete for the
		// executable that is currently running.
		removeErr := performUninstall()
		_ = removeInstallFiles()
		_ = scheduleDeleteOnReboot(exe)
		if removeErr != nil {
			showError("Playground Control was only partially removed. Restart Windows, then remove the application again if necessary.\n\n" + removeErr.Error())
			return
		}
		showInfo("Playground Control has been removed. Restart Windows to delete the last executable file.")
		return
	}

	if err := performUninstall(); err != nil {
		showError("Playground Control was only partially removed. Restart Windows and review the remaining files and service.\n\n" + err.Error())
		return
	}
	if err := removeInstallFiles(); err != nil {
		showError("The service and settings were removed, but application files remain.\n\n" + err.Error())
		return
	}
	showInfo("Playground Control has been removed.")
}

func commandUninstallCleanup() {
	if !isElevated() {
		return
	}
	// Give the originally invoked installed EXE time to exit so its directory
	// is no longer locked.
	time.Sleep(1 * time.Second)
	errs := []error{}
	if err := performUninstall(); err != nil {
		errs = append(errs, err)
	}
	if err := removeInstallFiles(); err != nil {
		errs = append(errs, err)
	}
	if exe, err := selfExe(); err == nil {
		_ = scheduleDeleteOnReboot(exe)
	}
	if err := errors.Join(errs...); err != nil {
		showError("Playground Control could not be fully removed. Restart Windows, then review remaining files and the service.\n\n" + err.Error())
		return
	}
	showInfo("Playground Control has been removed.")
}

func copySelfToTemp() (string, error) {
	source, err := selfExe()
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "PlaygroundControl-uninstall-*.exe")
	if err != nil {
		return "", err
	}
	target := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(target)
		return "", err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		os.Remove(target)
		return "", err
	}
	if err := os.WriteFile(target, data, 0o755); err != nil {
		os.Remove(target)
		return "", err
	}
	return target, nil
}

func pathInside(path, dir string) bool {
	relative, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}
