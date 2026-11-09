//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	wmDestroy       = 0x0002
	wmCommand       = 0x0111
	wmApp           = 0x8000
	wmTray          = wmApp + 12
	wmCloseApp      = wmApp + 13
	wmContextMenu   = 0x007B
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	imageIcon       = 1
	loadFromFile    = 0x0010
	niAdd           = 0
	niDelete        = 2
	nifMessage      = 0x01
	nifIcon         = 0x02
	nifTip          = 0x04
	mfString        = 0
	tpmRightButton  = 2
	menuOpen        = 201
	menuSend        = 202
	menuQuit        = 203
	menuUninstall   = 204
)

var (
	u32                  = syscall.NewLazyDLL("user32.dll")
	procRegisterClass    = u32.NewProc("RegisterClassExW")
	procCreateWindow     = u32.NewProc("CreateWindowExW")
	procDefWindow        = u32.NewProc("DefWindowProcW")
	procGetMessage       = u32.NewProc("GetMessageW")
	procTranslateMessage = u32.NewProc("TranslateMessage")
	procDispatchMessage  = u32.NewProc("DispatchMessageW")
	procDestroyWindow    = u32.NewProc("DestroyWindow")
	procPostMessage      = u32.NewProc("PostMessageW")
	procPostQuit         = u32.NewProc("PostQuitMessage")
	procLoadImage        = u32.NewProc("LoadImageW")
	procLoadIcon         = u32.NewProc("LoadIconW")
	procMessageBox       = u32.NewProc("MessageBoxW")
	procGetModuleHandle  = kernel32.NewProc("GetModuleHandleW")
	procCreateMutex      = kernel32.NewProc("CreateMutexW")
	procCloseHandle      = kernel32.NewProc("CloseHandle")
	shell32              = syscall.NewLazyDLL("shell32.dll")
	procShellNotify      = shell32.NewProc("Shell_NotifyIconW")
	procCreatePopup      = u32.NewProc("CreatePopupMenu")
	procAppendMenu       = u32.NewProc("AppendMenuW")
	procTrackPopup       = u32.NewProc("TrackPopupMenu")
	procDestroyMenu      = u32.NewProc("DestroyMenu")
	procGetCursorPos     = u32.NewProc("GetCursorPos")
	procSetForeground    = u32.NewProc("SetForegroundWindow")
	trayWindow           uintptr
	trayInfo             notifyIconData
)

type winPoint struct{ X, Y int32 }
type winMessage struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      winPoint
	Private uint32
}
type windowClass struct {
	Size        uint32
	Style       uint32
	WndProc     uintptr
	ClassExtra  int32
	WindowExtra int32
	Instance    uintptr
	Icon        uintptr
	Cursor      uintptr
	Background  uintptr
	MenuName    *uint16
	ClassName   *uint16
	IconSm      uintptr
}
type notifyIconData struct {
	Size            uint32
	Window          uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	Guid            [16]byte
	BalloonIcon     uintptr
}

func utf(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

func showMessage(msg string) {
	procMessageBox.Call(0, uintptr(unsafe.Pointer(utf(msg))), uintptr(unsafe.Pointer(utf("RAM Altman"))), 0x00000040)
}

func acquireSingleInstance() (bool, error) {
	name := utf("Local\\Mochi_RAM_Altman_2026")
	h, _, e := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return false, fmt.Errorf("CreateMutex: %v", e)
	}
	instanceHandle = h
	if errno, ok := e.(syscall.Errno); ok && errno == 183 {
		return false, nil
	}
	return true, nil
}
func closeInstance() {
	if instanceHandle != 0 {
		procCloseHandle.Call(instanceHandle)
		instanceHandle = 0
	}
}

func showPopup() {
	menu, _, _ := procCreatePopup.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)
	for _, m := range []struct {
		id   uintptr
		text string
	}{
		{menuOpen, "Open settings"}, {menuSend, "Send RAM report now"}, {menuUninstall, "Uninstall RAM Altman..."}, {menuQuit, "Exit (stop reporting)"},
	} {
		procAppendMenu.Call(menu, mfString, m.id, uintptr(unsafe.Pointer(utf(m.text))))
	}
	var pt winPoint
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForeground.Call(trayWindow)
	procTrackPopup.Call(menu, tpmRightButton, uintptr(pt.X), uintptr(pt.Y), 0, trayWindow, 0)
}

func removeTray() {
	if trayWindow != 0 {
		procShellNotify.Call(niDelete, uintptr(unsafe.Pointer(&trayInfo)))
	}
}

func postExit() {
	if trayWindow != 0 {
		procPostMessage.Call(trayWindow, wmCloseApp, 0, 0)
	}
}

func wndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmTray:
		switch lparam & 0xFFFF {
		case wmLButtonDblClk, wmLButtonUp:
			go openUI()
		case wmRButtonUp, wmContextMenu:
			showPopup()
		}
		return 0
	case wmCommand:
		switch wparam & 0xFFFF {
		case menuOpen:
			go openUI()
		case menuSend:
			go reportOnce()
		case menuUninstall:
			go openUI()
		case menuQuit:
			procDestroyWindow.Call(hwnd)
		}
		return 0
	case wmCloseApp:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		removeTray()
		procPostQuit.Call(0)
		return 0
	}
	res, _, _ := procDefWindow.Call(hwnd, msg, wparam, lparam)
	return res
}

func runTray() {
	inst, _, _ := procGetModuleHandle.Call(0)
	className := utf("RAMAltmanTrayClassV1")
	wc := windowClass{Size: uint32(unsafe.Sizeof(windowClass{})), WndProc: syscall.NewCallback(wndProc), Instance: inst, ClassName: className}
	atom, _, e := procRegisterClass.Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		showMessage("Could not register tray window: " + e.Error())
		return
	}
	hwnd, _, e := procCreateWindow.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(utf(appName))), 0, 0, 0, 0, 0, 0, 0, inst, 0)
	if hwnd == 0 {
		showMessage("Could not create tray window: " + e.Error())
		return
	}
	trayWindow = hwnd

	iconName := filepath.Join(installedDir, "ramaltman.ico")
	icon, _, _ := procLoadImage.Call(0, uintptr(unsafe.Pointer(utf(iconName))), imageIcon, 32, 32, loadFromFile)
	if icon == 0 {
		icon, _, _ = procLoadIcon.Call(0, 32512)
	}
	trayInfo = notifyIconData{Size: uint32(unsafe.Sizeof(notifyIconData{})), Window: hwnd, ID: 1, Flags: nifMessage | nifIcon | nifTip, CallbackMessage: wmTray, Icon: icon}
	copy(trayInfo.Tip[:], syscall.StringToUTF16("RAM Altman — your unused RAM, proudly wasted"))
	ok, _, _ := procShellNotify.Call(niAdd, uintptr(unsafe.Pointer(&trayInfo)))
	if ok == 0 {
		showMessage("Tray icon could not be added. Settings will remain accessible until this process exits.")
	}
	var m winMessage
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) == -1 || r == 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}
