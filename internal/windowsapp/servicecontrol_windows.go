//go:build windows

package windowsapp

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func openService() (*mgr.Service, *mgr.Mgr, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return nil, nil, fmt.Errorf("connect to service manager: %w", err)
	}
	service, err := manager.OpenService(serviceName)
	if err != nil {
		manager.Disconnect()
		return nil, nil, err
	}
	return service, manager, nil
}

func serviceExists() (bool, error) {
	service, manager, err := openService()
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return false, nil
		}
		if errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return true, nil
		}
		return false, err
	}
	service.Close()
	manager.Disconnect()
	return true, nil
}

func startService() error {
	service, manager, err := openService()
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer service.Close()
	defer manager.Disconnect()

	status, err := service.Query()
	if err == nil {
		switch status.State {
		case svc.Running:
			return nil
		case svc.StartPending, svc.ContinuePending:
			return waitForState(service, svc.Running, 30*time.Second)
		}
	}
	if err := service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start service: %w", err)
	}
	return waitForState(service, svc.Running, 30*time.Second)
}

func stopService() error {
	service, manager, err := openService()
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil
		}
		return fmt.Errorf("open service: %w", err)
	}
	defer service.Close()
	defer manager.Disconnect()

	status, err := service.Query()
	if err != nil {
		return fmt.Errorf("query service: %w", err)
	}
	if status.State == svc.Stopped {
		return nil
	}
	if status.State != svc.StopPending {
		if _, err := service.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return fmt.Errorf("stop service: %w", err)
		}
	}
	if err := waitForState(service, svc.Stopped, 30*time.Second); err != nil {
		return fmt.Errorf("wait for service stop: %w", err)
	}
	return nil
}

func waitForState(service *mgr.Service, wanted svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := service.Query()
		if err != nil {
			return err
		}
		if status.State == wanted {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("service did not reach state %d within %s", wanted, timeout)
}

func deleteService() error {
	exists, err := serviceExists()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	var lastErr error
	for i := 0; i < 40; i++ {
		service, manager, err := openService()
		if err != nil {
			if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
				return nil
			}
			return fmt.Errorf("open service: %w", err)
		}
		err = service.Delete()
		_ = service.Close()
		_ = manager.Disconnect()
		if err == nil || errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			lastErr = nil
			break
		}
		lastErr = err
		time.Sleep(150 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("delete service: %w", lastErr)
	}

	for i := 0; i < 40; i++ {
		exists, err := serviceExists()
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return errors.New("service remained registered after deletion")
}

func addFirewallRule(executable string) error {
	netsh := system32Executable("netsh.exe")
	_, _ = runHidden(netsh, "advfirewall", "firewall", "delete", "rule", "name="+firewallRule)
	output, err := runHidden(netsh,
		"advfirewall", "firewall", "add", "rule",
		"name="+firewallRule,
		"dir=in",
		"action=allow",
		"program="+executable,
		"protocol=TCP",
		fmt.Sprintf("localport=%d", httpPort),
		"profile=private",
		"remoteip=LocalSubnet",
		"enable=yes",
	)
	if err != nil {
		return commandError("netsh", output, err)
	}
	return nil
}

func removeFirewallRule() error {
	// A missing rule makes netsh return an error, which is not a failure for
	// uninstall. Verify that a named rule is gone afterward.
	netsh := system32Executable("netsh.exe")
	_, _ = runHidden(netsh, "advfirewall", "firewall", "delete", "rule", "name="+firewallRule)
	output, _ := runHidden(netsh, "advfirewall", "firewall", "show", "rule", "name="+firewallRule)
	if strings.Contains(string(output), firewallRule) {
		return errors.New("the Windows Firewall rule could not be removed")
	}
	return nil
}
