//go:build windows

package windowsapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const (
	serviceName    = "PlaygroundControl"
	serviceDisplay = "Playground Control"
	firewallRule   = "Playground Control"
	httpPort       = 8080
	uninstallKey   = `Software\Microsoft\Windows\CurrentVersion\Uninstall\PlaygroundControl`
	constIDYes     = 6
)

func programFilesDir() string {
	if path, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, windows.KF_FLAG_DEFAULT); err == nil && strings.TrimSpace(path) != "" {
		return path
	}
	if value := strings.TrimSpace(os.Getenv("ProgramFiles")); value != "" {
		return value
	}
	return `C:\Program Files`
}

func programDataDir() string {
	if path, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT); err == nil && strings.TrimSpace(path) != "" {
		return path
	}
	if value := strings.TrimSpace(os.Getenv("ProgramData")); value != "" {
		return value
	}
	return `C:\ProgramData`
}

func installDir() string {
	return filepath.Join(programFilesDir(), "PlaygroundControl")
}

func installedExe() string {
	return filepath.Join(installDir(), "PlaygroundControl.exe")
}

func dataDir() string {
	return filepath.Join(programDataDir(), "PlaygroundControl")
}

func setupTokenPath() string {
	return filepath.Join(dataDir(), "setup.token")
}

func selfExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Clean(exe), nil
}

func isInstalled() bool {
	_, err := os.Stat(installedExe())
	return err == nil
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func isElevated() bool {
	token := windows.GetCurrentProcessToken()
	return token.IsElevated()
}

func elevateSelf(extraArgs ...string) error {
	exe, err := selfExe()
	if err != nil {
		return err
	}
	filePtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	verbPtr, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	var argsPtr *uint16
	if len(extraArgs) > 0 {
		quoted := make([]string, len(extraArgs))
		for i, arg := range extraArgs {
			quoted[i] = windows.EscapeArg(arg)
		}
		argsPtr, err = windows.UTF16PtrFromString(strings.Join(quoted, " "))
		if err != nil {
			return err
		}
	}
	if err := windows.ShellExecute(0, verbPtr, filePtr, argsPtr, nil, windows.SW_SHOWNORMAL); err != nil {
		return fmt.Errorf("start elevated copy: %w", err)
	}
	return nil
}

func showMessage(title, message string, flags uint32) int32 {
	textPtr, _ := windows.UTF16PtrFromString(message)
	titlePtr, _ := windows.UTF16PtrFromString(title)
	result, _ := windows.MessageBox(0, textPtr, titlePtr, flags)
	return result
}

func showError(message string) {
	showMessage("Playground Control", message, windows.MB_OK|windows.MB_ICONERROR)
}

func showInfo(message string) {
	showMessage("Playground Control", message, windows.MB_OK|windows.MB_ICONINFORMATION)
}

func confirm(title, message string) bool {
	return showMessage(title, message, windows.MB_YESNO|windows.MB_ICONWARNING|windows.MB_DEFBUTTON2) == constIDYes
}

func system32Executable(name string) string {
	system32, err := windows.GetSystemDirectory()
	if err != nil || strings.TrimSpace(system32) == "" {
		system32 = filepath.Join(os.Getenv("SystemRoot"), "System32")
	}
	return filepath.Join(system32, name)
}

func runHidden(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.CombinedOutput()
}

func commandError(name string, output []byte, err error) error {
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(string(output))
	if message == "" {
		return fmt.Errorf("%s: %w", name, err)
	}
	return fmt.Errorf("%s: %w: %s", name, err, message)
}

func openURL(target string) error {
	// explorer.exe normally forwards the URL to the already-running,
	// unelevated shell, keeping the default browser unelevated even when setup
	// was launched through UAC.
	if err := exec.Command(system32Executable("explorer.exe"), target).Start(); err == nil { // #nosec G204 -- fixed trusted executable and an internally generated URL.
		return nil
	}
	verb, _ := windows.UTF16PtrFromString("open")
	file, _ := windows.UTF16PtrFromString(target)
	return windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL)
}

func httpClient() *http.Client {
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   2 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
}

type bootstrapInfo struct {
	SetupRequired bool   `json:"setupRequired"`
	Version       string `json:"version"`
	LocalRequest  bool   `json:"localRequest"`
}

func fetchBootstrap() (bootstrapInfo, error) {
	response, err := httpClient().Get("http://127.0.0.1:8080/api/v1/bootstrap")
	if err != nil {
		return bootstrapInfo{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return bootstrapInfo{}, fmt.Errorf("dashboard returned HTTP %d", response.StatusCode)
	}
	var info bootstrapInfo
	if err := json.NewDecoder(response.Body).Decode(&info); err != nil {
		return bootstrapInfo{}, err
	}
	return info, nil
}

func serviceHealthy() bool {
	response, err := httpClient().Get("http://127.0.0.1:8080/api/v1/health")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func waitForHealth(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if serviceHealthy() {
			return nil
		}
		lastErr = errors.New("service is not responding yet")
		time.Sleep(250 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("service did not become ready within %s: %w", timeout, lastErr)
	}
	return errors.New("service did not become ready")
}

func readSetupToken(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(setupTokenPath())
		if err == nil {
			token := strings.TrimSpace(string(data))
			if token != "" {
				return token, nil
			}
		} else {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr != nil {
		return "", fmt.Errorf("setup token was not created: %w", lastErr)
	}
	return "", errors.New("setup token was empty")
}

func setupURL(token string) string {
	return "http://127.0.0.1:8080/setup?token=" + urlQueryEscape(token)
}

func urlQueryEscape(value string) string {
	replacer := strings.NewReplacer("%", "%25", " ", "%20", "+", "%2B", "&", "%26", "#", "%23", "?", "%3F")
	return replacer.Replace(value)
}

func ensurePrivateDataDir() error {
	dir := dataDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	if err := rejectReparsePoints(dir); err != nil {
		return fmt.Errorf("validate data directory: %w", err)
	}
	if err := securePrivateDirectory(dir); err != nil {
		return fmt.Errorf("secure data directory: %w", err)
	}
	// Recheck after taking ownership so a reparse point cannot survive the ACL
	// transition.
	if err := rejectReparsePoints(dir); err != nil {
		return fmt.Errorf("validate secured data directory: %w", err)
	}
	return nil
}

// securePrivateDirectory replaces the directory DACL with a protected DACL
// owned by Administrators and accessible only to SYSTEM and Administrators.
// Resetting first removes any explicit or inherited ACEs supplied by a
// pre-existing user-created directory.
func securePrivateDirectory(dir string) error {
	steps := [][]string{
		{dir, "/setowner", "*S-1-5-32-544", "/T", "/C", "/Q"},
		{dir, "/reset", "/T", "/C", "/Q"},
		{dir, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", "/T", "/C", "/Q"},
	}
	for _, args := range steps {
		output, err := runHidden(system32Executable("icacls.exe"), args...)
		if err != nil {
			return commandError("icacls", output, err)
		}
	}
	return nil
}

func rejectReparsePoints(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("reparse point is not allowed: %s", path)
		}
		ptr, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		attributes, err := windows.GetFileAttributes(ptr)
		if err != nil {
			return err
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("reparse point is not allowed: %s", path)
		}
		return nil
	})
}

func copyFile(source, destination string) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".playground-copy-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.ReadFrom(src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, destination)
}

func scheduleDeleteOnReboot(path string) error {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(ptr, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

func isWindows10OrNewer() bool {
	version := windows.RtlGetVersion()
	return version.MajorVersion >= 10
}
