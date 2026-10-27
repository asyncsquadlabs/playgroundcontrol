//go:build windows

// Package platform implements the Windows-specific process discovery, graceful
// close helper, and termination primitives used by the enforcement engine.
package platform

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/enforce"
)

// Source is the Windows process implementation.
type Source struct {
	exePath string

	privilegeMu sync.Mutex
}

// NewSource creates a process source using the current executable for the
// per-session close helper.
func NewSource() (*Source, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate executable: %w", err)
	}
	return &Source{exePath: exe}, nil
}

// List enumerates every process visible to the service.
func (s *Source) List() ([]enforce.Process, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("create process snapshot: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, fmt.Errorf("read first process: %w", err)
	}

	var processes []enforce.Process
	for {
		process := enforce.Process{
			PID:  entry.ProcessID,
			Name: windows.UTF16ToString(entry.ExeFile[:]),
		}
		if entry.ProcessID != 0 {
			handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, entry.ProcessID)
			if err == nil {
				if path, pathErr := queryImagePath(handle); pathErr == nil {
					process.ImagePath = path
				}
				var creation, exit, kernel, user windows.Filetime
				if timesErr := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); timesErr == nil {
					process.CreatedAt = time.Unix(0, creation.Nanoseconds()).UTC()
				}
				var session uint32
				if sessionErr := windows.ProcessIdToSessionId(entry.ProcessID, &session); sessionErr == nil {
					process.SessionID = session
				}
				_ = windows.CloseHandle(handle)
			}
		}
		processes = append(processes, process)

		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, fmt.Errorf("read next process: %w", err)
		}
	}
	return processes, nil
}

func queryImagePath(handle windows.Handle) (string, error) {
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(handle, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:size]), nil
}

// RequestClose starts a helper in the target process's interactive session. The
// helper posts WM_CLOSE to that process's top-level windows. A failure here is
// not fatal: the enforcer will still force-terminate after the grace period.
func (s *Source) RequestClose(process enforce.Process) error {
	if process.SessionID == 0 {
		return errors.New("process is not in an interactive user session")
	}
	if err := s.enableHelperPrivileges(); err != nil {
		// Continue anyway; CreateProcessAsUser may still succeed with the
		// privileges LocalSystem already has.
	}

	var userToken windows.Token
	if err := windows.WTSQueryUserToken(process.SessionID, &userToken); err != nil {
		return fmt.Errorf("get user token for session %d: %w", process.SessionID, err)
	}
	defer userToken.Close()

	commandLine := windows.EscapeArg(s.exePath) + " --close-helper --pids " + strconv.FormatUint(uint64(process.PID), 10)
	exePtr, err := windows.UTF16PtrFromString(s.exePath)
	if err != nil {
		return err
	}
	cmdPtr, err := windows.UTF16PtrFromString(commandLine)
	if err != nil {
		return err
	}
	desktopPtr, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return err
	}

	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	startup.Desktop = desktopPtr

	var environment *uint16
	creationFlags := uint32(windows.CREATE_NO_WINDOW)
	if err := windows.CreateEnvironmentBlock(&environment, userToken, false); err == nil {
		defer windows.DestroyEnvironmentBlock(environment)
		creationFlags |= windows.CREATE_UNICODE_ENVIRONMENT
	}
	var info windows.ProcessInformation
	if err := windows.CreateProcessAsUser(
		userToken,
		exePtr,
		cmdPtr,
		nil,
		nil,
		false,
		creationFlags,
		environment,
		nil,
		&startup,
		&info,
	); err != nil {
		return fmt.Errorf("launch close helper: %w", err)
	}
	if info.Process != 0 {
		_ = windows.CloseHandle(info.Process)
	}
	if info.Thread != 0 {
		_ = windows.CloseHandle(info.Thread)
	}
	return nil
}

// Kill force-terminates a process after verifying its creation identity.
func (s *Source) Kill(process enforce.Process) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, process.PID)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return enforce.ErrProcessGone
		}
		return fmt.Errorf("open process %d: %w", process.PID, err)
	}
	defer windows.CloseHandle(handle)

	if !process.CreatedAt.IsZero() {
		var creation, exit, kernel, user windows.Filetime
		if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
			return fmt.Errorf("check process identity: %w", err)
		}
		actual := time.Unix(0, creation.Nanoseconds()).UTC()
		if !actual.Equal(process.CreatedAt.UTC()) {
			return enforce.ErrProcessGone
		}
	}
	if err := windows.TerminateProcess(handle, 1); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("terminate process: access denied (the game may be protected): %w", err)
		}
		return fmt.Errorf("terminate process: %w", err)
	}
	return nil
}

func (s *Source) enableHelperPrivileges() error {
	s.privilegeMu.Lock()
	defer s.privilegeMu.Unlock()

	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()

	var firstErr error
	for _, name := range []string{
		"SeAssignPrimaryTokenPrivilege",
		"SeIncreaseQuotaPrivilege",
	} {
		if err := enablePrivilege(token, name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func enablePrivilege(token windows.Token, name string) error {
	wide, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, wide, &luid); err != nil {
		return fmt.Errorf("look up privilege %s: %w", name, err)
	}
	state := windows.Tokenprivileges{
		PrivilegeCount: 1,
		Privileges: [1]windows.LUIDAndAttributes{{
			Luid:       luid,
			Attributes: windows.SE_PRIVILEGE_ENABLED,
		}},
	}
	if err := windows.AdjustTokenPrivileges(token, false, &state, 0, nil, nil); err != nil {
		return fmt.Errorf("enable privilege %s: %w", name, err)
	}
	return nil
}

// ParsePIDList parses the comma-separated internal close-helper argument.
func ParsePIDList(value string) ([]uint32, error) {
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("PID list is empty")
	}
	parts := strings.Split(value, ",")
	out := make([]uint32, 0, len(parts))
	for _, part := range parts {
		pid, err := strconv.ParseUint(strings.TrimSpace(part), 10, 32)
		if err != nil || pid == 0 {
			return nil, fmt.Errorf("invalid PID %q", part)
		}
		out = append(out, uint32(pid))
	}
	return out, nil
}
