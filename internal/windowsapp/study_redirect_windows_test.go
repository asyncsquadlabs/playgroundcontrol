//go:build windows

package windowsapp

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestStudySearchURL(t *testing.T) {
	const want = "https://www.google.com/search?q=gere+um+guia+de+estudos+para+passar+na+prova+do+ifisc+garopaba+2027"
	if got := studySearchURL(); got != want {
		t.Fatalf("studySearchURL() = %q, want %q", got, want)
	}

	parsed, err := url.Parse(studySearchURL())
	if err != nil {
		t.Fatalf("parse study search URL: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "www.google.com" || parsed.Path != "/search" {
		t.Fatalf("unexpected search URL parts: %+v", parsed)
	}
	if got := parsed.Query().Get("q"); got != studySearchQuery {
		t.Fatalf("query = %q, want %q", got, studySearchQuery)
	}
}

func TestNormalizeChromeAppPath(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain path",
			input: `C:\Program Files\Google\Chrome\Application\chrome.exe`,
			want:  `C:\Program Files\Google\Chrome\Application\chrome.exe`,
		},
		{
			name:  "quoted path",
			input: `"C:\Program Files\Google\Chrome\Application\chrome.exe"`,
			want:  `C:\Program Files\Google\Chrome\Application\chrome.exe`,
		},
		{
			name:  "quoted path with arguments",
			input: `"C:\Program Files\Google\Chrome\Application\chrome.exe" --single-argument`,
			want:  `C:\Program Files\Google\Chrome\Application\chrome.exe`,
		},
		{name: "wrong extension", input: `C:\Program Files\Google\Chrome\Application\chrome.txt`, want: ""},
		{name: "relative path", input: `chrome.exe`, want: ""},
		{name: "unterminated quote", input: `"C:\Program Files\Google\Chrome\Application\chrome.exe`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeChromeAppPath(test.input); got != test.want {
				t.Fatalf("normalizeChromeAppPath(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestFirstExistingExecutable(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.exe")
	second := filepath.Join(dir, "second.exe")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("stub"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	got, ok := firstExistingExecutable([]string{
		"",
		filepath.Join(dir, "missing.exe"),
		dir,
		first,
		second,
	})
	if !ok || got != filepath.Clean(first) {
		t.Fatalf("firstExistingExecutable() = %q, %t; want %q, true", got, ok, filepath.Clean(first))
	}

	if got, ok := firstExistingExecutable([]string{"", dir, filepath.Join(dir, "missing.exe")}); ok {
		t.Fatalf("firstExistingExecutable() = %q, true; want no executable", got)
	}
}

func TestOpenStudySearchWithFallback(t *testing.T) {
	target := studySearchURL()
	tests := []struct {
		name             string
		chromePath       string
		chromeFound      bool
		chromeErr        error
		defaultErr       error
		wantErr          bool
		wantChromeCalls  int
		wantDefaultCalls int
	}{
		{
			name:             "chrome succeeds",
			chromePath:       `C:\Chrome\chrome.exe`,
			chromeFound:      true,
			wantChromeCalls:  1,
			wantDefaultCalls: 0,
		},
		{
			name:             "chrome fails and default succeeds",
			chromePath:       `C:\Chrome\chrome.exe`,
			chromeFound:      true,
			chromeErr:        errors.New("chrome failed"),
			wantChromeCalls:  1,
			wantDefaultCalls: 1,
		},
		{
			name:             "chrome missing and default succeeds",
			wantChromeCalls:  0,
			wantDefaultCalls: 1,
		},
		{
			name:             "chrome and default fail",
			chromePath:       `C:\Chrome\chrome.exe`,
			chromeFound:      true,
			chromeErr:        errors.New("chrome failed"),
			defaultErr:       errors.New("default failed"),
			wantErr:          true,
			wantChromeCalls:  1,
			wantDefaultCalls: 1,
		},
		{
			name:             "chrome missing and default fails",
			defaultErr:       errors.New("default failed"),
			wantErr:          true,
			wantChromeCalls:  0,
			wantDefaultCalls: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chromeCalls := 0
			defaultCalls := 0
			findChrome := func() (string, bool) {
				return test.chromePath, test.chromeFound
			}
			launch := func(executable, gotTarget string) error {
				chromeCalls++
				if executable != test.chromePath {
					t.Fatalf("chrome executable = %q, want %q", executable, test.chromePath)
				}
				if gotTarget != target {
					t.Fatalf("chrome target = %q, want %q", gotTarget, target)
				}
				return test.chromeErr
			}
			openDefault := func(gotTarget string) error {
				defaultCalls++
				if gotTarget != target {
					t.Fatalf("default target = %q, want %q", gotTarget, target)
				}
				return test.defaultErr
			}

			err := openStudySearchWith(target, findChrome, launch, openDefault)
			if (err != nil) != test.wantErr {
				t.Fatalf("openStudySearchWith() error = %v, wantErr %t", err, test.wantErr)
			}
			if chromeCalls != test.wantChromeCalls {
				t.Fatalf("chrome calls = %d, want %d", chromeCalls, test.wantChromeCalls)
			}
			if defaultCalls != test.wantDefaultCalls {
				t.Fatalf("default browser calls = %d, want %d", defaultCalls, test.wantDefaultCalls)
			}
		})
	}
}
