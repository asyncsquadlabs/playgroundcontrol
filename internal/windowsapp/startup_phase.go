//go:build windows

package windowsapp

// Startup checkpoints are reported to the Windows Service Control Manager and
// mapped to human-readable labels by the installer progress window.
const (
	startupPhaseDataDir uint32 = iota + 1
	startupPhaseDatabase
	startupPhaseAuth
	startupPhaseGuard
	startupPhaseListener
	startupPhaseReady
)

type startupPhase struct {
	Checkpoint uint32
	Label      string
}

var startupPhases = []startupPhase{
	{Checkpoint: startupPhaseDataDir, Label: "Prepare the protected data directory"},
	{Checkpoint: startupPhaseDatabase, Label: "Open and migrate the database"},
	{Checkpoint: startupPhaseAuth, Label: "Load administrator authentication"},
	{Checkpoint: startupPhaseGuard, Label: "Synchronize the Roblox launch guard"},
	{Checkpoint: startupPhaseListener, Label: "Start the dashboard listener"},
	{Checkpoint: startupPhaseReady, Label: "Report the service as running"},
}

func startupPhaseLabel(checkpoint uint32) string {
	for _, phase := range startupPhases {
		if phase.Checkpoint == checkpoint {
			return phase.Label
		}
	}
	return ""
}
