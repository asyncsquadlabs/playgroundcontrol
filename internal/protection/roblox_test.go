package protection

import "testing"

func TestRobloxExecutablesCoverPlayerChain(t *testing.T) {
	for _, name := range []string{
		"RobloxPlayerBeta.exe",
		"robloxplayerbeta.EXE",
		"RobloxPlayerLauncher.exe",
		"RobloxPlayerInstaller.exe",
		"RobloxApp.exe",
		"Roblox.exe",
	} {
		if !IsRobloxExecutable(name) {
			t.Fatalf("%q is not covered by the launch guard", name)
		}
	}
	if IsRobloxExecutable("RobloxStudioBeta.exe") {
		t.Fatal("Roblox Studio should not be silently blocked")
	}
	if IsRobloxExecutable("notroblox.exe") {
		t.Fatal("unrelated executable matched the guard")
	}
}

func TestMatchRobloxProcessByPath(t *testing.T) {
	game, ok := MatchRobloxProcess("renamed-client.exe", `C:\Users\player\AppData\Local\Roblox\Versions\version-abc\renamed-client.exe`)
	if !ok {
		t.Fatal("renamed Roblox client in the install directory was not matched")
	}
	if game.ID == "" || game.Executable != "renamed-client.exe" {
		t.Fatalf("unexpected synthetic game: %+v", game)
	}

	game, ok = MatchRobloxProcess("whatever.exe", `C:\Program Files\WindowsApps\ROBLOXCorporation.ROBLOX_55nm5eh3cm0pr\whatever.exe`)
	if !ok || game.ID != "builtin-roblox-protected-path" {
		t.Fatalf("Store package path was not matched: %+v, %t", game, ok)
	}

	if _, ok := MatchRobloxProcess("game.exe", `C:\Games\Roblox\something\game.exe`); ok {
		t.Fatal("unrelated path matched the Roblox protection")
	}
}

func TestMatchRobloxProcessByKnownImage(t *testing.T) {
	game, ok := MatchRobloxProcess("RobloxPlayerBeta.exe", `D:\Elsewhere\RobloxPlayerBeta.exe`)
	if !ok || game.ID != "builtin-roblox-player-beta" {
		t.Fatalf("known Roblox image not matched: %+v, %t", game, ok)
	}
}
