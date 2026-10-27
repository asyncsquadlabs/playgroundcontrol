//go:build windows

package windowsapp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func runningUnderWine() bool {
	return windows.NewLazySystemDLL("ntdll.dll").NewProc("wine_get_version").Find() == nil
}

func TestSecurePrivateDirectory(t *testing.T) {
	if runningUnderWine() {
		t.Skip("Wine does not faithfully implement Windows ACL semantics")
	}
	if !isElevated() {
		t.Skip("administrator rights are required to test directory ACLs")
	}
	dir := t.TempDir()
	if err := securePrivateDirectory(dir); err != nil {
		t.Fatalf("secure directory: %v", err)
	}
	sd, err := windows.GetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil || !owner.Equals(adminSID) {
		t.Fatalf("owner = %v, want BUILTIN\\Administrators", owner)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("data directory DACL still inherits permissions")
	}
}

func TestRejectReparsePoints(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "file.txt"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectReparsePoints(root); err != nil {
		t.Fatalf("normal data directory was rejected: %v", err)
	}

	target := t.TempDir()
	junction := filepath.Join(root, "junction")
	cmd := exec.Command(system32Executable("cmd.exe"), "/c", "mklink", "/J", junction, target)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("junction creation is unavailable: %v: %s", err, output)
	}
	if err := rejectReparsePoints(root); err == nil {
		t.Fatal("data directory junction was accepted")
	}
}
