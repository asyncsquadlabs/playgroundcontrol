//go:build windows

package windowsapp

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	studySearchQuery        = "gere um guia de estudos para passar na prova do ifisc garopaba 2027"
	studySearchEndpoint     = "https://www.google.com/search"
	chromeAppPathKey        = `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe`
	chromeApplicationSubdir = `Google\Chrome\Application\chrome.exe`
)

func studySearchURL() string {
	query := url.Values{}
	query.Set("q", studySearchQuery)
	return studySearchEndpoint + "?" + query.Encode()
}

// openStudySearch opens Chrome with the fixed study search and falls back to the
// user's default browser when Chrome is unavailable.
func openStudySearch() error {
	return openStudySearchWith(studySearchURL(), chromeExecutable, launchChrome, openURL)
}

func openStudySearchWith(
	target string,
	findChrome func() (string, bool),
	launch func(string, string) error,
	openDefault func(string) error,
) error {
	var chromeErr error
	if chromePath, ok := findChrome(); ok {
		chromeErr = launch(chromePath, target)
		if chromeErr == nil {
			return nil
		}
	}

	defaultErr := openDefault(target)
	if defaultErr == nil {
		return nil
	}
	if chromeErr != nil {
		return errors.Join(chromeErr, defaultErr)
	}
	return fmt.Errorf("open default browser: %w", defaultErr)
}

func chromeExecutable() (string, bool) {
	return firstExistingExecutable(chromePathCandidates())
}

func chromePathCandidates() []string {
	return []string{
		chromeAppPath(registry.LOCAL_MACHINE, registry.WOW64_64KEY),
		chromeAppPath(registry.LOCAL_MACHINE, registry.WOW64_32KEY),
		chromeInstallCandidate(programFilesDir()),
		chromeInstallCandidate(programFilesX86Dir()),
		chromeInstallCandidate(localAppDataDir()),
		chromeAppPath(registry.CURRENT_USER, registry.WOW64_64KEY),
		chromeAppPath(registry.CURRENT_USER, registry.WOW64_32KEY),
	}
}

func chromeInstallCandidate(base string) string {
	if strings.TrimSpace(base) == "" {
		return ""
	}
	return filepath.Join(base, chromeApplicationSubdir)
}

func chromeAppPath(root registry.Key, access uint32) string {
	key, err := registry.OpenKey(root, chromeAppPathKey, registry.QUERY_VALUE|access)
	if err != nil {
		return ""
	}
	defer key.Close()

	value, _, err := key.GetStringValue("")
	if err != nil {
		return ""
	}
	return normalizeChromeAppPath(value)
}

func normalizeChromeAppPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, `"`) {
		rest := value[1:]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			return ""
		}
		value = rest[:end]
	}
	value = strings.TrimSpace(value)
	if !filepath.IsAbs(value) || !strings.EqualFold(filepath.Ext(value), ".exe") {
		return ""
	}
	return filepath.Clean(value)
}

func firstExistingExecutable(candidates []string) (string, bool) {
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		return filepath.Clean(candidate), true
	}
	return "", false
}

func launchChrome(executable, target string) error {
	cmd := exec.Command(executable, target) // #nosec G204 -- executable is resolved from Chrome install locations and target is a fixed internal URL.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Google Chrome: %w", err)
	}
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	return nil
}

func programFilesX86Dir() string {
	if path, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFilesX86, windows.KF_FLAG_DEFAULT); err == nil && strings.TrimSpace(path) != "" {
		return path
	}
	if value := strings.TrimSpace(os.Getenv("ProgramFiles(x86)")); value != "" {
		return value
	}
	return `C:\Program Files (x86)`
}

func localAppDataDir() string {
	if path, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT); err == nil && strings.TrimSpace(path) != "" {
		return path
	}
	if value := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); value != "" {
		return value
	}
	if profile := strings.TrimSpace(os.Getenv("USERPROFILE")); profile != "" {
		return filepath.Join(profile, "AppData", "Local")
	}
	return ""
}

func showStudySearchFailure() {
	showMessage(
		"Playground Control",
		"Roblox is blocked while parent-controlled play is turned off.\n\n"+
			"Google Chrome and your default browser could not be opened automatically.\n\n"+
			"Search for:\n"+studySearchQuery,
		windows.MB_OK|windows.MB_ICONWARNING|windows.MB_SETFOREGROUND,
	)
}
