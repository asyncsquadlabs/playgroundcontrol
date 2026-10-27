// Package protection describes always-on safeguards that are independent of
// the parent's editable game list.
package protection

import (
	"strings"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

// Roblox is guarded by image name before launch. The list covers the web
// launcher/client chain, the desktop app shell, and the Microsoft Store alias.
// Roblox Studio is intentionally not included: it is a development tool rather
// than the Roblox player.
var robloxExecutables = []string{
	"RobloxPlayerBeta.exe",
	"RobloxPlayer.exe",
	"RobloxPlayerLauncher.exe",
	"RobloxPlayerInstaller.exe",
	"RobloxApp.exe",
	"Roblox.exe",
}

var robloxGames = []model.Game{
	{ID: "builtin-roblox-player-beta", Name: "Roblox Player", Executable: "RobloxPlayerBeta.exe"},
	{ID: "builtin-roblox-player", Name: "Roblox Player (legacy)", Executable: "RobloxPlayer.exe"},
	{ID: "builtin-roblox-launcher", Name: "Roblox Launcher", Executable: "RobloxPlayerLauncher.exe"},
	{ID: "builtin-roblox-installer", Name: "Roblox Installer", Executable: "RobloxPlayerInstaller.exe"},
	{ID: "builtin-roblox-app", Name: "Roblox App", Executable: "RobloxApp.exe"},
	{ID: "builtin-roblox-store", Name: "Roblox Store App", Executable: "Roblox.exe"},
}

// RobloxExecutables returns the launch-guard image names.
func RobloxExecutables() []string {
	return append([]string(nil), robloxExecutables...)
}

// RobloxGames returns synthetic game records used by the enforcement engine
// and displayed as the mandatory built-in protection.
func RobloxGames() []model.Game {
	return append([]model.Game(nil), robloxGames...)
}

// IsRobloxExecutable reports whether an executable basename is covered by the
// Roblox launch guard.
func IsRobloxExecutable(name string) bool {
	for _, executable := range robloxExecutables {
		if strings.EqualFold(strings.TrimSpace(name), executable) {
			return true
		}
	}
	return false
}

// MatchRobloxProcess provides a second line of defense for running processes.
// Besides the known image names, it recognizes renamed copies inside Roblox's
// version and Microsoft Store package directories. The latter cannot be
// blocked before launch by image name alone.
func MatchRobloxProcess(name, imagePath string) (model.Game, bool) {
	for _, game := range robloxGames {
		if strings.EqualFold(strings.TrimSpace(name), game.Executable) {
			return game, true
		}
	}
	if looksLikeRobloxInstallPath(imagePath) {
		return model.Game{
			ID:         "builtin-roblox-protected-path",
			Name:       "Roblox (protected install)",
			Executable: strings.TrimSpace(name),
		}, true
	}
	return model.Game{}, false
}

func looksLikeRobloxInstallPath(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	path = strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	return strings.Contains(path, `\appdata\local\roblox\versions\`) ||
		strings.Contains(path, `\roblox\versions\`) ||
		strings.Contains(path, `\windowsapps\robloxcorporation.roblox_`)
}
