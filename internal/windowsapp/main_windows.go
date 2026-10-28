//go:build windows

package windowsapp

import (
	"os"
	"strings"

	"golang.org/x/sys/windows/svc"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/buildinfo"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/platform"
)

// Main dispatches the single binary's installer, service, dashboard and
// recovery modes.
func Main() {
	args := os.Args[1:]
	switch {
	case hasArg(args, "--service"):
		runWindowsService()
	case hasArg(args, "--close-helper"):
		pidsValue, _ := argAfter(args, "--pids")
		pids, err := platform.ParsePIDList(pidsValue)
		if err == nil {
			err = platform.RunCloseHelper(pids)
		}
		if err != nil {
			os.Exit(1)
		}
	case hasArg(args, "--deny-launch"):
		commandDenyLaunch()
	case hasArg(args, "--uninstall-cleanup"):
		commandUninstallCleanup()
	case hasArg(args, "--uninstall"):
		commandUninstall()
	case hasArg(args, "--reset-admin"):
		commandResetAdmin()
	case hasArg(args, "--setup"):
		commandSetup()
	case hasArg(args, "--start"):
		commandStart()
	case hasArg(args, "--install"):
		commandInstall()
	case hasArg(args, "--repair"):
		commandRepair()
	case hasArg(args, "--reinstall"):
		commandReinstall()
	case hasArg(args, "--version"):
		showInfo("Playground Control " + buildinfo.Version)
	case hasArg(args, "--help"):
		showInfo("Playground Control " + buildinfo.Version + "\n\nDouble-click to install or open the dashboard.\n\nLocal administrator recovery:\n  --setup         recreate the first-run setup link\n  --reset-admin   reset the administrator PIN\n  --repair        reinstall components and keep all data\n  --reinstall     full reset and fresh installation\n  --uninstall     remove the application and its settings")
	default:
		commandInteractive()
	}
}

func runWindowsService() {
	err := svc.Run(serviceName, &serviceHandler{})
	if err != nil {
		logger := newServiceLogger()
		logger.Errorf("svc.Run failed: %v", err)
		logger.Close()
	}
}

func hasArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func argAfter(args []string, name string) (string, bool) {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1], true
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"="), true
		}
	}
	return "", false
}
