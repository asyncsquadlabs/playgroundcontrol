//go:build windows

package browserwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

const (
	profileRetention = 5 * time.Minute
	profileListKey   = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`
)

type profileCache struct {
	ModTime  time.Time
	Size     int64
	WALMod   time.Time
	WALSize  int64
	Since    time.Time
	Detailed bool
	Loaded   bool
	Records  []historyRecord
	Count    int64
}

type localUser struct {
	SID  string
	Name string
	Path string
}

// WindowsHistorySource reads browser history databases for local Windows
// profiles. Detailed records are only persisted by the watcher when activity
// logging is enabled.
type WindowsHistorySource struct {
	tempDir string

	mu        sync.Mutex
	profiles  map[string]time.Time
	cache     map[string]profileCache
	userCache map[string]string
	users     []localUser
	usersAt   time.Time
}

// NewHistorySource builds a history source. baseTempDir should be the protected
// application data directory; an empty value falls back to the system temp
// directory.
func NewHistorySource(baseTempDir string) *WindowsHistorySource {
	if strings.TrimSpace(baseTempDir) == "" {
		baseTempDir = os.TempDir()
	}
	return &WindowsHistorySource{
		tempDir:   filepath.Join(baseTempDir, "browser-history"),
		profiles:  make(map[string]time.Time),
		cache:     make(map[string]profileCache),
		userCache: make(map[string]string),
	}
}

// BrowserUsers returns every local profile that can be selected for journaling.
func (s *WindowsHistorySource) BrowserUsers(ctx context.Context) ([]model.BrowserUser, error) {
	if s == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.discoverLocalUsers()
	if err != nil {
		return nil, sanitizeHistoryError(err)
	}
	out := make([]model.BrowserUser, 0, len(users))
	for _, user := range users {
		specs := browserProfilesForUser(user.Path, user.SID, user.Name, false)
		seen := make(map[string]struct{})
		browsers := make([]string, 0)
		for _, spec := range specs {
			if _, ok := seen[spec.Browser]; ok {
				continue
			}
			seen[spec.Browser] = struct{}{}
			browsers = append(browsers, spec.Browser)
		}
		sort.Strings(browsers)
		out = append(out, model.BrowserUser{
			SID: user.SID, UserName: user.Name, Browsers: browsers,
			Available: true,
		})
	}
	return out, nil
}

// RecentGameVisits returns domain-level visits for users with a running
// supported browser.
func (s *WindowsHistorySource) RecentGameVisits(ctx context.Context, since time.Time) ([]model.BrowserVisit, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	running := runningBrowserSIDs()
	visits, _, err := s.scanLocked(ctx, since, nil, running, false, gameHistoryQueryLimit)
	return visits, err
}

// RecentJournalScan returns detailed visits for selected users plus all
// running browser users, and profile counts for deletion auditing.
func (s *WindowsHistorySource) RecentJournalScan(ctx context.Context, since time.Time, selected map[string]struct{}) ([]model.BrowserVisit, []model.BrowserProfileInfo, error) {
	if s == nil {
		return nil, nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	running := runningBrowserSIDs()
	return s.scanLocked(ctx, since, selected, running, true, journalHistoryQueryLimit)
}

func (s *WindowsHistorySource) scanLocked(ctx context.Context, since time.Time, selected map[string]struct{}, running map[string]bool, detailed bool, limit int) ([]model.BrowserVisit, []model.BrowserProfileInfo, error) {
	specs, discoverErr := s.discoverProfilesLocked(running)
	if discoverErr != nil && len(specs) == 0 {
		return nil, nil, sanitizeHistoryError(discoverErr)
	}
	if len(selected) == 0 {
		selected = nil
	}
	var (
		visits   []model.BrowserVisit
		infos    []model.BrowserProfileInfo
		firstErr error
		readable int
	)
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return nil, infos, err
		}
		if !spec.Running {
			if _, ok := selected[strings.ToUpper(spec.UserSID)]; !ok {
				continue
			}
		}
		records, count, err := s.readProfileLocked(ctx, spec, since, detailed, detailed)
		info := model.BrowserProfileInfo{
			ProfileKey: spec.ProfileKey, Browser: spec.Browser,
			UserSID: spec.UserSID, UserName: spec.UserName,
			ProfileLabel: spec.ProfileLabel, HistoryCount: count,
			Readable: err == nil,
		}
		if err != nil {
			info.Error = sanitizeHistoryError(err).Error()
			if firstErr == nil {
				firstErr = err
			}
			infos = append(infos, info)
			continue
		}
		readable++
		infos = append(infos, info)
		for _, record := range records {
			if record.VisitedAt.Before(since) {
				continue
			}
			visit, ok := makeBrowserVisit(record, spec, detailed)
			if !ok {
				continue
			}
			visits = append(visits, visit)
		}
	}
	sort.Slice(visits, func(i, j int) bool {
		if !visits[i].VisitedAt.Equal(visits[j].VisitedAt) {
			return visits[i].VisitedAt.After(visits[j].VisitedAt)
		}
		if visits[i].Browser != visits[j].Browser {
			return visits[i].Browser < visits[j].Browser
		}
		return visits[i].Domain < visits[j].Domain
	})
	if readable == 0 && firstErr != nil {
		return nil, infos, sanitizeHistoryError(firstErr)
	}
	return visits, infos, nil
}

func (s *WindowsHistorySource) discoverProfilesLocked(running map[string]bool) ([]profileSpec, error) {
	now := time.Now().UTC()
	users, discoverErr := s.discoverLocalUsers()
	var specs []profileSpec
	for _, user := range users {
		specs = append(specs, browserProfilesForUser(user.Path, user.SID, user.Name, running[user.SID])...)
	}
	specs = dedupeProfiles(specs)
	for _, spec := range specs {
		s.profiles[spec.Path] = now
	}
	for path, lastSeen := range s.profiles {
		if now.Sub(lastSeen) > profileRetention {
			delete(s.profiles, path)
			delete(s.cache, path)
		}
	}
	return specs, discoverErr
}

func (s *WindowsHistorySource) discoverLocalUsers() ([]localUser, error) {
	if len(s.users) > 0 && time.Since(s.usersAt) < time.Minute {
		return append([]localUser(nil), s.users...), nil
	}
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, profileListKey, registry.QUERY_VALUE|registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		if len(s.users) > 0 {
			return append([]localUser(nil), s.users...), nil
		}
		return nil, fmt.Errorf("open profile list: %w", err)
	}
	defer key.Close()
	names, err := key.ReadSubKeyNames(-1)
	if err != nil {
		return nil, fmt.Errorf("enumerate profiles: %w", err)
	}
	var users []localUser
	for _, sid := range names {
		if skipProfileSID(sid) {
			continue
		}
		sub, err := registry.OpenKey(registry.LOCAL_MACHINE, profileListKey+`\`+sid, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		value, _, err := sub.GetStringValue("ProfileImagePath")
		_ = sub.Close()
		if err != nil {
			continue
		}
		profilePath := os.ExpandEnv(strings.TrimSpace(value))
		if profilePath == "" {
			continue
		}
		if info, err := os.Stat(profilePath); err != nil || !info.IsDir() {
			continue
		}
		if strings.Contains(strings.ToLower(profilePath), `\system32\`) {
			continue
		}
		base := strings.ToLower(filepath.Base(profilePath))
		if base == "public" || base == "default" || base == "default user" {
			continue
		}
		if _, err := os.Stat(filepath.Join(profilePath, "NTUSER.DAT")); err != nil {
			continue
		}
		name := s.userName(sid)
		users = append(users, localUser{SID: sid, Name: name, Path: profilePath})
	}
	sort.Slice(users, func(i, j int) bool {
		if users[i].Name != users[j].Name {
			return users[i].Name < users[j].Name
		}
		return users[i].SID < users[j].SID
	})
	s.users = append([]localUser(nil), users...)
	s.usersAt = time.Now()
	return users, nil
}

func (s *WindowsHistorySource) userName(sid string) string {
	if cached, ok := s.userCache[sid]; ok {
		return cached
	}
	name := sid
	if parsed, err := windows.StringToSid(sid); err == nil {
		if account, domain, _, err := parsed.LookupAccount(""); err == nil && account != "" {
			if domain != "" {
				name = domain + `\` + account
			} else {
				name = account
			}
		}
	}
	s.userCache[sid] = name
	return name
}

func skipProfileSID(sid string) bool {
	switch strings.ToUpper(sid) {
	case "S-1-5-18", "S-1-5-19", "S-1-5-20", "S-1-5-7":
		return true
	}
	return !strings.HasPrefix(strings.ToUpper(sid), "S-1-5-21-") &&
		!strings.HasPrefix(strings.ToUpper(sid), "S-1-12-1-")
}

func browserProfilesForUser(profilePath, sid, userName string, running bool) []profileSpec {
	type chromiumBase struct {
		root    string
		browser string
		label   string
	}
	bases := []chromiumBase{
		{filepath.Join(profilePath, "AppData", "Local", "Google", "Chrome", "User Data"), model.BrowserChrome, ""},
		{filepath.Join(profilePath, "AppData", "Local", "Microsoft", "Edge", "User Data"), model.BrowserEdge, ""},
		{filepath.Join(profilePath, "AppData", "Local", "BraveSoftware", "Brave-Browser", "User Data"), model.BrowserBrave, ""},
		{filepath.Join(profilePath, "AppData", "Local", "Vivaldi", "User Data"), model.BrowserVivaldi, ""},
		{filepath.Join(profilePath, "AppData", "Local", "Chromium", "User Data"), model.BrowserChromium, ""},
		{filepath.Join(profilePath, "AppData", "Roaming", "Opera Software", "Opera Stable"), model.BrowserOpera, "Opera Stable"},
		{filepath.Join(profilePath, "AppData", "Roaming", "Opera Software", "Opera GX Stable"), model.BrowserOpera, "Opera GX"},
	}
	var out []profileSpec
	for _, base := range bases {
		out = append(out, chromiumProfileSpecs(base.root, base.browser, base.label, sid, userName, running)...)
	}
	out = append(out, firefoxProfileSpecs(filepath.Join(profilePath, "AppData", "Roaming", "Mozilla", "Firefox"), sid, userName, running)...)
	return out
}

func chromiumProfileSpecs(root, browser, labelPrefix, sid, userName string, running bool) []profileSpec {
	var out []profileSpec
	add := func(historyPath, label string) {
		if !regularFile(historyPath) {
			return
		}
		out = append(out, profileSpec{
			Path: historyPath, Browser: browser, UserSID: sid, UserName: userName,
			ProfileKey: profileKeyForPath(historyPath), ProfileLabel: label, Running: running,
		})
	}
	if regularFile(filepath.Join(root, "History")) {
		add(filepath.Join(root, "History"), labelPrefix)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if !entry.IsDir() || skipChromiumProfileDir(entry.Name()) {
			continue
		}
		historyPath := filepath.Join(root, entry.Name(), "History")
		label := entry.Name()
		if labelPrefix != "" {
			label = labelPrefix + " " + entry.Name()
		}
		add(historyPath, label)
	}
	return out
}

func skipChromiumProfileDir(name string) bool {
	switch strings.ToLower(name) {
	case "guest profile", "system profile", "snapshots", "grshadercache", "graphitedawncache",
		"shadercache", "browsermetrics", "crashpad", "crowd deny", "meipreload",
		"optimizationhints", "safe browsing", "widevinecdm", "zxcvbndata", "first run":
		return true
	}
	return false
}

func firefoxProfileSpecs(root, sid, userName string, running bool) []profileSpec {
	seen := make(map[string]struct{})
	var out []profileSpec
	add := func(dir string) {
		places := filepath.Join(dir, "places.sqlite")
		clean := filepath.Clean(places)
		if _, ok := seen[strings.ToLower(clean)]; ok || !regularFile(places) {
			return
		}
		seen[strings.ToLower(clean)] = struct{}{}
		out = append(out, profileSpec{
			Path: clean, Browser: model.BrowserFirefox, UserSID: sid, UserName: userName,
			ProfileKey: profileKeyForPath(clean), ProfileLabel: filepath.Base(dir), Running: running,
		})
	}
	if data, err := os.ReadFile(filepath.Join(root, "profiles.ini")); err == nil { // #nosec G304 -- fixed profile path.
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(strings.ToLower(line), "path=") {
				continue
			}
			value := strings.TrimSpace(line[len("path="):])
			if value == "" {
				continue
			}
			if filepath.IsAbs(value) {
				add(value)
			} else {
				add(filepath.Join(root, value))
			}
		}
	}
	matches, _ := filepath.Glob(filepath.Join(root, "Profiles", "*", "places.sqlite"))
	for _, match := range matches {
		add(filepath.Dir(match))
	}
	return out
}

func (s *WindowsHistorySource) readProfileLocked(ctx context.Context, spec profileSpec, since time.Time, detailed, needCount bool) ([]historyRecord, int64, error) {
	info, err := os.Stat(spec.Path)
	if err != nil {
		return nil, 0, err
	}
	walMod, walSize := fileInfo(spec.Path + "-wal")
	cached, ok := s.cache[spec.Path]
	if ok && cached.Loaded && cached.ModTime.Equal(info.ModTime()) && cached.Size == info.Size() &&
		cached.WALMod.Equal(walMod) && cached.WALSize == walSize &&
		!since.Before(cached.Since) && (!detailed || cached.Detailed) {
		return cached.Records, cached.Count, nil
	}

	if err := os.MkdirAll(s.tempDir, 0o700); err != nil {
		return nil, 0, fmt.Errorf("create browser history temp directory: %w", err)
	}
	scanDir, err := os.MkdirTemp(s.tempDir, "scan-")
	if err != nil {
		return nil, 0, fmt.Errorf("create browser history snapshot directory: %w", err)
	}
	defer os.RemoveAll(scanDir)

	copyPath := filepath.Join(scanDir, filepath.Base(spec.Path))
	var copyErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		copyErr = copyHistoryFile(spec.Path, copyPath)
		if copyErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if copyErr != nil {
		return nil, 0, fmt.Errorf("copy browser history: %w", copyErr)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(spec.Path + suffix); err == nil {
			_ = copyHistoryFile(spec.Path+suffix, copyPath+suffix)
		}
	}

	limit := gameHistoryQueryLimit
	if detailed {
		limit = journalHistoryQueryLimit
	}
	var records []historyRecord
	if spec.Browser == model.BrowserFirefox {
		records, err = queryFirefoxHistory(ctx, copyPath, since, detailed, limit)
	} else {
		records, err = queryChromiumHistory(ctx, copyPath, since, detailed, limit)
	}
	if err != nil {
		return nil, 0, err
	}
	var count int64
	if needCount {
		count, err = queryHistoryCount(ctx, copyPath, spec.Browser)
		if err != nil {
			return nil, 0, err
		}
	}
	s.cache[spec.Path] = profileCache{
		ModTime: info.ModTime(), Size: info.Size(), WALMod: walMod, WALSize: walSize,
		Since: since, Detailed: detailed, Loaded: true, Records: records, Count: count,
	}
	return records, count, nil
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func fileInfo(path string) (time.Time, int64) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, 0
	}
	return info.ModTime(), info.Size()
}

func copyHistoryFile(source, destination string) error {
	src, err := os.Open(source) // #nosec G304 -- caller supplies a History path discovered under a user profile.
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- destination is under the protected temp directory.
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

func runningBrowserSIDs() map[string]bool {
	names := map[string]struct{}{
		"chrome.exe": {}, "msedge.exe": {}, "firefox.exe": {}, "brave.exe": {},
		"vivaldi.exe": {}, "opera.exe": {}, "opera_gx.exe": {}, "chromium.exe": {},
	}
	out := make(map[string]bool)
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return out
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return out
	}
	for {
		name := strings.ToLower(windows.UTF16ToString(entry.ExeFile[:]))
		if _, ok := names[name]; ok && entry.ProcessID != 0 {
			var session uint32
			if err := windows.ProcessIdToSessionId(entry.ProcessID, &session); err == nil && session != 0 {
				if sid := processUserSID(entry.ProcessID); sid != "" {
					out[sid] = true
				}
			}
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}
	return out
}

func processUserSID(pid uint32) string {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(handle)
	var token windows.Token
	if err := windows.OpenProcessToken(handle, windows.TOKEN_QUERY, &token); err != nil {
		return ""
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return ""
	}
	return user.User.Sid.String()
}

var (
	userProfilePathPattern = regexp.MustCompile(`(?i)([a-z]:\\users\\)[^\\]+`)
	userSIDPattern         = regexp.MustCompile(`(?i)S-1-[0-9-]+`)
	historyURLPattern      = regexp.MustCompile(`(?i)https?://\S+`)
)

func sanitizeHistoryError(err error) error {
	if err == nil {
		return nil
	}
	message := userProfilePathPattern.ReplaceAllString(err.Error(), `${1}<user>`)
	message = userSIDPattern.ReplaceAllString(message, "<sid>")
	message = historyURLPattern.ReplaceAllString(message, "<url>")
	return errors.New(message)
}

func dedupeProfiles(specs []profileSpec) []profileSpec {
	seen := make(map[string]profileSpec, len(specs))
	for _, spec := range specs {
		key := strings.ToLower(spec.Path)
		seen[key] = spec
	}
	out := make([]profileSpec, 0, len(seen))
	for _, spec := range seen {
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
