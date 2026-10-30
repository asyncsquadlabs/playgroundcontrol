//go:build windows

package browsercontrol

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

const (
	chromePolicyKey = `SOFTWARE\Policies\Google\Chrome`
	edgePolicyKey   = `SOFTWARE\Policies\Microsoft\Edge`
	markerStart     = "# >>> Playground Control blocked browser domains >>>"
	markerEnd       = "# <<< Playground Control blocked browser domains <<<"
	manifestVersion = 1
)

type savedValue struct {
	Existed bool   `json:"existed"`
	Type    uint32 `json:"type"`
	String  string `json:"string,omitempty"`
	DWORD   uint32 `json:"dword,omitempty"`
}

type enforcementManifest struct {
	Version          int                              `json:"version"`
	URLBlocklist     map[string]map[string]string     `json:"urlBlocklist,omitempty"`
	DoHOriginal      map[string]map[string]savedValue `json:"dohOriginal,omitempty"`
	HostsBackup      string                           `json:"hostsBackup,omitempty"`
	HostsOriginalSHA string                           `json:"hostsOriginalSha256,omitempty"`
}

// WindowsApplier manages Chrome/Edge URLBlocklist, DoH policies, and the hosts
// file. Paths are injectable for tests.
type WindowsApplier struct {
	root      registry.Key
	chromeKey string
	edgeKey   string
	hostsPath string
	stateDir  string
	backupDir string
	manifest  string

	browsersOverride []string

	mu     sync.Mutex
	status model.BrowserStatus
}

// NewWindowsApplier builds the production machine-wide applier.
func NewWindowsApplier(dataDir string) *WindowsApplier {
	return newWindowsApplier(registry.LOCAL_MACHINE, chromePolicyKey, edgePolicyKey,
		defaultHostsPath(), dataDir)
}

func defaultHostsPath() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "drivers", "etc", "hosts")
}

func newWindowsApplier(root registry.Key, chromeKey, edgeKey, hostsPath, dataDir string) *WindowsApplier {
	return &WindowsApplier{
		root:      root,
		chromeKey: chromeKey,
		edgeKey:   edgeKey,
		hostsPath: hostsPath,
		stateDir:  dataDir,
		backupDir: filepath.Join(dataDir, "browser-backup"),
		manifest:  filepath.Join(dataDir, "browser-enforcement.json"),
		status:    model.BrowserStatus{Browsers: []string{}},
	}
}

// Restore removes all Playground Control-managed browser controls for the
// given protected data directory. It is used by uninstall and full reset.
func Restore(dataDir string) error {
	return NewWindowsApplier(dataDir).Restore()
}

// Sync applies or clears controls and returns the resulting status.
func (a *WindowsApplier) Sync(active bool, domains []string) (model.BrowserStatus, error) {
	if a == nil {
		return model.BrowserStatus{Browsers: []string{}}, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	domains = normalizeDomains(domains)
	manifest, err := a.loadManifest()
	if err != nil {
		return a.failure(err)
	}
	manifest.URLBlocklist = ensureBrowserMap(manifest.URLBlocklist)
	manifest.DoHOriginal = ensureSavedMap(manifest.DoHOriginal)

	browsers := a.detectBrowsers()
	drift := a.detectDrift(active, domains, browsers, manifest)

	for _, browser := range []string{"chrome", "edge"} {
		var policyErr error
		if active && containsString(browsers, browser) {
			policyErr = a.applyPolicy(browser, domains, manifest)
		} else {
			policyErr = a.clearPolicy(browser, manifest)
		}
		if policyErr != nil {
			_ = a.saveManifest(manifest)
			return a.failure(fmt.Errorf("%s browser policy: %w", browser, policyErr))
		}
	}
	var hostsErr error
	if active && len(domains) > 0 {
		hostsErr = a.applyHosts(domains, manifest)
	} else {
		hostsErr = a.restoreHosts(manifest)
	}
	if hostsErr != nil {
		_ = a.saveManifest(manifest)
		return a.failure(fmt.Errorf("hosts entries: %w", hostsErr))
	}
	if err := a.saveManifest(manifest); err != nil {
		return a.failure(err)
	}

	status := a.computeStatus(active, domains, browsers, manifest)
	status.DriftDetected = drift
	status.LastAppliedAt = time.Now().UTC()
	a.status = status
	return status, nil
}

// Restore removes managed controls and restores saved originals.
func (a *WindowsApplier) Restore() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	manifest, err := a.loadManifest()
	if err != nil {
		return err
	}
	manifest.URLBlocklist = ensureBrowserMap(manifest.URLBlocklist)
	manifest.DoHOriginal = ensureSavedMap(manifest.DoHOriginal)
	for _, browser := range []string{"chrome", "edge"} {
		if err := a.clearPolicy(browser, manifest); err != nil {
			_ = a.saveManifest(manifest)
			return fmt.Errorf("restore %s browser policy: %w", browser, err)
		}
	}
	if err := a.restoreHosts(manifest); err != nil {
		_ = a.saveManifest(manifest)
		return fmt.Errorf("restore hosts entries: %w", err)
	}
	if err := a.saveManifest(manifest); err != nil {
		return err
	}
	a.status = model.BrowserStatus{Browsers: a.detectBrowsers()}
	return nil
}

// Status returns the last known enforcement state.
func (a *WindowsApplier) Status() model.BrowserStatus {
	if a == nil {
		return model.BrowserStatus{Browsers: []string{}}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	status := a.status
	if status.Browsers == nil {
		status.Browsers = a.detectBrowsers()
	} else {
		browsers := make([]string, len(status.Browsers))
		copy(browsers, status.Browsers)
		status.Browsers = browsers
	}
	return status
}

func (a *WindowsApplier) failure(err error) (model.BrowserStatus, error) {
	status := a.status
	status.Browsers = a.detectBrowsers()
	status.LastError = err.Error()
	a.status = status
	return status, err
}

func (a *WindowsApplier) applyPolicy(browser string, domains []string, manifest *enforcementManifest) error {
	policyKey := a.policyKeyFor(browser)
	listPath := policyKey + `\URLBlocklist`
	key, _, err := registry.CreateKey(a.root, listPath, registry.QUERY_VALUE|registry.SET_VALUE|registry.CREATE_SUB_KEY)
	if err != nil {
		return err
	}
	defer key.Close()

	existing, err := readStringValues(key)
	if err != nil {
		return err
	}
	previous := manifest.URLBlocklist[browser]
	if managedDomainsMatch(previous, existing, domains) {
		manifest.URLBlocklist[browser] = previous
		return a.applyDoHPolicy(browser, manifest)
	}
	written := make(map[string]string)
	for name, value := range previous {
		if current, ok := existing[name]; ok && current == value {
			if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
				return err
			}
			delete(existing, name)
		}
	}

	next := nextListIndex(existing)
	manifest.URLBlocklist[browser] = written
	for i, domain := range domains {
		name := strconv.Itoa(next + i)
		if err := key.SetStringValue(name, domain); err != nil {
			return err
		}
		written[name] = domain
	}

	return a.applyDoHPolicy(browser, manifest)
}

func (a *WindowsApplier) clearPolicy(browser string, manifest *enforcementManifest) error {
	policyKey := a.policyKeyFor(browser)
	listPath := policyKey + `\URLBlocklist`
	key, err := registry.OpenKey(a.root, listPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err == nil {
		previous := manifest.URLBlocklist[browser]
		for name, value := range previous {
			current, _, readErr := key.GetStringValue(name)
			if readErr == nil && current == value {
				if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
					key.Close()
					return err
				}
			}
		}
		key.Close()
	} else if !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	delete(manifest.URLBlocklist, browser)

	return a.restoreDoHPolicy(browser, manifest)
}

func (a *WindowsApplier) applyDoHPolicy(browser string, manifest *enforcementManifest) error {
	key, _, err := registry.CreateKey(a.root, a.policyKeyFor(browser), registry.QUERY_VALUE|registry.SET_VALUE|registry.CREATE_SUB_KEY)
	if err != nil {
		return err
	}
	defer key.Close()

	original := manifest.DoHOriginal[browser]
	if original == nil {
		original = make(map[string]savedValue)
	}
	for _, name := range []string{"DnsOverHttpsMode", "BuiltInDnsClientEnabled"} {
		if _, saved := original[name]; saved {
			continue
		}
		current, err := readSavedValue(key, name)
		if err != nil {
			return err
		}
		original[name] = current
	}
	manifest.DoHOriginal[browser] = original

	mode, _, err := key.GetStringValue("DnsOverHttpsMode")
	if err != nil || mode != "off" {
		if err := key.SetStringValue("DnsOverHttpsMode", "off"); err != nil {
			return err
		}
	}
	enabled, _, err := key.GetIntegerValue("BuiltInDnsClientEnabled")
	if err != nil || enabled != 0 {
		if err := key.SetDWordValue("BuiltInDnsClientEnabled", 0); err != nil {
			return err
		}
	}
	return nil
}

func (a *WindowsApplier) restoreDoHPolicy(browser string, manifest *enforcementManifest) error {
	original := manifest.DoHOriginal[browser]
	if len(original) == 0 {
		return nil
	}
	key, err := registry.OpenKey(a.root, a.policyKeyFor(browser), registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			delete(manifest.DoHOriginal, browser)
			return nil
		}
		return err
	}
	defer key.Close()

	for name, saved := range original {
		current, readErr := readSavedValue(key, name)
		if readErr != nil {
			continue
		}
		if current.Existed {
			if name == "DnsOverHttpsMode" && (current.String != "off" || current.Type != registry.SZ) {
				continue
			}
			if name == "BuiltInDnsClientEnabled" && (current.DWORD != 0 || current.Type != registry.DWORD) {
				continue
			}
		}
		switch {
		case !saved.Existed:
			if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
				return err
			}
		case saved.Type == registry.DWORD:
			if err := key.SetDWordValue(name, saved.DWORD); err != nil {
				return err
			}
		case saved.Type == registry.EXPAND_SZ:
			if err := key.SetExpandStringValue(name, saved.String); err != nil {
				return err
			}
		default:
			if err := key.SetStringValue(name, saved.String); err != nil {
				return err
			}
		}
	}
	delete(manifest.DoHOriginal, browser)
	return nil
}

func (a *WindowsApplier) detectDrift(active bool, domains, browsers []string, manifest *enforcementManifest) bool {
	if active {
		hosts, err := os.ReadFile(a.hostsPath) // #nosec G304 -- fixed hosts path or injected test path.
		if err != nil || !hostsContainsDomains(string(hosts), domains) {
			return true
		}
		for _, browser := range browsers {
			written := manifest.URLBlocklist[browser]
			if len(written) == 0 {
				continue
			}
			key, err := registry.OpenKey(a.root, a.policyKeyFor(browser)+`\URLBlocklist`, registry.QUERY_VALUE)
			if err != nil {
				return true
			}
			values, readErr := readStringValues(key)
			key.Close()
			if readErr != nil {
				return true
			}
			for name, value := range written {
				if values[name] != value {
					return true
				}
			}
			for _, name := range []string{"DnsOverHttpsMode", "BuiltInDnsClientEnabled"} {
				policy, err := registry.OpenKey(a.root, a.policyKeyFor(browser), registry.QUERY_VALUE)
				if err != nil {
					return true
				}
				current, readErr := readSavedValue(policy, name)
				policy.Close()
				if readErr != nil {
					return true
				}
				if name == "DnsOverHttpsMode" && current.String != "off" {
					return true
				}
				if name == "BuiltInDnsClientEnabled" && current.DWORD != 0 {
					return true
				}
			}
		}
		return false
	}

	for _, browser := range []string{"chrome", "edge"} {
		if len(manifest.URLBlocklist[browser]) == 0 && len(manifest.DoHOriginal[browser]) == 0 {
			continue
		}
		key, err := registry.OpenKey(a.root, a.policyKeyFor(browser)+`\URLBlocklist`, registry.QUERY_VALUE)
		if err == nil {
			values, readErr := readStringValues(key)
			key.Close()
			if readErr == nil {
				for name, value := range manifest.URLBlocklist[browser] {
					if values[name] == value {
						return true
					}
				}
			}
		}
		if len(manifest.DoHOriginal[browser]) > 0 {
			return true
		}
	}
	hosts, err := os.ReadFile(a.hostsPath) // #nosec G304 -- fixed hosts path or injected test path.
	if err == nil && hostsHasMarker(string(hosts)) {
		return true
	}
	return false
}

func (a *WindowsApplier) computeStatus(active bool, domains, browsers []string, manifest *enforcementManifest) model.BrowserStatus {
	status := model.BrowserStatus{
		MonitoringEnabled: true,
		Browsers:          browsers,
		LastAppliedAt:     time.Now().UTC(),
	}
	if !active || len(domains) == 0 {
		return status
	}
	hosts, err := os.ReadFile(a.hostsPath) // #nosec G304 -- fixed hosts path or injected test path.
	status.HostsApplied = err == nil && hostsContainsDomains(string(hosts), domains)
	status.DoHDisabled = len(browsers) > 0
	status.URLBlocklistApplied = len(browsers) > 0
	for _, browser := range browsers {
		key, err := registry.OpenKey(a.root, a.policyKeyFor(browser)+`\URLBlocklist`, registry.QUERY_VALUE)
		if err != nil {
			status.URLBlocklistApplied = false
		} else {
			values, readErr := readStringValues(key)
			key.Close()
			if readErr != nil || !manifestCoversDomains(values, manifest.URLBlocklist[browser], domains) {
				status.URLBlocklistApplied = false
			}
		}
		for _, name := range []string{"DnsOverHttpsMode", "BuiltInDnsClientEnabled"} {
			policy, err := registry.OpenKey(a.root, a.policyKeyFor(browser), registry.QUERY_VALUE)
			if err != nil {
				status.DoHDisabled = false
				continue
			}
			current, readErr := readSavedValue(policy, name)
			policy.Close()
			if readErr != nil ||
				(name == "DnsOverHttpsMode" && current.String != "off") ||
				(name == "BuiltInDnsClientEnabled" && current.DWORD != 0) {
				status.DoHDisabled = false
			}
		}
	}
	return status
}

func (a *WindowsApplier) applyHosts(domains []string, manifest *enforcementManifest) error {
	current, err := os.ReadFile(a.hostsPath) // #nosec G304 -- fixed hosts path or injected test path.
	if err != nil {
		return err
	}
	base, _ := stripManagedHosts(string(current))
	if manifest.HostsBackup == "" {
		if err := os.MkdirAll(a.backupDir, 0o700); err != nil {
			return err
		}
		backupName := "hosts.original"
		if err := os.WriteFile(filepath.Join(a.backupDir, backupName), []byte(base), 0o600); err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(base))
		manifest.HostsBackup = backupName
		manifest.HostsOriginalSHA = hex.EncodeToString(sum[:])
	}
	next := renderManagedHosts(base, domains)
	if next == string(current) {
		return nil
	}
	if err := a.replaceHosts(next); err != nil {
		return err
	}
	flushDNS()
	return nil
}

func (a *WindowsApplier) restoreHosts(manifest *enforcementManifest) error {
	current, err := os.ReadFile(a.hostsPath) // #nosec G304 -- fixed hosts path or injected test path.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	stripped, changed := stripManagedHosts(string(current))
	if !changed && manifest.HostsBackup == "" {
		return nil
	}

	restore := stripped
	if manifest.HostsBackup != "" {
		backupPath := filepath.Join(a.backupDir, manifest.HostsBackup)
		if backup, readErr := os.ReadFile(backupPath); readErr == nil { // #nosec G304 -- backup name comes from our manifest under the protected data directory.
			backupText := string(backup)
			sum := sha256.Sum256([]byte(stripped))
			if hex.EncodeToString(sum[:]) == manifest.HostsOriginalSHA ||
				strings.TrimSuffix(stripped, "\r\n") == backupText ||
				strings.TrimSuffix(stripped, "\n") == backupText {
				restore = backupText
			}
		}
		_ = os.Remove(backupPath)
	}
	if restore != string(current) {
		if err := a.replaceHosts(restore); err != nil {
			return err
		}
		flushDNS()
	}
	manifest.HostsBackup = ""
	manifest.HostsOriginalSHA = ""
	return nil
}

func (a *WindowsApplier) replaceHosts(content string) error {
	dir := filepath.Dir(a.hostsPath)
	temp, err := os.CreateTemp(dir, "hosts.pc-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempName)
		}
	}()
	if _, err := temp.WriteString(content); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tempName)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(a.hostsPath)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func (a *WindowsApplier) loadManifest() (*enforcementManifest, error) {
	data, err := os.ReadFile(a.manifest) // #nosec G304 -- fixed path under the protected data directory.
	if errors.Is(err, os.ErrNotExist) {
		return &enforcementManifest{Version: manifestVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read browser enforcement backup: %w", err)
	}
	var manifest enforcementManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode browser enforcement backup: %w", err)
	}
	if manifest.Version == 0 {
		manifest.Version = manifestVersion
	}
	return &manifest, nil
}

func (a *WindowsApplier) saveManifest(manifest *enforcementManifest) error {
	if a.stateDir == "" {
		return nil
	}
	if err := os.MkdirAll(a.stateDir, 0o750); err != nil {
		return fmt.Errorf("create browser enforcement state directory: %w", err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode browser enforcement backup: %w", err)
	}
	temp := a.manifest + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return fmt.Errorf("write browser enforcement backup: %w", err)
	}
	from, err := windows.UTF16PtrFromString(temp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(a.manifest)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("install browser enforcement backup: %w", err)
	}
	return nil
}

func (a *WindowsApplier) policyKeyFor(browser string) string {
	if browser == "edge" {
		return a.edgeKey
	}
	return a.chromeKey
}

func (a *WindowsApplier) detectBrowsers() []string {
	if a.browsersOverride != nil {
		return append([]string(nil), a.browsersOverride...)
	}
	var out []string
	if browserExecutableExists("chrome") {
		out = append(out, "chrome")
	}
	if browserExecutableExists("edge") {
		out = append(out, "edge")
	}
	return out
}

func browserExecutableExists(browser string) bool {
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	programFilesX86 := os.Getenv("ProgramFiles(x86)")
	if programFilesX86 == "" {
		programFilesX86 = `C:\Program Files (x86)`
	}
	var candidates []string
	switch browser {
	case "chrome":
		candidates = []string{
			filepath.Join(programFiles, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(programFilesX86, "Google", "Chrome", "Application", "chrome.exe"),
		}
		matches, _ := filepath.Glob(`C:\Users\*\AppData\Local\Google\Chrome\Application\chrome.exe`)
		candidates = append(candidates, matches...)
	case "edge":
		candidates = []string{
			filepath.Join(programFilesX86, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(programFiles, "Microsoft", "Edge", "Application", "msedge.exe"),
		}
		matches, _ := filepath.Glob(`C:\Users\*\AppData\Local\Microsoft\Edge\Application\msedge.exe`)
		candidates = append(candidates, matches...)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func readStringValues(key registry.Key) (map[string]string, error) {
	names, err := key.ReadValueNames(-1)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		value, _, err := key.GetStringValue(name)
		if err != nil {
			continue
		}
		out[name] = value
	}
	return out, nil
}

func nextListIndex(existing map[string]string) int {
	max := 0
	for name := range existing {
		index, err := strconv.Atoi(name)
		if err == nil && index > max {
			max = index
		}
	}
	return max + 1
}

func readSavedValue(key registry.Key, name string) (savedValue, error) {
	size, _, err := key.GetValue(name, nil)
	if errors.Is(err, registry.ErrNotExist) {
		return savedValue{Existed: false}, nil
	}
	if err != nil {
		return savedValue{}, err
	}
	buf := make([]byte, size)
	_, valueType, err := key.GetValue(name, buf)
	if err != nil {
		return savedValue{}, err
	}
	saved := savedValue{Existed: true, Type: valueType}
	switch valueType {
	case registry.SZ, registry.EXPAND_SZ:
		if len(buf) >= 2 {
			words := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[0])), len(buf)/2)
			saved.String = windows.UTF16ToString(words)
		}
	case registry.DWORD:
		if len(buf) >= 4 {
			saved.DWORD = binary.LittleEndian.Uint32(buf)
		}
	}
	return saved, nil
}

func managedDomainsMatch(written, existing map[string]string, domains []string) bool {
	if len(written) != len(domains) {
		return false
	}
	desired := make(map[string]struct{}, len(domains))
	for _, domain := range domains {
		desired[domain] = struct{}{}
	}
	for name, value := range written {
		if existing[name] != value {
			return false
		}
		if _, ok := desired[value]; !ok {
			return false
		}
		delete(desired, value)
	}
	return len(desired) == 0
}

func normalizeDomains(domains []string) []string {
	seen := make(map[string]struct{}, len(domains))
	out := make([]string, 0, len(domains))
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if domain == "" {
			continue
		}
		if _, duplicate := seen[domain]; duplicate {
			continue
		}
		seen[domain] = struct{}{}
		out = append(out, domain)
	}
	sort.Strings(out)
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func hostsHasMarker(content string) bool {
	return strings.Contains(content, markerStart) && strings.Contains(content, markerEnd)
}

func hostsContainsDomains(content string, domains []string) bool {
	if !hostsHasMarker(content) {
		return false
	}
	for _, domain := range domains {
		if !strings.Contains(content, "0.0.0.0 "+domain+"\r\n") && !strings.Contains(content, "0.0.0.0 "+domain+"\n") {
			return false
		}
		if !strings.Contains(content, "0.0.0.0 www."+domain+"\r\n") && !strings.Contains(content, "0.0.0.0 www."+domain+"\n") {
			return false
		}
	}
	return true
}

func manifestCoversDomains(values, written map[string]string, domains []string) bool {
	present := make(map[string]struct{}, len(values))
	for _, value := range values {
		present[value] = struct{}{}
	}
	for _, domain := range domains {
		if _, ok := present[domain]; !ok {
			return false
		}
	}
	for name, value := range written {
		if values[name] != value {
			return false
		}
	}
	return true
}

func stripManagedHosts(content string) (string, bool) {
	start := strings.Index(content, markerStart)
	if start < 0 {
		return content, false
	}
	lineStart := strings.LastIndex(content[:start], "\n")
	if lineStart < 0 {
		lineStart = 0
	} else {
		lineStart++
	}
	endRel := strings.Index(content[start:], markerEnd)
	if endRel < 0 {
		// A truncated marker block is still ours; remove it to recover.
		return content[:lineStart], true
	}
	end := start + endRel + len(markerEnd)
	if end < len(content) && content[end] == '\r' {
		end++
	}
	if end < len(content) && content[end] == '\n' {
		end++
	}
	return content[:lineStart] + content[end:], true
}

func renderManagedHosts(base string, domains []string) string {
	var builder strings.Builder
	builder.WriteString(base)
	if base != "" && !strings.HasSuffix(base, "\n") {
		builder.WriteString("\r\n")
	}
	if len(domains) == 0 {
		return builder.String()
	}
	if builder.Len() > 0 && !strings.HasSuffix(builder.String(), "\n") {
		builder.WriteString("\r\n")
	}
	builder.WriteString(markerStart)
	builder.WriteString("\r\n")
	for _, domain := range domains {
		builder.WriteString("0.0.0.0 ")
		builder.WriteString(domain)
		builder.WriteString("\r\n")
		builder.WriteString("0.0.0.0 www.")
		builder.WriteString(domain)
		builder.WriteString("\r\n")
	}
	builder.WriteString(markerEnd)
	builder.WriteString("\r\n")
	return builder.String()
}

func flushDNS() {
	proc := windows.NewLazySystemDLL("dnsapi.dll").NewProc("DnsFlushResolverCache")
	_, _, _ = proc.Call()
}

func ensureBrowserMap(value map[string]map[string]string) map[string]map[string]string {
	if value == nil {
		return make(map[string]map[string]string)
	}
	return value
}

func ensureSavedMap(value map[string]map[string]savedValue) map[string]map[string]savedValue {
	if value == nil {
		return make(map[string]map[string]savedValue)
	}
	return value
}
