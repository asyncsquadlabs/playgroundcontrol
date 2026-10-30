package domains

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"bare", "poki.com", "poki.com"},
		{"uppercase and path", "https://WWW.Poki.com:443/games/play?x=1#frag", "poki.com"},
		{"subdomain", "https://play.crazygames.com/game", "crazygames.com"},
		{"google subdomain", "https://families.google.com/", "google.com"},
		{"multi label suffix", "https://games.example.co.uk/path", "example.co.uk"},
		{"trailing dot", "https://bloxd.io./", "bloxd.io"},
		{"idn", "https://BÜCHER.de/", "xn--bcher-kva.de"},
		{"http", "http://friv.com", "friv.com"},
		{"query only", "miniclip.com?x=1", "miniclip.com"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Normalize(test.input)
			if err != nil {
				t.Fatalf("Normalize(%q): %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("Normalize(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestNormalizeRejects(t *testing.T) {
	for _, input := range []string{
		"",
		"http://127.0.0.1/",
		"http://[::1]/",
		"http://localhost/",
		"http://printer.local/",
		"http://intranet/",
		"chrome://settings/",
		"edge://flags/",
		"file:///C:/Games/example.exe",
		"about:blank",
		"http://user:pass@example.com/",
		"com",
		"http://example.com:bad/",
	} {
		if got, err := Normalize(input); err == nil {
			t.Fatalf("Normalize(%q) = %q, want an error", input, got)
		}
	}
}

func TestSeedListCoversRequiredDomains(t *testing.T) {
	for _, required := range []string{
		"crazygames.com", "bloxd.io", "poki.com", "friv.com", "miniclip.com",
	} {
		if !IsSeed(required) {
			t.Fatalf("seed list is missing %s", required)
		}
	}
	for _, nonGame := range []string{"google.com", "youtube.com", "classroom.google.com"} {
		if IsSeed(nonGame) {
			t.Fatalf("seed list unexpectedly contains non-game domain %s", nonGame)
		}
	}
	if len(Seeds()) == 0 {
		t.Fatal("seed list is empty")
	}
}
