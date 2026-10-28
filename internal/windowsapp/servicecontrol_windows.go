//go:build windows

package windowsapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceStartTimeout = 120 * time.Second
	serviceStopTimeout  = 30 * time.Second
	serviceWaitHintMS   = uint32(180000)
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

type serviceStatusQuerier interface {
	QueryStatus() (svc.Status, error)
}

type mgrServiceQuerier struct {
	service *mgr.Service
}

func (q mgrServiceQuerier) QueryStatus() (svc.Status, error) {
	return queryServiceStatus(q.service)
}

func queryServiceStatus(service *mgr.Service) (svc.Status, error) {
	var status windows.SERVICE_STATUS_PROCESS
	var needed uint32
	err := windows.QueryServiceStatusEx(
		service.Handle,
		windows.SC_STATUS_PROCESS_INFO,
		(*byte)(unsafe.Pointer(&status)),
		uint32(unsafe.Sizeof(status)),
		&needed,
	)
	if err != nil {
		return svc.Status{}, err
	}
	return svc.Status{
		State:                   svc.State(status.CurrentState),
		Accepts:                 svc.Accepted(status.ControlsAccepted),
		CheckPoint:              status.CheckPoint,
		WaitHint:                status.WaitHint,
		ProcessId:               status.ProcessId,
		Win32ExitCode:           status.Win32ExitCode,
		ServiceSpecificExitCode: status.ServiceSpecificExitCode,
	}, nil
}

func startService() error {
	if err := startServiceWithReporter(nil); err != nil {
		return withServiceDiagnostics(err)
	}
	return nil
}

func startServiceWithReporter(rep installReporter) error {
	service, manager, err := openService()
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer service.Close()
	defer manager.Disconnect()

	query := mgrServiceQuerier{service: service}
	status, queryErr := query.QueryStatus()
	if queryErr == nil {
		switch status.State {
		case svc.Running:
			if rep != nil {
				rep.Info("The existing service is already running.")
			}
			return nil
		case svc.StartPending, svc.ContinuePending:
			return waitForServiceStart(query, status, rep)
		case svc.StopPending:
			if rep != nil {
				rep.Info("Waiting for the previous service instance to stop.")
			}
			if err := waitForState(query, svc.Stopped, serviceStopTimeout, nil); err != nil {
				return fmt.Errorf("wait for previous service stop: %w", err)
			}
		}
	}

	if err := service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start service: %w", err)
	}
	return waitForServiceStart(query, svc.Status{}, rep)
}

func waitForServiceStart(query serviceStatusQuerier, initial svc.Status, rep installReporter) error {
	lastCheckpoint := initial.CheckPoint
	lastState := initial.State
	if lastState == 0 {
		lastState = svc.StartPending
	}
	onStatus := func(status svc.Status) {
		lastState = status.State
		if rep == nil {
			return
		}
		if status.State == svc.StartPending && status.CheckPoint != lastCheckpoint {
			lastCheckpoint = status.CheckPoint
			if label := startupPhaseLabel(status.CheckPoint); label != "" {
				rep.Info("Service startup: " + label)
				rep.SetStatus("Service startup: " + label)
			}
		}
	}
	if err := waitForState(query, svc.Running, serviceStartTimeout, onStatus); err != nil {
		if rep != nil {
			rep.Failure(serviceStateError(statusForReporter(query), lastState, lastCheckpoint))
		}
		return err
	}
	if rep != nil {
		rep.CompleteStep("Service startup completed")
	}
	return nil
}

func statusForReporter(query serviceStatusQuerier) svc.Status {
	if query == nil {
		return svc.Status{}
	}
	status, _ := query.QueryStatus()
	return status
}

func serviceStateError(status svc.Status, lastState svc.State, lastCheckpoint uint32) string {
	phase := startupPhaseLabel(lastCheckpoint)
	if phase == "" {
		phase = "unknown"
	}
	message := fmt.Sprintf("The service did not reach Running (last state %s, last phase %s)", serviceStateName(lastState), phase)
	if status.Win32ExitCode != 0 || status.ServiceSpecificExitCode != 0 {
		message += fmt.Sprintf(" (Windows error %d, service-specific error %d)", status.Win32ExitCode, status.ServiceSpecificExitCode)
	}
	return message
}

func waitForState(query serviceStatusQuerier, wanted svc.State, timeout time.Duration, onStatus func(svc.Status)) error {
	deadline := time.Now().Add(timeout)
	var last svc.Status
	for {
		status, err := query.QueryStatus()
		if err != nil {
			if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
				return errors.New("the service registration disappeared before startup completed")
			}
			return fmt.Errorf("query service: %w", err)
		}
		last = status
		if onStatus != nil {
			onStatus(status)
		}
		if status.State == wanted {
			return nil
		}
		if wanted == svc.Running && status.State == svc.Stopped {
			detail := fmt.Sprintf("service stopped before reaching Running (Windows error %d, service-specific error %d", status.Win32ExitCode, status.ServiceSpecificExitCode)
			if label := startupPhaseLabel(status.CheckPoint); label != "" {
				detail += ", last phase " + label
			}
			detail += ")"
			return errors.New(detail)
		}
		if time.Now().After(deadline) {
			detail := fmt.Sprintf("service did not reach %s within %s (last state %s, checkpoint %d", serviceStateName(wanted), timeout.Round(time.Second), serviceStateName(last.State), last.CheckPoint)
			if last.Win32ExitCode != 0 || last.ServiceSpecificExitCode != 0 {
				detail += fmt.Sprintf(", Windows error %d, service-specific error %d", last.Win32ExitCode, last.ServiceSpecificExitCode)
			}
			detail += ")"
			return errors.New(detail)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func serviceStateName(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "Stopped"
	case svc.StartPending:
		return "StartPending"
	case svc.StopPending:
		return "StopPending"
	case svc.Running:
		return "Running"
	case svc.ContinuePending:
		return "ContinuePending"
	case svc.PausePending:
		return "PausePending"
	case svc.Paused:
		return "Paused"
	default:
		return fmt.Sprintf("state-%d", state)
	}
}

func stopService() error {
	service, manager, err := openService()
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) || errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return nil
		}
		return fmt.Errorf("open service: %w", err)
	}
	defer service.Close()
	defer manager.Disconnect()

	query := mgrServiceQuerier{service: service}
	status, err := query.QueryStatus()
	if err != nil {
		return fmt.Errorf("query service: %w", err)
	}
	if status.State == svc.Stopped {
		return nil
	}
	if status.State != svc.StopPending {
		if _, err := service.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return fmt.Errorf("stop service: %w", err)
		}
	}
	if err := waitForState(query, svc.Stopped, serviceStopTimeout, nil); err != nil {
		return fmt.Errorf("wait for service stop: %w", err)
	}
	return nil
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
			if errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
				lastErr = nil
				break
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
	return errors.New("service remained registered after deletion; restart Windows and retry the operation")
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

func withServiceDiagnostics(err error) error {
	diagnostics := collectServiceDiagnostics()
	if strings.TrimSpace(diagnostics) == "" {
		return err
	}
	return fmt.Errorf("%w\n\n%s", err, diagnostics)
}

func collectServiceDiagnostics() string {
	var sections []string
	logPath := filepath.Join(dataDir(), "service.log")
	if tail := tailFile(logPath, 20); tail != "" {
		sections = append(sections, "service.log:\n"+tail)
	}
	if events := recentServiceEvents(); events != "" {
		sections = append(sections, "Recent Event Log entries:\n"+events)
	}
	out := strings.Join(sections, "\n\n")
	if len(out) > 6000 {
		out = out[len(out)-6000:]
	}
	return out
}

func tailFile(path string, maxLines int) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return ""
	}
	const chunkSize = 32 * 1024
	start := info.Size() - chunkSize
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, 0); err != nil {
		return ""
	}
	data := make([]byte, info.Size()-start)
	if _, err := io.ReadFull(file, data); err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func recentServiceEvents() string {
	wevtutil := system32Executable("wevtutil.exe")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, wevtutil, "qe", "Application", "/q:*[System[Provider[@Name='PlaygroundControl']]]", "/c:8", "/rd:true", "/f:text")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
