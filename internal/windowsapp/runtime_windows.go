//go:build windows

package windowsapp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/auth"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/buildinfo"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/enforce"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/platform"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/server"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/store"
)

type runtimeApp struct {
	logger       *serviceLogger
	store        *store.Store
	auth         *auth.Manager
	enforcer     *enforce.Enforcer
	httpServer   *http.Server
	cancel       context.CancelFunc
	enforcerDone chan struct{}
}

func startRuntime(logger *serviceLogger, report func(uint32)) (*runtimeApp, error) {
	reportPhase := func(checkpoint uint32) {
		label := startupPhaseLabel(checkpoint)
		if label == "" {
			label = "unknown"
		}
		logger.Infof("startup phase %d: %s", checkpoint, label)
		if report != nil {
			report(checkpoint)
		}
	}

	reportPhase(startupPhaseDataDir)
	if err := ensurePrivateDataDir(); err != nil {
		logger.Errorf("failed to secure application data directory: %v", err)
		return nil, fmt.Errorf("secure application data: %w", err)
	}
	reportPhase(startupPhaseDatabase)
	st, err := store.Open(dataDir(), time.Now)
	if err != nil {
		return nil, fmt.Errorf("open configuration: %w", err)
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = st.Close()
		}
	}()
	reportPhase(startupPhaseAuth)
	authManager, err := auth.Open(dataDir(), st, time.Now)
	if err != nil {
		return nil, fmt.Errorf("open authentication: %w", err)
	}
	if err := authManager.Prepare(); err != nil {
		return nil, fmt.Errorf("prepare setup state: %w", err)
	}

	if notice := st.LoadNotice(); notice != "" {
		logger.Warningf("%s", notice)
		_ = st.AddEvent(model.AuditEvent{
			Type:    model.EventSystem,
			Message: notice,
			Result:  "warning",
		})
	}
	reportPhase(startupPhaseGuard)
	launchGuard, err := platform.NewRobloxGuard()
	if err != nil {
		return nil, fmt.Errorf("initialize Roblox launch guard: %w", err)
	}
	if err := launchGuard.SetBlocked(!st.PlayAllowed()); err != nil {
		logger.Errorf("failed to synchronize Roblox launch guard: %v", err)
		_ = st.AddEvent(model.AuditEvent{
			Type:    model.EventSystem,
			Message: "Failed to synchronize the Roblox pre-launch guard: " + err.Error(),
			Result:  "guard-failed",
		})
		return nil, fmt.Errorf("synchronize Roblox launch guard: %w", err)
	}
	logger.Infof("Roblox launch guard synchronized; blocked=%t", !st.PlayAllowed())

	source, err := platform.NewSource()
	if err != nil {
		return nil, err
	}
	engine := enforce.New(source, st, st, enforce.Options{
		PollInterval: enforce.DefaultPollInterval,
		CloseGrace:   enforce.DefaultCloseGrace,
	})

	ctx, cancel := context.WithCancel(context.Background())
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		engine.Run(ctx)
	}()

	reportPhase(startupPhaseListener)
	httpAPI := server.New(server.Options{
		Address:     fmt.Sprintf("0.0.0.0:%d", server.Port),
		Version:     buildinfo.Version,
		LaunchGuard: launchGuard,
	}, st, authManager, engine)
	listener, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", server.Port))
	if err != nil {
		cancel()
		<-engineDone
		return nil, fmt.Errorf("listen on TCP %d: %w", server.Port, err)
	}
	httpServer := &http.Server{
		Handler:           httpAPI.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	app := &runtimeApp{
		logger:       logger,
		store:        st,
		auth:         authManager,
		enforcer:     engine,
		httpServer:   httpServer,
		cancel:       cancel,
		enforcerDone: engineDone,
	}
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Errorf("dashboard server stopped unexpectedly: %v", err)
			_ = st.AddEvent(model.AuditEvent{
				Type:    model.EventSystem,
				Message: "Dashboard server stopped unexpectedly: " + err.Error(),
				Result:  "failed",
			})
		}
	}()

	reportPhase(startupPhaseReady)
	if err := st.EnsureBackup(); err != nil {
		logger.Warningf("could not refresh database backup: %v", err)
	}
	logger.Infof("service started; version=%s; port=%d; setupRequired=%t; database=%s", buildinfo.Version, server.Port, authManager.SetupRequired(), st.DatabasePath())
	_ = st.AddEvent(model.AuditEvent{
		Type:    model.EventSystem,
		Message: "Playground Control service started.",
		Result:  "started",
	})
	succeeded = true
	return app, nil
}

func (a *runtimeApp) Shutdown() {
	if a == nil {
		return
	}
	if a.cancel != nil {
		a.cancel()
	}
	select {
	case <-a.enforcerDone:
	case <-time.After(5 * time.Second):
		a.logger.Warningf("enforcement loop did not stop within the timeout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if a.httpServer != nil {
		_ = a.httpServer.Shutdown(ctx)
	}
	if a.store != nil {
		_ = a.store.AddEvent(model.AuditEvent{
			Type:    model.EventSystem,
			Message: "Playground Control service stopped.",
			Result:  "stopped",
		})
		_ = a.store.Close()
	}
	a.logger.Infof("service stopped")
	a.logger.Close()
}

type serviceHandler struct {
	logger *serviceLogger
}

func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	// Report StartPending before any filesystem, registry, or logger work so a
	// slow ProgramData or antivirus scan cannot consume the SCM startup window.
	changes <- svc.Status{State: svc.StartPending, CheckPoint: startupPhaseDataDir, WaitHint: serviceWaitHintMS}
	if h.logger == nil {
		h.logger = newServiceLogger()
	}

	report := func(checkpoint uint32) {
		changes <- svc.Status{State: svc.StartPending, CheckPoint: checkpoint, WaitHint: serviceWaitHintMS}
	}
	app, err := startRuntime(h.logger, report)
	if err != nil {
		h.logger.Errorf("service failed to start: %v", err)
		changes <- svc.Status{State: svc.Stopped}
		h.logger.Close()
		return true, 1
	}

	running := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	changes <- running
	for request := range requests {
		switch request.Cmd {
		case svc.Interrogate:
			changes <- running
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: 10000}
			app.Shutdown()
			changes <- svc.Status{State: svc.Stopped}
			return false, 0
		}
	}
	app.Shutdown()
	return false, 0
}
