package windowsapp

import (
	"errors"
	"fmt"
)

type cleanupOps interface {
	StopService() error
	DeleteService() error
	RemoveRobloxGuard() error
	RemoveFirewallRule() error
	RemoveUninstallEntry() error
	RemoveEventLogSource() error
	RemoveInstallFiles() error
	RemoveDataDirectory() error
}

type cleanupOptions struct {
	RemoveData        bool
	RemoveInstallFile bool
}

type cleanupStep struct {
	label string
	run   func() error
}

// runCleanup attempts every requested cleanup step, reports each result, and
// joins all failures so one locked artifact cannot hide the rest.
func runCleanup(ops cleanupOps, options cleanupOptions, rep installReporter) error {
	if rep == nil {
		rep = noopReporter{}
	}
	steps := []cleanupStep{
		{label: "Stop the Playground Control service", run: ops.StopService},
		{label: "Delete the service registration", run: ops.DeleteService},
		{label: "Remove Roblox launch-guard entries", run: ops.RemoveRobloxGuard},
		{label: "Remove the Windows Firewall rule", run: ops.RemoveFirewallRule},
		{label: "Remove the uninstall entry", run: ops.RemoveUninstallEntry},
		{label: "Remove the Windows Event Log source", run: ops.RemoveEventLogSource},
	}
	if options.RemoveInstallFile {
		steps = append(steps, cleanupStep{label: "Remove installed program files", run: ops.RemoveInstallFiles})
	}
	if options.RemoveData {
		steps = append(steps, cleanupStep{label: "Remove application data and history", run: ops.RemoveDataDirectory})
	}

	rep.SetProgress(0, len(steps))
	var errs []error
	for i, step := range steps {
		rep.StartStep(step.label)
		if err := step.run(); err != nil {
			wrapped := fmt.Errorf("%s: %w", step.label, err)
			rep.Failure(wrapped.Error())
			errs = append(errs, wrapped)
		} else {
			rep.CompleteStep(step.label)
		}
		rep.SetProgress(i+1, len(steps))
	}
	return errors.Join(errs...)
}
