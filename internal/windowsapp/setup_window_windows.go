//go:build windows

package windowsapp

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wsChild          = 0x40000000
	wsVisible        = 0x10000000
	wsVScroll        = 0x00200000
	wsCaption        = 0x00C00000
	wsSysMenu        = 0x00080000
	wsMinimizeBox    = 0x00020000
	wsExClientEdge   = 0x00000200
	esMultiline      = 0x0004
	esAutoVScroll    = 0x0040
	esReadOnly       = 0x0800
	swHide           = 0
	swShow           = 5
	wmCreate         = 0x0001
	wmDestroy        = 0x0002
	wmClose          = 0x0010
	wmSetFont        = 0x0030
	wmCommand        = 0x0111
	wmTimer          = 0x0113
	bnClicked        = 0
	defaultGUIFont   = 17
	iccProgressClass = 0x00000020
	pbmSetRange32    = 0x0406
	pbmSetPos        = 0x0402
	emSetLimitText   = 0x00C5
	emSetSel         = 0x00B1
	emScrollCaret    = 0x00B7
	idcArrow         = 32512
	idCopyDetails    = 1001
	idRepair         = 1002
	idFullReset      = 1003
	idExit           = 1004
	idFinish         = 1005
)

var (
	user32Setup = windows.NewLazySystemDLL("user32.dll")
	comctlSetup = windows.NewLazySystemDLL("comctl32.dll")

	procRegisterClassExW    = user32Setup.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32Setup.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32Setup.NewProc("DefWindowProcW")
	procShowWindow          = user32Setup.NewProc("ShowWindow")
	procUpdateWindow        = user32Setup.NewProc("UpdateWindow")
	procGetMessageW         = user32Setup.NewProc("GetMessageW")
	procTranslateMessage    = user32Setup.NewProc("TranslateMessage")
	procDispatchMessageW    = user32Setup.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32Setup.NewProc("PostQuitMessage")
	procSendMessageW        = user32Setup.NewProc("SendMessageW")
	procSetWindowTextW      = user32Setup.NewProc("SetWindowTextW")
	procEnableWindow        = user32Setup.NewProc("EnableWindow")
	procDestroyWindow       = user32Setup.NewProc("DestroyWindow")
	procSetTimer            = user32Setup.NewProc("SetTimer")
	procKillTimer           = user32Setup.NewProc("KillTimer")
	procLoadCursorW         = user32Setup.NewProc("LoadCursorW")
	procGetSystemMetrics    = user32Setup.NewProc("GetSystemMetrics")
	procInitCommonControls  = comctlSetup.NewProc("InitCommonControlsEx")
	procGetStockObject      = windows.NewLazySystemDLL("gdi32.dll").NewProc("GetStockObject")
	setupWndProcCallback    = windows.NewCallback(setupWindowProc)
	setupWindowClassOnce    sync.Once
	setupWindowClassErr     error
	setupWindowMap          sync.Map
	setupWindowClassName    = "PlaygroundControlSetup"
	setupWindowClassNamePtr = func() *uint16 {
		ptr, _ := windows.UTF16PtrFromString(setupWindowClassName)
		return ptr
	}()
)

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

type msgW struct {
	Hwnd    windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

type initCommonControlsExW struct {
	Size uint32
	ICC  uint32
}

type setupWindow struct {
	hwnd       windows.HWND
	model      *progressModel
	workDone   chan struct{}
	finishCh   chan struct{}
	finishOnce sync.Once
	action     chan recoveryAction

	lastDetails string
	lastStatus  string

	details windows.HWND
	status  windows.HWND
	bar     windows.HWND
	copy    windows.HWND
	repair  windows.HWND
	reset   windows.HWND
	exit    windows.HWND
	finish  windows.HWND
}

func newSetupWindow() (*setupWindow, error) {
	setupWindowClassOnce.Do(func() {
		var instance windows.Handle
		if err := windows.GetModuleHandleEx(0, nil, &instance); err != nil {
			setupWindowClassErr = err
			return
		}
		var controls initCommonControlsExW
		controls.Size = uint32(unsafe.Sizeof(controls))
		controls.ICC = iccProgressClass
		_, _, _ = procInitCommonControls.Call(uintptr(unsafe.Pointer(&controls)))
		cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
		class := wndClassExW{
			CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
			LpfnWndProc:   setupWndProcCallback,
			HInstance:     instance,
			HCursor:       windows.Handle(cursor),
			HbrBackground: 6,
			LpszClassName: setupWindowClassNamePtr,
		}
		atom, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&class)))
		if atom == 0 {
			setupWindowClassErr = err
			return
		}
	})
	if setupWindowClassErr != nil {
		return nil, setupWindowClassErr
	}

	title, _ := windows.UTF16PtrFromString("Playground Control Setup")
	screenW, _, _ := procGetSystemMetrics.Call(0)
	screenH, _, _ := procGetSystemMetrics.Call(1)
	const width, height = 700, 520
	x := (int(screenW) - width) / 2
	y := (int(screenH) - height) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(setupWindowClassNamePtr)),
		uintptr(unsafe.Pointer(title)),
		uintptr(wsCaption|wsSysMenu|wsMinimizeBox),
		uintptr(x),
		uintptr(y),
		width,
		height,
		0,
		0,
		0,
		0,
	)
	if hwnd == 0 {
		return nil, fmt.Errorf("create setup window: %w", err)
	}
	window := &setupWindow{
		hwnd:     windows.HWND(hwnd),
		model:    newProgressModel(setupVersion()),
		workDone: make(chan struct{}),
		finishCh: make(chan struct{}),
		action:   make(chan recoveryAction, 1),
	}
	setupWindowMap.Store(window.hwnd, window)
	window.createControls()
	// WM_CREATE is not used because the window must be registered in the map
	// before child controls can dispatch messages through the window procedure.
	window.refresh()
	procShowWindow.Call(uintptr(window.hwnd), swShow)
	procUpdateWindow.Call(uintptr(window.hwnd))
	procSetTimer.Call(uintptr(window.hwnd), 1, 100, 0)
	return window, nil
}

func (w *setupWindow) createControls() {
	font, _, _ := procGetStockObject.Call(defaultGUIFont)
	header := w.createControl("STATIC", "Playground Control Setup", 18, 14, 650, 26, 0, 0)
	w.createControl("STATIC", "Version "+setupVersion()+"  |  The setup window shows each installation step.", 18, 40, 650, 20, 0, 0)
	w.details = w.createControl("EDIT", "", 18, 68, 650, 310, wsVScroll|esMultiline|esAutoVScroll|esReadOnly, wsExClientEdge)
	w.status = w.createControl("STATIC", "Preparing setup...", 18, 386, 650, 22, 0, 0)
	w.bar = w.createControl("msctls_progress32", "", 18, 412, 650, 18, 0, 0)
	w.copy = w.createControl("BUTTON", "Copy details", 526, 442, 140, 30, 0, 0)
	w.repair = w.createControl("BUTTON", "Repair / keep data", 18, 442, 160, 30, 0, 0)
	w.reset = w.createControl("BUTTON", "Full reset", 186, 442, 120, 30, 0, 0)
	w.exit = w.createControl("BUTTON", "Exit", 314, 442, 90, 30, 0, 0)
	w.finish = w.createControl("BUTTON", "Finish", 314, 442, 90, 30, 0, 0)
	for _, control := range []windows.HWND{header, w.details, w.status, w.bar, w.copy, w.repair, w.reset, w.exit, w.finish} {
		procSendMessageW.Call(uintptr(control), wmSetFont, font, 1)
	}
	procSendMessageW.Call(uintptr(w.details), emSetLimitText, 1<<20, 0)
	procShowWindow.Call(uintptr(w.repair), swHide)
	procShowWindow.Call(uintptr(w.reset), swHide)
	procShowWindow.Call(uintptr(w.exit), swHide)
	procShowWindow.Call(uintptr(w.finish), swHide)
}

func moduleHandle() (windows.Handle, error) {
	var instance windows.Handle
	err := windows.GetModuleHandleEx(0, nil, &instance)
	return instance, err
}

func (w *setupWindow) createControl(class, text string, x, y, width, height int, style, exStyle uintptr) windows.HWND {
	classPtr, _ := windows.UTF16PtrFromString(class)
	textPtr, _ := windows.UTF16PtrFromString(text)
	instance, _ := moduleHandle()
	hwnd, _, _ := procCreateWindowExW.Call(
		exStyle,
		uintptr(unsafe.Pointer(classPtr)),
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(wsChild|wsVisible)|style,
		uintptr(x),
		uintptr(y),
		uintptr(width),
		uintptr(height),
		uintptr(w.hwnd),
		0,
		uintptr(instance),
		0,
	)
	return windows.HWND(hwnd)
}

func setupWindowProc(hwnd windows.HWND, msg uint32, wparam, lparam uintptr) uintptr {
	value, ok := setupWindowMap.Load(hwnd)
	if !ok {
		result, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
		return result
	}
	window := value.(*setupWindow)
	switch msg {
	case wmCommand:
		window.handleCommand(int(wparam&0xFFFF), int(wparam>>16))
		return 0
	case wmTimer:
		window.refresh()
		return 0
	case wmClose:
		if !window.canClose() {
			window.model.Info("Installation is still running. Please wait for the current step to finish.")
			return 0
		}
		window.sendAction(recoveryExit)
		window.signalFinish()
		procDestroyWindow.Call(uintptr(hwnd))
		return 0
	case wmDestroy:
		procKillTimer.Call(uintptr(hwnd), 1)
		setupWindowMap.Delete(hwnd)
		procPostQuitMessage.Call(0)
		return 0
	case wmCreate:
		return 0
	default:
		result, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
		return result
	}
}

func (w *setupWindow) handleCommand(id, code int) {
	if code != bnClicked {
		return
	}
	switch id {
	case idCopyDetails:
		w.copyTranscript()
	case idRepair:
		w.sendAction(recoveryRepair)
	case idFullReset:
		w.sendAction(recoveryReset)
	case idExit:
		w.sendAction(recoveryExit)
		w.signalFinish()
		procDestroyWindow.Call(uintptr(w.hwnd))
	case idFinish:
		w.signalFinish()
		procDestroyWindow.Call(uintptr(w.hwnd))
	}
}

func (w *setupWindow) sendAction(action recoveryAction) {
	select {
	case w.action <- action:
	default:
	}
}

func (w *setupWindow) canClose() bool {
	snapshot := w.model.Snapshot()
	return snapshot.Terminal || snapshot.AwaitingChoice
}

func (w *setupWindow) Run(work func()) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	go func() {
		defer close(w.workDone)
		defer func() {
			if recovered := recover(); recovered != nil {
				w.model.Failure(fmt.Sprintf("Setup stopped unexpectedly: %v", recovered))
				w.model.MarkFailure()
			}
		}()
		work()
	}()
	var message msgW
	for {
		result, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(result) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&message)))
	}
	select {
	case <-w.workDone:
	case <-time.After(2 * time.Second):
	}
	return nil
}

func (w *setupWindow) StartStep(text string)    { w.model.StartStep(text) }
func (w *setupWindow) CompleteStep(text string) { w.model.CompleteStep(text) }
func (w *setupWindow) Info(text string)         { w.model.Info(text) }
func (w *setupWindow) Failure(text string)      { w.model.Failure(text) }
func (w *setupWindow) ServiceLine(text string)  { w.model.ServiceLine(text) }
func (w *setupWindow) SetStatus(text string)    { w.model.SetStatus(text) }
func (w *setupWindow) SetProgress(done, total int) {
	w.model.SetProgress(done, total)
}
func (w *setupWindow) MarkSuccess()   { w.model.MarkSuccess() }
func (w *setupWindow) MarkFailure()   { w.model.MarkFailure() }
func (w *setupWindow) ClearTerminal() { w.model.ClearTerminal() }

func (w *setupWindow) AskRecovery(reason string) recoveryAction {
	w.model.Info("Recovery required: " + strings.TrimSpace(reason))
	w.model.MarkAwaitingChoice()
	action := <-w.action
	w.model.ClearTerminal()
	return action
}

func (w *setupWindow) ConfirmFullReset() bool {
	text := "Full reset permanently deletes:\n\n" +
		"  - " + dataDir() + "\n" +
		"  - The administrator PIN, game list, play permission, and audit history\n" +
		"  - Database backups and logs\n" +
		"  - The service, firewall rule, uninstall entry, Event Log source, and Roblox guard entries\n" +
		"  - Installed program files\n\n" +
		"This cannot be undone. Continue with the full reset?"
	return messageBoxYesNo(w.hwnd, "Full reset Playground Control?", text, true)
}

func (w *setupWindow) WaitForFinish() {
	<-w.finishCh
}

func (w *setupWindow) signalFinish() {
	w.finishOnce.Do(func() {
		close(w.finishCh)
	})
}

func (w *setupWindow) refresh() {
	snapshot := w.model.Snapshot()
	text := strings.Join(snapshot.Lines, "\r\n")
	if text != w.lastDetails {
		w.lastDetails = text
		setControlText(w.details, text)
		if utf16, err := windows.UTF16FromString(text); err == nil && len(utf16) > 0 {
			end := uintptr(len(utf16) - 1)
			procSendMessageW.Call(uintptr(w.details), emSetSel, end, end)
			procSendMessageW.Call(uintptr(w.details), emScrollCaret, 0, 0)
		}
	}
	if snapshot.Status != w.lastStatus {
		w.lastStatus = snapshot.Status
		setControlText(w.status, snapshot.Status)
	}
	if snapshot.Total > 0 {
		procSendMessageW.Call(uintptr(w.bar), pbmSetRange32, 0, uintptr(snapshot.Total))
		procSendMessageW.Call(uintptr(w.bar), pbmSetPos, uintptr(snapshot.Done), 0)
	}
	switch {
	case snapshot.AwaitingChoice:
		showControl(w.repair, true)
		showControl(w.reset, true)
		showControl(w.exit, true)
		showControl(w.finish, false)
	case snapshot.Terminal && snapshot.Success:
		showControl(w.repair, false)
		showControl(w.reset, false)
		showControl(w.exit, false)
		showControl(w.finish, true)
	case snapshot.Terminal:
		showControl(w.repair, false)
		showControl(w.reset, false)
		showControl(w.exit, true)
		showControl(w.finish, false)
	default:
		showControl(w.repair, false)
		showControl(w.reset, false)
		showControl(w.exit, false)
		showControl(w.finish, false)
	}
}

func setControlText(hwnd windows.HWND, text string) {
	ptr, err := windows.UTF16PtrFromString(strings.TrimRight(text, "\r\n"))
	if err != nil {
		return
	}
	procSetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(ptr)))
}

func showControl(hwnd windows.HWND, visible bool) {
	command := uintptr(swHide)
	if visible {
		command = swShow
	}
	procShowWindow.Call(uintptr(hwnd), command)
	procEnableWindow.Call(uintptr(hwnd), boolUintptr(visible))
}

func boolUintptr(value bool) uintptr {
	if value {
		return 1
	}
	return 0
}

func (w *setupWindow) copyTranscript() {
	text := strings.Join(w.model.Snapshot().Lines, "\r\n")
	if strings.TrimSpace(text) == "" {
		w.model.Info("There is no transcript to copy yet.")
		return
	}
	cmd := exec.Command(system32Executable("clip.exe"))
	cmd.Stdin = strings.NewReader(text)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Run(); err != nil {
		w.model.Failure("Could not copy details to the Windows clipboard: " + err.Error())
		return
	}
	w.model.Info("The installation transcript was copied to the clipboard.")
}

func messageBoxYesNo(owner windows.HWND, title, text string, warning bool) bool {
	titlePtr, _ := windows.UTF16PtrFromString(title)
	textPtr, _ := windows.UTF16PtrFromString(text)
	flags := uint32(0x00000004) // MB_YESNO
	if warning {
		flags |= 0x00000030 // MB_ICONWARNING
		flags |= 0x00000100 // MB_DEFBUTTON2
	}
	result, _ := windows.MessageBox(owner, textPtr, titlePtr, flags)
	return result == 6
}

type messageBoxUI struct {
	model   *progressModel
	success bool
	closed  bool
}

func newMessageBoxUI() *messageBoxUI {
	return &messageBoxUI{model: newProgressModel(setupVersion())}
}

func (m *messageBoxUI) StartStep(text string)    { m.model.StartStep(text) }
func (m *messageBoxUI) CompleteStep(text string) { m.model.CompleteStep(text) }
func (m *messageBoxUI) Info(text string)         { m.model.Info(text) }
func (m *messageBoxUI) Failure(text string)      { m.model.Failure(text) }
func (m *messageBoxUI) ServiceLine(text string)  { m.model.ServiceLine(text) }
func (m *messageBoxUI) SetStatus(text string)    { m.model.SetStatus(text) }
func (m *messageBoxUI) SetProgress(done, total int) {
	m.model.SetProgress(done, total)
}
func (m *messageBoxUI) MarkSuccess()   { m.success = true; m.model.MarkSuccess() }
func (m *messageBoxUI) MarkFailure()   { m.model.MarkFailure() }
func (m *messageBoxUI) ClearTerminal() { m.model.ClearTerminal() }

func (m *messageBoxUI) AskRecovery(reason string) recoveryAction {
	m.model.Info("Recovery required: " + strings.TrimSpace(reason))
	title, _ := windows.UTF16PtrFromString("Repair Playground Control?")
	text, _ := windows.UTF16PtrFromString(strings.TrimSpace(reason) + "\n\nYes: Repair and keep PIN, games, play state, and history.\nNo: Full reset and delete all application data.\nCancel: Exit setup.")
	result, _ := windows.MessageBox(0, text, title, 0x00000003|0x00000030) // YESNOCANCEL; default Yes = Repair
	switch result {
	case 6:
		return recoveryRepair
	case 7:
		return recoveryReset
	default:
		return recoveryExit
	}
}

func (m *messageBoxUI) ConfirmFullReset() bool {
	title, _ := windows.UTF16PtrFromString("Full reset Playground Control?")
	text, _ := windows.UTF16PtrFromString("This permanently deletes " + dataDir() + " (PIN, game list, play state, audit history, database backups, logs) plus the service, firewall rule, uninstall entry, Event Log source, Roblox guard entries, and installed files.\n\nContinue?")
	result, _ := windows.MessageBox(0, text, title, 0x00000004|0x00000030|0x00000100)
	return result == 6
}

func (m *messageBoxUI) WaitForFinish() {
	if m.closed {
		return
	}
	m.closed = true
	title, _ := windows.UTF16PtrFromString("Playground Control Setup")
	text := strings.Join(m.model.Snapshot().Lines, "\r\n")
	if len(text) > 3500 {
		text = text[len(text)-3500:]
	}
	textPtr, _ := windows.UTF16PtrFromString(text)
	flags := uint32(0x00000040) // MB_ICONINFORMATION
	if !m.success {
		flags = 0x00000010 // MB_ICONERROR
	}
	_, _ = windows.MessageBox(0, textPtr, title, flags)
}

func runSetupUI(work func(setupUI)) error {
	window, err := newSetupWindow()
	if err != nil {
		fallback := newMessageBoxUI()
		defer func() {
			if recovered := recover(); recovered != nil {
				fallback.Failure(fmt.Sprintf("Setup stopped unexpectedly: %v", recovered))
				fallback.MarkFailure()
				fallback.WaitForFinish()
			}
		}()
		work(fallback)
		return nil
	}
	return window.Run(func() {
		work(window)
	})
}

var _ setupUI = (*setupWindow)(nil)
var _ setupUI = (*messageBoxUI)(nil)
