package windowsapp

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/buildinfo"
)

type installMode int

const (
	modeAuto installMode = iota
	modeFresh
	modeRepair
	modeFullReset
)

type recoveryAction int

const (
	recoveryExit recoveryAction = iota
	recoveryRepair
	recoveryReset
)

type setupOutcome int

const (
	outcomeCanceled setupOutcome = iota
	outcomeInstalled
	outcomeAlreadyRunning
	outcomeError
)

// installationInfo is the platform-independent summary used to choose the
// installer path.
type installationInfo struct {
	ServiceRegistered bool
	ServiceState      string
	InstalledExe      bool
	DataDir           bool
	UninstallEntry    bool
	InstalledVersion  string
	Healthy           bool
}

func (i installationInfo) Present() bool {
	return i.ServiceRegistered || i.InstalledExe || i.DataDir || i.UninstallEntry
}

type installBackend interface {
	Inspect() (installationInfo, error)
	Install(rep installReporter, mode installMode) error
	FullReset(rep installReporter) error
	TryStart(rep installReporter) error
}

type setupUI interface {
	installReporter
	MarkSuccess()
	MarkFailure()
	ClearTerminal()
	AskRecovery(reason string) recoveryAction
	ConfirmFullReset() bool
	WaitForFinish()
}

// runInstallFlow owns the user-visible decision tree. All platform-specific
// work is behind installBackend, which keeps the recovery paths testable on
// non-Windows hosts.
func runInstallFlow(backend installBackend, ui setupUI, force installMode) setupOutcome {
	ui.StartStep("Inspecting the current installation")
	info, err := backend.Inspect()
	if err != nil {
		return failAndFinish(ui, fmt.Errorf("inspect installation: %w", err))
	}
	ui.CompleteStep("Installation inspection complete")

	switch force {
	case modeFullReset:
		if !ui.ConfirmFullReset() {
			ui.Info("Full reset canceled. No changes were made.")
			ui.MarkFailure()
			ui.WaitForFinish()
			return outcomeCanceled
		}
		return resetAndInstall(backend, ui, "Full reset requested.")
	case modeRepair:
		return installWithRecovery(backend, ui, modeRepair, "Repair requested.")
	}

	if !info.Present() {
		return installWithRecovery(backend, ui, modeFresh, "")
	}

	installed := strings.TrimSpace(info.InstalledVersion)
	current := strings.TrimSpace(buildinfo.Version)
	if installed != "" && current != "" && !strings.EqualFold(current, "dev") && compareVersions(current, installed) < 0 {
		return failAndFinish(ui, fmt.Errorf("a newer version (%s) is already installed; run that installer instead of downgrading to %s", installed, current))
	}

	sameVersion := installed != "" && current != "" && (strings.EqualFold(installed, current) || strings.EqualFold(current, "dev"))
	if sameVersion {
		if info.Healthy {
			ui.Info("Playground Control is already installed and the dashboard is responding.")
			ui.MarkSuccess()
			ui.WaitForFinish()
			return outcomeAlreadyRunning
		}
		ui.StartStep("Starting the existing service")
		if err := backend.TryStart(ui); err == nil {
			ui.CompleteStep("The existing service started and the dashboard is responding")
			ui.MarkSuccess()
			ui.WaitForFinish()
			return outcomeAlreadyRunning
		} else {
			ui.Failure(err.Error())
			return askAndRecover(backend, ui, fmt.Sprintf("The installed service did not start.\n\n%s", err))
		}
	}

	reason := fmt.Sprintf("Version %s is installed, but this installer is %s.", installed, current)
	if installed == "" {
		reason = fmt.Sprintf("An existing Playground Control installation was found, but its version could not be determined. This installer is %s.", current)
	}
	if info.Healthy {
		reason += "\nThe existing service is running. Upgrading will preserve the PIN, games, play state, and history."
	} else {
		reason += "\nThe existing installation is not healthy. Repair will preserve the PIN, games, play state, and history."
	}
	return askAndRecover(backend, ui, reason)
}

func installWithRecovery(backend installBackend, ui setupUI, mode installMode, reason string) setupOutcome {
	if reason != "" {
		ui.Info(reason)
	}
	if err := backend.Install(ui, mode); err != nil {
		ui.Failure(err.Error())
		return askAndRecover(backend, ui, err.Error())
	}
	return finishSuccess(ui, "Playground Control is installed and the dashboard is responding.")
}

func resetAndInstall(backend installBackend, ui setupUI, reason string) setupOutcome {
	ui.Info(reason)
	ui.StartStep("Cleaning up the previous installation and data")
	if err := backend.FullReset(ui); err != nil {
		ui.Failure(err.Error())
		return askAndRecover(backend, ui, err.Error())
	}
	ui.CompleteStep("Previous installation and data removed")
	if err := backend.Install(ui, modeFresh); err != nil {
		ui.Failure(err.Error())
		return askAndRecover(backend, ui, err.Error())
	}
	return finishSuccess(ui, "Playground Control was reset, installed, and the dashboard is responding.")
}

func askAndRecover(backend installBackend, ui setupUI, reason string) setupOutcome {
	for {
		action := ui.AskRecovery(reason)
		switch action {
		case recoveryExit:
			ui.Info("Setup canceled. No further changes were made.")
			ui.MarkFailure()
			ui.WaitForFinish()
			return outcomeCanceled
		case recoveryRepair:
			ui.ClearTerminal()
			if err := backend.Install(ui, modeRepair); err != nil {
				ui.Failure(err.Error())
				reason = err.Error()
				continue
			}
			return finishSuccess(ui, "Repair completed and the dashboard is responding.")
		case recoveryReset:
			if !ui.ConfirmFullReset() {
				continue
			}
			ui.ClearTerminal()
			if err := backend.FullReset(ui); err != nil {
				ui.Failure(err.Error())
				reason = err.Error()
				continue
			}
			if err := backend.Install(ui, modeFresh); err != nil {
				ui.Failure(err.Error())
				reason = err.Error()
				continue
			}
			return finishSuccess(ui, "Full reset completed and the dashboard is responding.")
		default:
			return outcomeError
		}
	}
}

func finishSuccess(ui setupUI, message string) setupOutcome {
	ui.CompleteStep(message)
	ui.MarkSuccess()
	ui.WaitForFinish()
	return outcomeInstalled
}

func failAndFinish(ui setupUI, err error) setupOutcome {
	ui.Failure(err.Error())
	ui.MarkFailure()
	ui.WaitForFinish()
	return outcomeError
}

// compareVersions compares dotted numeric versions. Non-numeric or dev
// versions compare equal so an unknown build never blocks recovery.
func compareVersions(a, b string) int {
	parse := func(value string) ([]int, bool) {
		value = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(value, "v"), "V"))
		if value == "" || strings.EqualFold(value, "dev") {
			return nil, false
		}
		parts := strings.Split(value, ".")
		out := make([]int, len(parts))
		for i, part := range parts {
			number, err := strconv.Atoi(part)
			if err != nil || number < 0 {
				return nil, false
			}
			out[i] = number
		}
		return out, true
	}
	left, leftOK := parse(a)
	right, rightOK := parse(b)
	if !leftOK || !rightOK {
		return 0
	}
	length := len(left)
	if len(right) > length {
		length = len(right)
	}
	for i := 0; i < length; i++ {
		var lv, rv int
		if i < len(left) {
			lv = left[i]
		}
		if i < len(right) {
			rv = right[i]
		}
		if lv < rv {
			return -1
		}
		if lv > rv {
			return 1
		}
	}
	return 0
}
