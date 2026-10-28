//go:build windows

package windowsapp

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/buildinfo"
)

const serviceDescription = "Blocks configured games on this PC until a parent enables play from the local dashboard."

type windowsInstallBackend struct {
	log *installLog
}

func (b *windowsInstallBackend) Inspect() (installationInfo, error) {
	return inspectInstallation()
}

func (b *windowsInstallBackend) Install(rep installReporter, mode installMode) error {
	return installApplication(rep, mode)
}

func (b *windowsInstallBackend) FullReset(rep installReporter) error {
	if b != nil && b.log != nil {
		if preserved := b.log.Preserve(); preserved != "" {
			rep.Info("Installer transcript preserved at " + preserved)
		}
		b.log.Close()
		defer b.log.Reopen()
	}
	return performFullReset(rep)
}

func (b *windowsInstallBackend) TryStart(rep installReporter) error {
	stopTail := make(chan struct{})
	tailDone := make(chan struct{})
	go func() {
		defer close(tailDone)
		tailServiceLog(rep, stopTail)
	}()
	err := startServiceWithReporter(rep)
	close(stopTail)
	<-tailDone
	if err != nil {
		return withServiceDiagnostics(err)
	}
	if err := waitForHealth(20 * time.Second); err != nil {
		return fmt.Errorf("the service started but the dashboard did not respond: %w", err)
	}
	return nil
}

func inspectInstallation() (installationInfo, error) {
	info := installationInfo{
		InstalledExe:     pathExists(installedExe()),
		DataDir:          pathExists(dataDir()),
		UninstallEntry:   uninstallEntryExists(),
		InstalledVersion: installedVersion(),
	}
	exists, err := serviceExists()
	if err != nil {
		return info, err
	}
	info.ServiceRegistered = exists
	if !exists {
		return info, nil
	}
	service, manager, err := openService()
	if err != nil {
		info.ServiceState = "unknown"
		return info, nil
	}
	status, queryErr := queryServiceStatus(service)
	service.Close()
	manager.Disconnect()
	if queryErr != nil {
		info.ServiceState = "unknown"
		return info, nil
	}
	info.ServiceState = serviceStateName(status.State)
	if status.State == svc.Running {
		info.Healthy = serviceHealthy()
	}
	return info, nil
}

func installApplication(rep installReporter, mode installMode) (err error) {
	if rep == nil {
		rep = noopReporter{}
	}
	if !isElevated() {
		return errors.New("administrator elevation is required")
	}
	if !isWindows10OrNewer() {
		return errors.New("the installer requires Windows 10 or Windows 11")
	}

	installDirExisted := pathExists(installDir())
	dataDirExisted := pathExists(dataDir())
	target := installedExe()
	serviceCreated := false
	firewallCreated := false
	uninstallCreated := false
	eventLogCreated := false
	copiedExecutable := false

	rep.SetProgress(0, 8)
	rep.StartStep("Preparing the installation")
	source, err := selfExe()
	if err != nil {
		return err
	}

	if mode == modeRepair {
		rep.StartStep("Stopping the existing service")
		if err := stopService(); err != nil {
			rep.Failure("Could not stop the existing service: " + err.Error())
			return fmt.Errorf("stop existing service: %w", err)
		}
		rep.CompleteStep("Existing service stopped")
		if err := deleteService(); err != nil {
			rep.Failure("Could not remove the old service registration: " + err.Error())
			return fmt.Errorf("remove old service registration: %w", err)
		}
		rep.CompleteStep("Old service registration removed")
	} else if exists, err := serviceExists(); err != nil {
		return err
	} else if exists {
		return errors.New("the Playground Control service is already registered; use Repair or Full reset")
	}
	rep.SetProgress(1, 8)

	if err := checkPortAvailable(); err != nil {
		return err
	}
	rep.CompleteStep("TCP port 8080 is available")
	rep.CompleteStep("Installation preflight complete")

	if mode == modeFresh {
		defer func() {
			if err != nil {
				rollbackFailedInstall(rep, mode, installDirExisted, dataDirExisted, target, serviceCreated, firewallCreated, uninstallCreated, eventLogCreated, copiedExecutable)
			}
		}()
	}

	rep.StartStep("Copying the application to Program Files")
	if !samePath(source, target) {
		if err := copyInstalledExecutable(source, target); err != nil {
			rep.Failure("Could not copy the application: " + err.Error())
			return fmt.Errorf("copy application: %w", err)
		}
		copiedExecutable = true
	}
	rep.CompleteStep("Application copied to " + target)
	rep.SetProgress(2, 8)

	rep.StartStep("Securing the application data directory")
	if err := ensurePrivateDataDir(); err != nil {
		rep.Failure("Could not secure application data: " + err.Error())
		return fmt.Errorf("secure application data: %w", err)
	}
	rep.CompleteStep("Application data directory secured")
	rep.SetProgress(3, 8)

	rep.StartStep("Registering the Windows Event Log source")
	createdEventLog, err := ensureEventLogSource()
	if err != nil {
		rep.Failure("Could not register the Event Log source: " + err.Error())
		return fmt.Errorf("register Windows Event Log source: %w", err)
	}
	eventLogCreated = createdEventLog
	rep.CompleteStep("Windows Event Log source ready")
	rep.SetProgress(4, 8)

	rep.StartStep("Registering the Windows service")
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager: %w", err)
	}
	service, err := manager.CreateService(serviceName, target, mgr.Config{
		DisplayName: serviceDisplay,
		Description: serviceDescription,
		StartType:   mgr.StartAutomatic,
		ServiceType: windows.SERVICE_WIN32_OWN_PROCESS,
	}, "--service")
	if err != nil {
		manager.Disconnect()
		rep.Failure("Could not register the service: " + err.Error())
		return fmt.Errorf("register Windows service: %w", err)
	}
	serviceCreated = true
	if err := secureServicePermissions(service); err != nil {
		service.Close()
		manager.Disconnect()
		return fmt.Errorf("secure service permissions: %w", err)
	}
	service.Close()
	manager.Disconnect()
	rep.CompleteStep("Windows service registered")
	rep.SetProgress(5, 8)

	rep.StartStep("Adding the private-LAN firewall rule")
	if err := addFirewallRule(target); err != nil {
		return fmt.Errorf("add private-LAN firewall rule: %w", err)
	}
	firewallCreated = true
	rep.CompleteStep("Firewall rule added")
	rep.SetProgress(6, 8)

	rep.StartStep("Registering the uninstall entry")
	if err := registerUninstallEntry(target); err != nil {
		return fmt.Errorf("register uninstall entry: %w", err)
	}
	uninstallCreated = true
	rep.CompleteStep("Uninstall entry registered")
	rep.SetProgress(7, 8)

	rep.StartStep("Starting the service")
	stopTail := make(chan struct{})
	tailDone := make(chan struct{})
	go func() {
		defer close(tailDone)
		tailServiceLog(rep, stopTail)
	}()
	startErr := startServiceWithReporter(rep)
	close(stopTail)
	<-tailDone
	if startErr != nil {
		startErr = withServiceDiagnostics(startErr)
		rep.Failure("Service startup failed: " + startErr.Error())
		return startErr
	}
	if err := waitForHealth(20 * time.Second); err != nil {
		rep.Failure("The service started but the dashboard did not respond: " + err.Error())
		return fmt.Errorf("dashboard health check: %w", err)
	}
	rep.CompleteStep("Service started and dashboard is responding")
	rep.SetProgress(8, 8)

	if err := configureServiceRecovery(); err != nil {
		rep.Info("Warning: the service started, but recovery actions could not be configured: " + err.Error())
	}
	rep.CompleteStep("Playground Control " + buildinfo.Version + " installation complete")
	return nil
}

func copyInstalledExecutable(source, target string) error {
	var lastErr error
	for i := 0; i < 30; i++ {
		if err := copyFile(source, target); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return lastErr
}

func ensureEventLogSource() (bool, error) {
	if log, err := eventlog.Open(serviceName); err == nil {
		log.Close()
		return false, nil
	}
	_ = eventlog.Remove(serviceName)
	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Info|eventlog.Warning|eventlog.Error); err != nil {
		return false, err
	}
	return true, nil
}

func configureServiceRecovery() error {
	service, manager, err := openService()
	if err != nil {
		return err
	}
	defer service.Close()
	defer manager.Disconnect()
	return service.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400)
}

func rollbackFailedInstall(rep installReporter, mode installMode, installDirExisted, dataDirExisted bool, target string, serviceCreated, firewallCreated, uninstallCreated, eventLogCreated, copiedExecutable bool) {
	rep.StartStep("Rolling back incomplete installation changes")
	_ = stopService()
	if serviceCreated {
		_ = deleteService()
	}
	_ = removeFirewallRuleIfCreated(firewallCreated)
	_ = removeUninstallEntryIfCreated(uninstallCreated)
	_ = removeEventLogIfCreated(eventLogCreated)
	_ = removeGuardIfNeeded(mode)
	if mode == modeFresh {
		if !installDirExisted {
			_ = os.RemoveAll(installDir())
		} else if copiedExecutable {
			_ = os.Remove(target)
		}
	}
	if dataDirExisted {
		rep.Info("Existing application data was preserved at " + dataDir())
	} else {
		rep.Info("Application data and diagnostic logs were preserved at " + dataDir())
	}
	rep.CompleteStep("Rollback finished")
}

func removeFirewallRuleIfCreated(created bool) error {
	if !created {
		return nil
	}
	return removeFirewallRule()
}

func removeUninstallEntryIfCreated(created bool) error {
	if !created {
		return nil
	}
	return removeUninstallEntry()
}

func removeEventLogIfCreated(created bool) error {
	if !created {
		return nil
	}
	return windowsCleanupOps{}.RemoveEventLogSource()
}

func removeGuardIfNeeded(mode installMode) error {
	if mode != modeFresh {
		return nil
	}
	return platformRemoveRobloxGuard()
}

func platformRemoveRobloxGuard() error {
	return windowsCleanupOps{}.RemoveRobloxGuard()
}

func tailServiceLog(rep installReporter, stop <-chan struct{}) {
	if rep == nil {
		return
	}
	path := serviceLogPath()
	var offset int64
	if info, err := os.Stat(path); err == nil {
		offset = info.Size()
	}
	var partial string
	read := func() {
		info, err := os.Stat(path)
		if err != nil {
			return
		}
		if info.Size() < offset {
			offset = 0
			partial = ""
		}
		if info.Size() == offset {
			return
		}
		file, err := os.Open(path)
		if err != nil {
			return
		}
		if _, err := file.Seek(offset, 0); err != nil {
			file.Close()
			return
		}
		data := make([]byte, info.Size()-offset)
		n, _ := file.Read(data)
		file.Close()
		offset += int64(n)
		text := partial + string(data[:n])
		lines := strings.Split(text, "\n")
		partial = lines[len(lines)-1]
		for _, line := range lines[:len(lines)-1] {
			if strings.TrimSpace(line) != "" {
				rep.ServiceLine(line)
			}
		}
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			read()
			return
		case <-ticker.C:
			read()
		}
	}
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

func uninstallEntryExists() bool {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, uninstallKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	key.Close()
	return true
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
	return runCleanup(windowsCleanupOps{}, cleanupOptions{RemoveData: true}, noopReporter{})
}

func performFullReset(rep installReporter) error {
	if rep == nil {
		rep = noopReporter{}
	}
	return runCleanup(windowsCleanupOps{}, cleanupOptions{RemoveData: true, RemoveInstallFile: true}, rep)
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

func setupVersion() string {
	return buildinfo.Version
}
