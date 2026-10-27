package model

import (
	"strings"
	"testing"
)

func TestNormalizeGame(t *testing.T) {
	game, err := NormalizeGame(Game{
		Name:       "  Example Game  ",
		Executable: "  Game.EXE ",
		ImagePath:  `C:/Games/Example/game.exe`,
	})
	if err != nil {
		t.Fatalf("NormalizeGame: %v", err)
	}
	if game.Name != "Example Game" {
		t.Fatalf("name = %q", game.Name)
	}
	if game.Executable != "Game.EXE" {
		t.Fatalf("executable = %q", game.Executable)
	}
	if game.ImagePath != `C:\Games\Example\game.exe` {
		t.Fatalf("image path = %q", game.ImagePath)
	}
	if game.ID == "" || len(game.ID) != 32 {
		t.Fatalf("unexpected ID %q", game.ID)
	}
	if game.AddedAt.IsZero() {
		t.Fatal("AddedAt was not set")
	}
}

func TestNormalizeGameRejectsUnsafeInput(t *testing.T) {
	tests := []struct {
		name string
		game Game
	}{
		{"empty name", Game{Name: "", Executable: "game.exe"}},
		{"path as exe", Game{Name: "Game", Executable: `C:\game.exe`}},
		{"wrong extension", Game{Name: "Game", Executable: "game.com"}},
		{"critical process", Game{Name: "Explorer", Executable: "explorer.exe"}},
		{"own process", Game{Name: "Control", Executable: "PlaygroundControl.exe"}},
		{"relative path", Game{Name: "Game", Executable: "game.exe", ImagePath: `games\game.exe`}},
		{"wrong path basename", Game{Name: "Game", Executable: "game.exe", ImagePath: `C:\Games\other.exe`}},
		{"alternate stream", Game{Name: "Game", Executable: "game.exe", ImagePath: `C:\Games\game.exe:secret`}},
		{"control character", Game{Name: "Game\x00", Executable: "game.exe"}},
		{"invalid filename character", Game{Name: "Game", Executable: "game?.exe"}},
		{"invalid path character", Game{Name: "Game", Executable: "game.exe", ImagePath: `C:\Games\g*me.exe`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NormalizeGame(test.game); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestNormalizeGameAllowsUNCAndLongPath(t *testing.T) {
	game, err := NormalizeGame(Game{
		Name:       "Network Game",
		Executable: "game.exe",
		ImagePath:  `\\server\share\game.exe`,
	})
	if err != nil {
		t.Fatalf("UNC path: %v", err)
	}
	if game.ImagePath != `\\server\share\game.exe` {
		t.Fatalf("path = %q", game.ImagePath)
	}
	game, err = NormalizeGame(Game{
		Name:       "Long Path Game",
		Executable: "game.exe",
		ImagePath:  `\\?\C:\Games\game.exe`,
	})
	if err != nil {
		t.Fatalf("long path: %v", err)
	}
	if game.ImagePath != `C:\Games\game.exe` {
		t.Fatalf("long path normalized to %q", game.ImagePath)
	}
}

func TestGameMatches(t *testing.T) {
	basename := Game{Name: "Game", Executable: "Game.exe"}
	if !basename.Matches("game.EXE", `D:\Other\game.exe`) {
		t.Fatal("basename match should be case-insensitive and path-independent")
	}
	if basename.Matches("other.exe", `D:\Other\other.exe`) {
		t.Fatal("unexpected basename match")
	}

	exact := Game{Name: "Game", Executable: "game.exe", ImagePath: `C:\Games\game.exe`}
	if !exact.Matches("game.exe", `c:\games\GAME.EXE`) {
		t.Fatal("exact path match should be case-insensitive")
	}
	if !exact.Matches("", `\\?\C:\Games\game.exe`) {
		t.Fatal("process path normalization should strip the long-path prefix")
	}
	if exact.Matches("game.exe", `D:\Games\game.exe`) {
		t.Fatal("path-specific game matched the wrong directory")
	}
	if exact.Matches("game.exe", "") {
		t.Fatal("path-specific game matched when the process path was unavailable")
	}
}

func TestGameValidationAndKeys(t *testing.T) {
	first, err := NormalizeGame(Game{Name: "One", Executable: "game.exe", ImagePath: `C:\One\game.exe`})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NormalizeGame(Game{Name: "Two", Executable: "GAME.EXE", ImagePath: `c:\one\GAME.EXE`})
	if err != nil {
		t.Fatal(err)
	}
	if GameKey(first) != GameKey(second) {
		t.Fatal("expected case-insensitive game key equality")
	}
	cfg := DefaultConfig()
	cfg.Games = []Game{first, second}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate validation error, got %v", err)
	}
}
