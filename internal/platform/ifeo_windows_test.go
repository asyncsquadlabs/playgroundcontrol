//go:build windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func TestRobloxGuardManagesIFEOAndRestoresExistingDebugger(t *testing.T) {
	image := fmt.Sprintf("PlaygroundControlGuardTest-%d.exe", os.Getpid())
	guard := newRobloxGuard(os.Args[0], []string{image})
	t.Cleanup(func() {
		_ = guard.SetBlocked(false)
		removeTestIFEOValues(t, image)
	})

	const original = `C:\Windows\System32\cmd.exe /c`
	for _, view := range ifeoViews {
		key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, ifeoKeyPath(image), registry.WRITE|view.access)
		if err != nil {
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				t.Skip("administrator rights are required to test IFEO entries")
			}
			t.Fatalf("seed %s-bit IFEO: %v", view.label, err)
		}
		if err := key.SetStringValue(debuggerValue, original); err != nil {
			key.Close()
			t.Fatal(err)
		}
		key.Close()
	}

	if err := guard.SetBlocked(true); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Skip("administrator rights are required to test IFEO entries")
		}
		t.Fatalf("install launch guard: %v", err)
	}
	active, err := guard.Active()
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("guard did not report active after installation")
	}
	for _, view := range ifeoViews {
		value, ok := readTestIFEO(t, image, view)
		if !ok || !isManagedDebugger(value) {
			t.Fatalf("%s-bit Debugger value = %q, present=%t", view.label, value, ok)
		}
	}

	if err := guard.SetBlocked(false); err != nil {
		t.Fatalf("remove launch guard: %v", err)
	}
	for _, view := range ifeoViews {
		value, ok := readTestIFEO(t, image, view)
		if !ok || value != original {
			t.Fatalf("%s-bit original debugger was not restored: %q, present=%t", view.label, value, ok)
		}
	}
}

func TestRobloxGuardRemovesManagedValueWithoutPriorDebugger(t *testing.T) {
	image := fmt.Sprintf("PlaygroundControlGuardEmptyTest-%d.exe", os.Getpid())
	guard := newRobloxGuard(os.Args[0], []string{image})
	t.Cleanup(func() {
		_ = guard.SetBlocked(false)
		removeTestIFEOValues(t, image)
	})

	if err := guard.SetBlocked(true); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Skip("administrator rights are required to test IFEO entries")
		}
		t.Fatalf("install launch guard: %v", err)
	}
	if err := guard.SetBlocked(false); err != nil {
		t.Fatalf("remove launch guard: %v", err)
	}
	for _, view := range ifeoViews {
		value, ok := readTestIFEO(t, image, view)
		if ok && strings.TrimSpace(value) != "" {
			t.Fatalf("%s-bit unexpected debugger value %q", view.label, value)
		}
	}
}

func readTestIFEO(t *testing.T, image string, view registryView) (string, bool) {
	t.Helper()
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, ifeoKeyPath(image), registry.QUERY_VALUE|view.access)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", false
		}
		t.Fatal(err)
	}
	defer key.Close()
	value, _, err := key.GetStringValue(debuggerValue)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return value, true
}

func removeTestIFEOValues(t *testing.T, image string) {
	t.Helper()
	for _, view := range ifeoViews {
		key, err := registry.OpenKey(registry.LOCAL_MACHINE, ifeoKeyPath(image), registry.SET_VALUE|view.access)
		if err != nil {
			continue
		}
		_ = key.DeleteValue(debuggerValue)
		key.Close()
	}
	for _, view := range ifeoViews {
		key, err := registry.OpenKey(registry.LOCAL_MACHINE, ifeoBackupRoot, registry.SET_VALUE)
		if err != nil {
			continue
		}
		_ = key.DeleteValue(backupValueName(view, image))
		key.Close()
	}
}
