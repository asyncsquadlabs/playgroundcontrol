//go:build windows

package windowsapp

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/buildinfo"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/platform"
)

func installApplication() error {
	if !isElevated() {
		return errors.New("administrator elevation is required")
	}
	if !isWindows10OrNewer() {
		return errors.New("Playground Control requires Windows 10 or Windows 11")
	}

	if exists, err := serviceExists(); err != nil {
		return err
	} else if exists {
		return errors.New("Playground Control is already installed")
	}

	if err := checkPortAvailable(); err != nil {
		return err
	}

	source, err := selfExe()
	if err != nil {
		return err
	}
	target := installedExe()
	installDirExisted := pathExists(installDir())
	dataDirExisted := pathExists(dataDir())
	copiedExecutable := false
	serviceCreated := false
	firewallCreated := false
	uninstallCreated := false
	eventLogCreated := false
	installSucceeded := false

	defer func() {
		if installSucceeded {
			return
		}
		_ = stopService()
		if serviceCreated {
			_ = deleteService()
		}
		_ = platform.RemoveRobloxGuard()
		if firewallCreated {
			_ = removeFirewallRule()
		}
		if uninstallCreated {
			_ = removeUninstallEntry()
		}
		if eventLogCreated {
			_ = eventlog.Remove(serviceName)
		}
		if !dataDirExisted {
			_ = os.RemoveAll(dataDir())
		}
		if !installDirExisted {
			_ = os.RemoveAll(installDir())
		} else if copiedExecutable {
			_ = os.Remove(target)
		}
	}()

	if !samePath(source, target) {
		if err := copyFile(source, target); err != nil {
			return fmt.Errorf("copy application: %w", err)
		}
		copiedExecutable = true
	}
	if err := ensurePrivateDataDir(); err != nil {
		return fmt.Errorf("secure application data: %w", err)
	}

	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Info|eventlog.Warning|eventlog.Error); err != nil {
		return fmt.Errorf("register Windows Event Log source: %w", err)
	}
	eventLogCreated = true

	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager: %w", err)
	}
	service, err := manager.CreateService(serviceName, target, mgr.Config{
		DisplayName: serviceDisplay,
		Description: "Blocks configured games on this PC until a parent enables play from the local dashboard.",
		StartType:   mgr.StartAutomatic,
		ServiceType: windows.SERVICE_WIN32_OWN_PROCESS,
	}, "--service")
	if err != nil {
		manager.Disconnect()
		return fmt.Errorf("register Windows service: %w", err)
	}
	serviceCreated = true
	if err := secureServicePermissions(service); err != nil {
		service.Close()
		manager.Disconnect()
		return fmt.Errorf("secure service permissions: %w", err)
	}
	if err := service.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400); err != nil {
		service.Close()
		manager.Disconnect()
		return fmt.Errorf("set service recovery: %w", err)
	}
	service.Close()
	manager.Disconnect()

	if err := addFirewallRule(target); err != nil {
		return fmt.Errorf("add private-LAN firewall rule: %w", err)
	}
	firewallCreated = true

	if err := registerUninstallEntry(target); err != nil {
		return fmt.Errorf("register uninstall entry: %w", err)
	}
	uninstallCreated = true

	if err := startService(); err != nil {
		return err
	}
	if err := waitForHealth(20 * time.Second); err != nil {
		return err
	}

	installSucceeded = true
	return nil
}

func secureServicePermissions(service *mgr.Service) error {
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.SERVICE_ALL_ACCESS,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(systemSID),
			},
		},
		{
			AccessPermissions: windows.SERVICE_ALL_ACCESS,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(adminSID),
			},
		},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(
		service.Handle,
		windows.SE_SERVICE,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}

func checkPortAvailable() error {
	listener, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", httpPort))
	if err != nil {
		return fmt.Errorf("TCP port %d is already in use; close the other program and run the installer again: %w", httpPort, err)
	}
	return listener.Close()
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func registerUninstallEntry(executable string) error {
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, uninstallKey, registry.WRITE)
	if err != nil {
		return err
	}
	defer key.Close()

	values := map[string]string{
		"DisplayName":     serviceDisplay,
		"DisplayVersion":  buildinfo.Version,
		"Publisher":       "Playground Control",
		"InstallLocation": filepath.Dir(executable),
		"DisplayIcon":     windows.EscapeArg(executable),
		"UninstallString": windows.EscapeArg(executable) + " --uninstall",
	}
	for name, value := range values {
		if err := key.SetStringValue(name, value); err != nil {
			return err
		}
	}
	if err := key.SetDWordValue("NoModify", 1); err != nil {
		return err
	}
	if err := key.SetDWordValue("NoRepair", 1); err != nil {
		return err
	}
	return nil
}

func removeUninstallEntry() error {
	err := registry.DeleteKey(registry.LOCAL_MACHINE, uninstallKey)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}

func performUninstall() error {
	var errs []error
	if err := stopService(); err != nil {
		errs = append(errs, err)
	}
	if err := deleteService(); err != nil {
		errs = append(errs, err)
	}
	if err := platform.RemoveRobloxGuard(); err != nil {
		errs = append(errs, fmt.Errorf("remove Roblox launch guard: %w", err))
	}
	if err := removeFirewallRule(); err != nil {
		errs = append(errs, err)
	}
	if err := removeUninstallEntry(); err != nil {
		errs = append(errs, err)
	}
	if err := eventlog.Remove(serviceName); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		errs = append(errs, fmt.Errorf("remove Event Log source: %w", err))
	}
	if err := os.RemoveAll(dataDir()); err != nil {
		errs = append(errs, fmt.Errorf("remove application data: %w", err))
	}
	return errors.Join(errs...)
}

func removeInstallFiles() error {
	var lastErr error
	for i := 0; i < 10; i++ {
		if err := os.RemoveAll(installDir()); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("remove application files: %w", lastErr)
}
