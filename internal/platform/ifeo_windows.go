//go:build windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/protection"
)

const (
	ifeoRoot         = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Image File Execution Options`
	ifeoBackupRoot   = `SOFTWARE\PlaygroundControl\IFEOBackup`
	debuggerValue    = "Debugger"
	managedMarker    = "--deny-launch"
	backupPresent    = "1\n"
	backupPreviously = "0"
)

type registryView struct {
	label  string
	access uint32
}

var ifeoViews = []registryView{
	{label: "64", access: registry.WOW64_64KEY},
	{label: "32", access: registry.WOW64_32KEY},
}

// RobloxGuard manages the per-image IFEO entries that prevent Roblox from
// reaching process creation while play is blocked.
type RobloxGuard struct {
	executable string
	images     []string
}

// NewRobloxGuard builds the mandatory Roblox launch guard for the installed
// executable.
func NewRobloxGuard() (*RobloxGuard, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate executable for launch guard: %w", err)
	}
	if strings.TrimSpace(executable) == "" {
		return nil, errors.New("launch guard executable path is empty")
	}
	return newRobloxGuard(executable, protection.RobloxExecutables()), nil
}

func newRobloxGuard(executable string, images []string) *RobloxGuard {
	return &RobloxGuard{
		executable: executable,
		images:     append([]string(nil), images...),
	}
}

// SetBlocked installs the pre-launch guard when blocked is true and removes it
// when false. Both the 64-bit and 32-bit IFEO registry views are managed.
func (g *RobloxGuard) SetBlocked(blocked bool) error {
	if g == nil {
		return errors.New("launch guard is not configured")
	}
	if !blocked && len(g.images) == 0 {
		return errors.New("launch guard has no images")
	}
	var errs []error
	for _, image := range g.images {
		if strings.TrimSpace(image) == "" {
			continue
		}
		for _, view := range ifeoViews {
			var err error
			if blocked {
				err = g.blockImage(image, view)
			} else {
				err = g.unblockImage(image, view)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s (%s-bit): %w", image, view.label, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Active reports whether every managed image is currently guarded with the
// expected debugger command.
func (g *RobloxGuard) Active() (bool, error) {
	if g == nil || strings.TrimSpace(g.executable) == "" {
		return false, nil
	}
	expected := guardCommand(g.executable)
	for _, image := range g.images {
		for _, view := range ifeoViews {
			key, err := registry.OpenKey(registry.LOCAL_MACHINE, ifeoKeyPath(image), registry.QUERY_VALUE|view.access)
			if err != nil {
				return false, nil
			}
			value, _, err := key.GetStringValue(debuggerValue)
			key.Close()
			if err != nil || !strings.EqualFold(strings.TrimSpace(value), expected) {
				return false, nil
			}
		}
	}
	return true, nil
}

func (g *RobloxGuard) blockImage(image string, view registryView) error {
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, ifeoKeyPath(image), registry.QUERY_VALUE|registry.SET_VALUE|registry.CREATE_SUB_KEY|view.access)
	if err != nil {
		return fmt.Errorf("open IFEO key: %w", err)
	}
	defer key.Close()

	previous, hadDebugger, err := readDebugger(key)
	if err != nil {
		return fmt.Errorf("read existing debugger: %w", err)
	}
	if !hadDebugger || !isManagedDebugger(previous) {
		backup := backupPreviously
		if hadDebugger {
			backup = backupPresent + previous
		}
		if err := writeBackup(view, image, backup); err != nil {
			return fmt.Errorf("back up existing debugger: %w", err)
		}
	}
	if err := key.SetStringValue(debuggerValue, guardCommand(g.executable)); err != nil {
		return fmt.Errorf("set IFEO debugger: %w", err)
	}
	return nil
}

func (g *RobloxGuard) unblockImage(image string, view registryView) error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, ifeoKeyPath(image), registry.QUERY_VALUE|registry.SET_VALUE|view.access)
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	if err == nil {
		previous, hadDebugger, readErr := readDebugger(key)
		if readErr != nil {
			key.Close()
			return readErr
		}
		if hadDebugger && isManagedDebugger(previous) {
			if err := key.DeleteValue(debuggerValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
				key.Close()
				return err
			}
		}
		key.Close()
	}

	backup, found, err := readBackup(view, image)
	if err != nil {
		return err
	}
	if found {
		if strings.HasPrefix(backup, backupPresent) {
			original := strings.TrimPrefix(backup, backupPresent)
			restore, _, err := registry.CreateKey(registry.LOCAL_MACHINE, ifeoKeyPath(image), registry.WRITE|view.access)
			if err != nil {
				return fmt.Errorf("restore previous debugger: %w", err)
			}
			if err := restore.SetStringValue(debuggerValue, original); err != nil {
				restore.Close()
				return fmt.Errorf("restore previous debugger value: %w", err)
			}
			restore.Close()
		}
		if err := deleteBackup(view, image); err != nil {
			return err
		}
	}
	return nil
}

// RemoveRobloxGuard removes only Playground Control's IFEO debugger entries.
// It is intentionally usable from the temporary uninstaller process.
func RemoveRobloxGuard() error {
	guard := newRobloxGuard("", protection.RobloxExecutables())
	return guard.SetBlocked(false)
}

func guardCommand(executable string) string {
	return windows.EscapeArg(executable) + " " + managedMarker
}

func isManagedDebugger(value string) bool {
	return strings.Contains(strings.ToLower(value), managedMarker)
}

func readDebugger(key registry.Key) (string, bool, error) {
	value, _, err := key.GetStringValue(debuggerValue)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func writeBackup(view registryView, image, value string) error {
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, ifeoBackupRoot, registry.WRITE)
	if err != nil {
		return fmt.Errorf("open guard backup: %w", err)
	}
	defer key.Close()
	if err := key.SetStringValue(backupValueName(view, image), value); err != nil {
		return fmt.Errorf("write guard backup: %w", err)
	}
	return nil
}

func readBackup(view registryView, image string) (string, bool, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, ifeoBackupRoot, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("open guard backup: %w", err)
	}
	defer key.Close()
	value, _, err := key.GetStringValue(backupValueName(view, image))
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read guard backup: %w", err)
	}
	return value, true, nil
}

func deleteBackup(view registryView, image string) error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, ifeoBackupRoot, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open guard backup for cleanup: %w", err)
	}
	defer key.Close()
	if err := key.DeleteValue(backupValueName(view, image)); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("delete guard backup: %w", err)
	}
	return nil
}

func ifeoKeyPath(image string) string {
	return ifeoRoot + `\` + image
}

func backupValueName(view registryView, image string) string {
	return view.label + "|" + image
}
