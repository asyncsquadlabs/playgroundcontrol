//go:build windows

package browserwatch

import (
	"context"
	"database/sql"
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

	_ "modernc.org/sqlite" // register the pure-Go SQLite driver

	"github.com/asyncsquadlabs/playgroundcontrol/internal/domains"
)

const (
	chromeEpochOffsetMicros = int64(11_644_473_600_000_000)
	historyQueryLimit       = 5_000
	profileRetention        = 5 * time.Minute
)

type profileSpec struct {
	Path    string
	Browser string
}

type profileCache struct {
	ModTime time.Time
	Size    int64
	WALMod  time.Time
	WALSize int64
	Loaded  bool
	Visits  []Visit
}

// WindowsHistorySource reads Chrome and Edge History SQLite databases for
// interactive users with a running browser. It never exposes full URLs to
// callers.
type WindowsHistorySource struct {
	tempDir string

	mu          sync.Mutex
	profiles    map[string]time.Time
	cache       map[string]profileCache
	sidProfiles map[string]string
}

// NewHistorySource builds a history source. baseTempDir should be the protected
// application data directory; an empty value falls back to the system temp
// directory.
func NewHistorySource(baseTempDir string) *WindowsHistorySource {
	if strings.TrimSpace(baseTempDir) == "" {
		baseTempDir = os.TempDir()
	}
	return &WindowsHistorySource{
		tempDir:     filepath.Join(baseTempDir, "browser-history"),
		profiles:    make(map[string]time.Time),
		cache:       make(map[string]profileCache),
		sidProfiles: make(map[string]string),
	}
}

// RecentVisits returns unique normalized domains visited since the cutoff.
func (s *WindowsHistorySource) RecentVisits(ctx context.Context, since time.Time) ([]Visit, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	specs, discoverErr := s.discoverProfiles()
	for _, spec := range specs {
		s.profiles[spec.Path] = now
	}
	for path, lastSeen := range s.profiles {
		if now.Sub(lastSeen) > profileRetention {
			delete(s.profiles, path)
			delete(s.cache, path)
		}
	}
	if len(s.profiles) == 0 {
		return nil, sanitizeHistoryError(discoverErr)
	}

	paths := make([]string, 0, len(s.profiles))
	for path := range s.profiles {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var (
		visits   []Visit
		firstErr error
		loaded   int
	)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		profileVisits, err := s.readProfile(ctx, path, since)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		loaded++
		for _, visit := range profileVisits {
			if visit.VisitedAt.Before(since) {
				continue
			}
			visits = append(visits, visit)
		}
	}
	deduped := dedupeVisits(visits)
	if loaded == 0 && firstErr != nil {
		return nil, sanitizeHistoryError(firstErr)
	}
	return deduped, nil
}

func (s *WindowsHistorySource) discoverProfiles() ([]profileSpec, error) {
	var (
		specs    []profileSpec
		firstErr error
	)
	seenSIDs := make(map[string]struct{})
	for _, processName := range []string{"chrome.exe", "msedge.exe"} {
		sids, err := processUserSIDs(processName)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		for _, sid := range sids {
			if _, duplicate := seenSIDs[sid]; duplicate {
				continue
			}
			seenSIDs[sid] = struct{}{}
			profile, ok := s.sidProfiles[sid]
			if !ok {
				profile, err = profilePathForSID(sid)
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				s.sidProfiles[sid] = profile
			}
			specs = append(specs, browserProfiles(profile)...)
		}
	}
	return dedupeProfiles(specs), firstErr
}

func browserProfiles(profilePath string) []profileSpec {
	bases := []profileSpec{
		{Path: filepath.Join(profilePath, "AppData", "Local", "Google", "Chrome", "User Data"), Browser: "chrome"},
		{Path: filepath.Join(profilePath, "AppData", "Local", "Microsoft", "Edge", "User Data"), Browser: "edge"},
	}
	var out []profileSpec
	for _, base := range bases {
		entries, err := os.ReadDir(base.Path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			history := filepath.Join(base.Path, entry.Name(), "History")
			info, err := os.Stat(history)
			if err == nil && info.Mode().IsRegular() {
				out = append(out, profileSpec{Path: history, Browser: base.Browser})
			}
		}
	}
	return out
}

func processUserSIDs(processName string) ([]string, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("create process snapshot: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, fmt.Errorf("read first process: %w", err)
	}
	var out []string
	for {
		if strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), processName) && entry.ProcessID != 0 {
			var session uint32
			if err := windows.ProcessIdToSessionId(entry.ProcessID, &session); err == nil && session != 0 {
				handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, entry.ProcessID)
				if err == nil {
					var token windows.Token
					if err := windows.OpenProcessToken(handle, windows.TOKEN_QUERY, &token); err == nil {
						if user, err := token.GetTokenUser(); err == nil && user != nil && user.User.Sid != nil {
							out = append(out, user.User.Sid.String())
						}
						token.Close()
					}
					windows.CloseHandle(handle)
				}
			}
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return out, fmt.Errorf("read next process: %w", err)
		}
	}
	return out, nil
}

func profilePathForSID(sid string) (string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList\`+sid, registry.QUERY_VALUE)
	if err != nil {
		return "", fmt.Errorf("open profile list entry: %w", err)
	}
	defer key.Close()
	value, _, err := key.GetStringValue("ProfileImagePath")
	if err != nil {
		return "", fmt.Errorf("read profile image path: %w", err)
	}
	value = os.ExpandEnv(strings.TrimSpace(value))
	if value == "" {
		return "", errors.New("profile image path is empty")
	}
	return value, nil
}

func (s *WindowsHistorySource) readProfile(ctx context.Context, historyPath string, since time.Time) ([]Visit, error) {
	info, err := os.Stat(historyPath)
	if err != nil {
		return nil, err
	}
	walMod, walSize := fileInfo(historyPath + "-wal")
	cached, ok := s.cache[historyPath]
	if ok && cached.Loaded && cached.ModTime.Equal(info.ModTime()) && cached.Size == info.Size() &&
		cached.WALMod.Equal(walMod) && cached.WALSize == walSize {
		return cached.Visits, nil
	}

	if err := os.MkdirAll(s.tempDir, 0o700); err != nil {
		return nil, fmt.Errorf("create browser history temp directory: %w", err)
	}
	scanDir, err := os.MkdirTemp(s.tempDir, "scan-")
	if err != nil {
		return nil, fmt.Errorf("create browser history snapshot directory: %w", err)
	}
	defer os.RemoveAll(scanDir)

	copyPath := filepath.Join(scanDir, "History")
	if err := copyHistoryFile(historyPath, copyPath); err != nil {
		return nil, fmt.Errorf("copy browser history: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		source := historyPath + suffix
		if _, err := os.Stat(source); err == nil {
			_ = copyHistoryFile(source, copyPath+suffix)
		}
	}

	browser := "chrome"
	if strings.Contains(strings.ToLower(historyPath), strings.ToLower(filepath.Join("Microsoft", "Edge"))) {
		browser = "edge"
	}
	visits, err := queryHistory(ctx, copyPath, browser, since)
	if err != nil {
		return nil, err
	}
	s.cache[historyPath] = profileCache{
		ModTime: info.ModTime(),
		Size:    info.Size(),
		WALMod:  walMod,
		WALSize: walSize,
		Loaded:  true,
		Visits:  visits,
	}
	return visits, nil
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

func queryHistory(ctx context.Context, path, browser string, since time.Time) ([]Visit, error) {
	// Open the copied database read-write so SQLite can recover any WAL pages
	// into the snapshot. The copy lives under the protected data directory and
	// is removed after the query.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open browser history snapshot: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	cutoff := since.UTC().UnixMicro() + chromeEpochOffsetMicros
	if since.IsZero() {
		cutoff = 0
	}
	var visits []Visit
	rows, err := db.QueryContext(ctx,
		`SELECT v.visit_time, u.url FROM visits v JOIN urls u ON v.url = u.id
		 WHERE v.visit_time >= ? ORDER BY v.visit_time DESC LIMIT ?`, cutoff, historyQueryLimit)
	if err != nil {
		if rows != nil {
			rows.Close()
		}
		// Older or unusual profiles may not have the visits table; fall back to
		// the URL last-visit timestamp.
		rows, err = db.QueryContext(ctx,
			`SELECT last_visit_time, url FROM urls
			 WHERE last_visit_time >= ? ORDER BY last_visit_time DESC LIMIT ?`, cutoff, historyQueryLimit)
		if err != nil {
			return nil, fmt.Errorf("query browser history: %w", err)
		}
	}
	defer rows.Close()
	for rows.Next() {
		var chromeTime int64
		var rawURL string
		if err := rows.Scan(&chromeTime, &rawURL); err != nil {
			return nil, fmt.Errorf("read browser history row: %w", err)
		}
		domain, err := domains.Normalize(rawURL)
		if err != nil {
			continue
		}
		visits = append(visits, Visit{
			Browser:   browser,
			Domain:    domain,
			VisitedAt: time.UnixMicro(chromeTime - chromeEpochOffsetMicros).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read browser history: %w", err)
	}
	return visits, nil
}

var (
	userProfilePathPattern = regexp.MustCompile(`(?i)([a-z]:\\users\\)[^\\]+`)
	userSIDPattern         = regexp.MustCompile(`(?i)S-1-[0-9-]+`)
)

func sanitizeHistoryError(err error) error {
	if err == nil {
		return nil
	}
	message := userProfilePathPattern.ReplaceAllString(err.Error(), `${1}<user>`)
	message = userSIDPattern.ReplaceAllString(message, "<sid>")
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

func dedupeVisits(visits []Visit) []Visit {
	best := make(map[string]Visit, len(visits))
	for _, visit := range visits {
		key := visit.Browser + "\x00" + visit.Domain
		if existing, ok := best[key]; !ok || visit.VisitedAt.After(existing.VisitedAt) {
			best[key] = visit
		}
	}
	out := make([]Visit, 0, len(best))
	for _, visit := range best {
		out = append(out, visit)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].VisitedAt.Equal(out[j].VisitedAt) {
			return out[i].VisitedAt.After(out[j].VisitedAt)
		}
		return out[i].Domain < out[j].Domain
	})
	return out
}
