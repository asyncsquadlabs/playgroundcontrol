//go:build windows

package browsercontrol

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

func newTestApplier(t *testing.T, hostsContent string) *WindowsApplier {
	t.Helper()
	rootName := `Software\PlaygroundControlTests\` + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() { _ = registry.DeleteKey(registry.CURRENT_USER, rootName) })
	hostsPath := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hostsPath, []byte(hostsContent), 0o644); err != nil {
		t.Fatal(err)
	}
	applier := newWindowsApplier(
		registry.CURRENT_USER,
		rootName+`\Chrome`,
		rootName+`\Edge`,
		hostsPath,
		t.TempDir(),
	)
	applier.browsersOverride = []string{"chrome", "edge"}
	return applier
}

func seedExistingPolicies(t *testing.T, applier *WindowsApplier) {
	t.Helper()
	listKey, _, err := registry.CreateKey(registry.CURRENT_USER, applier.chromeKey+`\URLBlocklist`, registry.WRITE)
	if err != nil {
		t.Fatal(err)
	}
	if err := listKey.SetStringValue("1", "existing.example"); err != nil {
		t.Fatal(err)
	}
	if err := listKey.SetStringValue("2", ".exact.example"); err != nil {
		t.Fatal(err)
	}
	listKey.Close()
	policyKey, _, err := registry.CreateKey(registry.CURRENT_USER, applier.chromeKey, registry.WRITE)
	if err != nil {
		t.Fatal(err)
	}
	if err := policyKey.SetStringValue("DnsOverHttpsMode", "secure"); err != nil {
		t.Fatal(err)
	}
	if err := policyKey.SetDWordValue("BuiltInDnsClientEnabled", 1); err != nil {
		t.Fatal(err)
	}
	policyKey.Close()
}

func readListValues(t *testing.T, applier *WindowsApplier, browser string) map[string]string {
	t.Helper()
	key, err := registry.OpenKey(registry.CURRENT_USER, applier.policyKeyFor(browser)+`\URLBlocklist`, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Close()
	values, err := readStringValues(key)
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func readDoHValues(t *testing.T, applier *WindowsApplier, browser string) (string, uint32) {
	t.Helper()
	key, err := registry.OpenKey(registry.CURRENT_USER, applier.policyKeyFor(browser), registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Close()
	mode, _, err := key.GetStringValue("DnsOverHttpsMode")
	if err != nil {
		t.Fatal(err)
	}
	enabled, _, err := key.GetIntegerValue("BuiltInDnsClientEnabled")
	if err != nil {
		t.Fatal(err)
	}
	return mode, uint32(enabled)
}

func TestWindowsApplierMergesPoliciesAndRestoresHosts(t *testing.T) {
	originalHosts := "127.0.0.1 localhost\r\n\r\n# keep this comment\r\n"
	applier := newTestApplier(t, originalHosts)
	seedExistingPolicies(t, applier)

	status, err := applier.Sync(true, []string{"poki.com", "crazygames.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !status.URLBlocklistApplied || !status.HostsApplied || !status.DoHDisabled {
		t.Fatalf("status = %+v", status)
	}
	values := readListValues(t, applier, "chrome")
	if values["1"] != "existing.example" || values["2"] != ".exact.example" {
		t.Fatalf("existing policies were not preserved: %v", values)
	}
	present := map[string]bool{}
	for _, value := range values {
		present[value] = true
	}
	if !present["poki.com"] || !present["crazygames.com"] {
		t.Fatalf("managed domains missing: %v", values)
	}
	if mode, enabled := readDoHValues(t, applier, "chrome"); mode != "off" || enabled != 0 {
		t.Fatalf("DoH policy = %q/%d", mode, enabled)
	}
	hosts, err := os.ReadFile(applier.hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hosts), markerStart) || !strings.Contains(string(hosts), "0.0.0.0 poki.com") {
		t.Fatalf("hosts missing managed block:\n%s", hosts)
	}

	if _, err := applier.Sync(true, []string{"poki.com", "crazygames.com"}); err != nil {
		t.Fatal(err)
	}
	if count := len(readListValues(t, applier, "chrome")); count != 4 {
		t.Fatalf("idempotent apply grew URLBlocklist to %d entries: %v", count, readListValues(t, applier, "chrome"))
	}

	if _, err := applier.Sync(false, nil); err != nil {
		t.Fatal(err)
	}
	values = readListValues(t, applier, "chrome")
	if len(values) != 2 || values["1"] != "existing.example" || values["2"] != ".exact.example" {
		t.Fatalf("managed policies were not removed cleanly: %v", values)
	}
	if mode, enabled := readDoHValues(t, applier, "chrome"); mode != "secure" || enabled != 1 {
		t.Fatalf("DoH policy was not restored: %q/%d", mode, enabled)
	}
	hosts, err = os.ReadFile(applier.hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(hosts) != originalHosts {
		t.Fatalf("hosts was not restored exactly:\n%q\nwant:\n%q", hosts, originalHosts)
	}
}

func TestWindowsApplierRepairsDrift(t *testing.T) {
	applier := newTestApplier(t, "127.0.0.1 localhost\r\n")
	if _, err := applier.Sync(true, []string{"poki.com"}); err != nil {
		t.Fatal(err)
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, applier.chromeKey+`\URLBlocklist`, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	values := readListValues(t, applier, "chrome")
	for name, value := range values {
		if value == "poki.com" {
			if err := key.DeleteValue(name); err != nil {
				t.Fatal(err)
			}
		}
	}
	key.Close()

	status, err := applier.Sync(true, []string{"poki.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !status.DriftDetected {
		t.Fatalf("drift was not reported: %+v", status)
	}
	present := false
	for _, value := range readListValues(t, applier, "chrome") {
		if value == "poki.com" {
			present = true
		}
	}
	if !present {
		t.Fatal("drifted policy was not repaired")
	}
}

func TestWindowsApplierPreservesExternalHostsEdit(t *testing.T) {
	applier := newTestApplier(t, "127.0.0.1 localhost\r\n")
	if _, err := applier.Sync(true, []string{"poki.com"}); err != nil {
		t.Fatal(err)
	}
	hosts, err := os.ReadFile(applier.hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := string(hosts) + "# external edit\r\n"
	if err := os.WriteFile(applier.hostsPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := applier.Sync(false, nil); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(applier.hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(restored), markerStart) {
		t.Fatalf("managed marker survived restore:\n%s", restored)
	}
	if !strings.Contains(string(restored), "# external edit") {
		t.Fatalf("external hosts edit was lost:\n%s", restored)
	}
}

func TestWindowsApplierRestoreWithoutActiveSyncRemovesMarker(t *testing.T) {
	applier := newTestApplier(t, "127.0.0.1 localhost\r\n")
	if _, err := applier.Sync(true, []string{"poki.com"}); err != nil {
		t.Fatal(err)
	}
	// A fresh applier simulates uninstall after the in-memory manager is gone;
	// it must use the persisted manifest.
	fresh := newWindowsApplier(registry.CURRENT_USER, applier.chromeKey, applier.edgeKey, applier.hostsPath, applier.stateDir)
	fresh.browsersOverride = []string{"chrome", "edge"}
	if err := fresh.Restore(); err != nil {
		t.Fatal(err)
	}
	hosts, err := os.ReadFile(applier.hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(hosts), markerStart) {
		t.Fatalf("marker survived restore:\n%s", hosts)
	}
	if values := readListValues(t, fresh, "chrome"); len(values) != 0 {
		t.Fatalf("policies survived restore: %v", values)
	}
}

func TestWindowsApplierRestoresLFHostsExactly(t *testing.T) {
	original := "127.0.0.1 localhost\n# comment without CRLF\n"
	applier := newTestApplier(t, original)
	if _, err := applier.Sync(true, []string{"poki.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := applier.Sync(false, nil); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(applier.hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("LF hosts was not restored exactly: %q want %q", restored, original)
	}
}
