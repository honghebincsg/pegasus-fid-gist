package main

import (
	"fmt"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// 浅色置顶的原生告警窗口（Win32），多条变更合并展示，同一条变更只弹一次。

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassExW   = user32.NewProc("RegisterClassExW")
	procCreateWindowExW    = user32.NewProc("CreateWindowExW")
	procDefWindowProcW     = user32.NewProc("DefWindowProcW")
	procDestroyWindow      = user32.NewProc("DestroyWindow")
	procGetMessageW        = user32.NewProc("GetMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procDispatchMessageW   = user32.NewProc("DispatchMessageW")
	procPostThreadMessageW = user32.NewProc("PostThreadMessageW")
	procShowWindow         = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procFindWindowW        = user32.NewProc("FindWindowW")
	procSetWindowPos       = user32.NewProc("SetWindowPos")
	procAdjustWindowRectEx = user32.NewProc("AdjustWindowRectEx")
	procSystemParametersInfoW = user32.NewProc("SystemParametersInfoW")
	procMessageBeep         = user32.NewProc("MessageBeep")
	procSendMessageW        = user32.NewProc("SendMessageW")
	procGetDlgCtrlID        = user32.NewProc("GetDlgCtrlID")
	procLoadCursorW         = user32.NewProc("LoadCursorW")
	procGetCurrentThreadID  = kernel32.NewProc("GetCurrentThreadId")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")

	procSetBkColor        = gdi32.NewProc("SetBkColor")
	procSetTextColor      = gdi32.NewProc("SetTextColor")
	procCreateSolidBrush  = gdi32.NewProc("CreateSolidBrush")
	procCreateFontW       = gdi32.NewProc("CreateFontW")
)

const (
	wmCommand        = 0x0111
	wmClose          = 0x0010
	wmSetFont        = 0x0030
	wmCtlColorStatic = 0x0138
	wmAppRefreshAlerts = 0x8001

	wsChild      = 0x40000000
	wsVisible    = 0x10000000
	wsPopup      = 0x80000000
	wsCaption    = 0x00C00000
	wsSysMenu    = 0x00080000
	wsTabStop    = 0x00010000
	exTopmost    = 0x00000008
	swShow       = 5
	swRestore    = 9
	hwndTopmost  = ^uintptr(0) // HWND_TOPMOST = -1
	swpNoMove    = 0x0002
	swpNoSize    = 0x0001
	swpShowWindow = 0x0040
	spiGetWorkArea = 0x0030

	ssLeft        = 0x00000000
	ssRight       = 0x00000002
	ssNoprefix    = 0x00000080
	ssEtchedHorz  = 0x00000010
	bsPushButton  = 0x00000000
	bsDefPushButton = 0x00000001

	idAlertOK      = 1001
	idAlertHistory = 1002

	alertBgColor = 0x00FAF8F7 // #F7F8FA (COLORREF: BBGGRR)
)

type wndClassExT struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSm     uintptr
}

type pointT struct{ x, y int32 }

type winMsgT struct {
	hwnd     uintptr
	message  uint32
	_        uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       pointT
	lPrivate uint32
}

type rectT struct{ left, top, right, bottom int32 }

var (
	alertMu       sync.Mutex
	alertQueue    []gateAlert
	alertShown    []gateAlert
	alertHWND     uintptr
	alertThreadID uint32
	alertClassOK  bool
	alertBrush    uintptr
	alertColors   = map[int]uint32{}
	alertFonts    = map[int]uintptr{}
	alertProc     = syscall.NewCallback(gateAlertWndProc)
	alertClass   = &wndClassExT{}

	alertClassUTF16, _ = syscall.UTF16PtrFromString("AncGateAlertWnd")
	alertTitleUTF16, _ = syscall.UTF16PtrFromString("停机位变更告警 — ANC 值班")
	alertMainTitleUTF16, _ = syscall.UTF16PtrFromString("CSN ANC · 航班与 ATIS")
	staticClassUTF16, _ = syscall.UTF16PtrFromString("STATIC")
	buttonClassUTF16, _ = syscall.UTF16PtrFromString("BUTTON")
	yaheiUTF16, _ = syscall.UTF16PtrFromString("Microsoft YaHei UI")
)

func startAlertUI() {
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		tid, _, _ := procGetCurrentThreadID.Call()
		atomic.StoreUint32(&alertThreadID, uint32(tid))
		if !registerAlertClass() {
			log.Printf("gate alert window class registration failed")
			return
		}
		log.Printf("gate alert ui ready (thread %d)", tid)
		alertMu.Lock()
		hasQueued := len(alertQueue) > 0
		alertMu.Unlock()
		if hasQueued {
			rebuildAlertWindow()
		}
		var msg winMsgT
		for {
			ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if int32(ret) <= 0 {
				return
			}
			if msg.hwnd == 0 {
				if msg.message == wmAppRefreshAlerts {
					rebuildAlertWindow()
				}
				continue
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
		}
	}()
}

func registerAlertClass() bool {
	if alertClassOK {
		return true
	}
	instance, _, _ := procGetModuleHandleW.Call(0)
	cursor, _, _ := procLoadCursorW.Call(0, 32512) // IDC_ARROW
	brush, _, _ := procCreateSolidBrush.Call(uintptr(alertBgColor))
	alertBrush = brush
	alertClass.size = uint32(unsafe.Sizeof(*alertClass))
	alertClass.wndProc = alertProc
	alertClass.instance = instance
	alertClass.cursor = cursor
	alertClass.background = brush
	alertClass.className = alertClassUTF16
	ret, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(alertClass)))
	alertClassOK = ret != 0
	if alertClassOK {
		alertFonts[1] = createUIFont(-21, 600, 0) // 标题
		alertFonts[2] = createUIFont(-15, 400, 0) // 小字
		alertFonts[3] = createUIFont(-19, 600, 0) // 航班号
		alertFonts[4] = createUIFont(-18, 400, 1) // 原机位（删除线）
		alertFonts[5] = createUIFont(-20, 700, 0) // 新机位
	}
	return alertClassOK
}

func createUIFont(height int32, weight int32, strikeout uint8) uintptr {
	font, _, _ := procCreateFontW.Call(
		uintptr(height), 0, 0, 0, uintptr(weight), 0, 0, uintptr(strikeout),
		1, 0, 0, 5, 0, uintptr(unsafe.Pointer(yaheiUTF16)))
	return font
}

func wparamLo(value uintptr) int { return int(value & 0xffff) }

func gateAlertWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmCtlColorStatic:
		procSetBkColor.Call(wparam, uintptr(alertBgColor))
		if id, _, _ := procGetDlgCtrlID.Call(lparam); id != 0 {
			if color, ok := alertColors[int(id)]; ok {
				procSetTextColor.Call(wparam, uintptr(color))
			}
		}
		return alertBrush
	case wmCommand:
		switch wparamLo(wparam) {
		case idAlertOK:
			acknowledgeShownAlerts()
			destroyAlertWindow()
		case idAlertHistory:
			openHistoryFromAlert()
			destroyAlertWindow()
		}
		return 0
	case wmClose:
		alertMu.Lock()
		alertShown = nil
		alertHWND = 0
		alertMu.Unlock()
		procDestroyWindow.Call(hwnd)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return ret
}

// enqueueGateAlerts 由 HTTP 处理线程调用，非阻塞。
func enqueueGateAlerts(alerts []gateAlert) {
	if len(alerts) == 0 {
		return
	}
	alertMu.Lock()
	alertQueue = append(alertQueue, alerts...)
	alertMu.Unlock()
	if tid := atomic.LoadUint32(&alertThreadID); tid != 0 {
		procPostThreadMessageW.Call(uintptr(tid), wmAppRefreshAlerts, 0, 0)
	}
}

func acknowledgeShownAlerts() {
	alertMu.Lock()
	ids := make([]int64, 0, len(alertShown))
	for _, alert := range alertShown {
		if alert.ID > 0 {
			ids = append(ids, alert.ID)
		}
	}
	alertShown = nil
	alertMu.Unlock()
	if storeDB == nil || len(ids) == 0 {
		return
	}
	for _, id := range ids {
		if _, err := storeDB.Exec(`UPDATE gate_alerts SET acked_at = ? WHERE id = ?`, utcNow(), id); err != nil {
			log.Printf("ack gate alert %d: %v", id, err)
		}
	}
}

func openHistoryFromAlert() {
	alertMu.Lock()
	flight := ""
	if len(alertShown) > 0 {
		flight = alertShown[0].Flight
	}
	alertMu.Unlock()
	setPendingGateHistory(flight)
	if hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(alertMainTitleUTF16))); hwnd != 0 {
		procShowWindow.Call(hwnd, swRestore)
		procSetForegroundWindow.Call(hwnd)
	}
}

func destroyAlertWindow() {
	alertMu.Lock()
	hwnd := alertHWND
	alertHWND = 0
	alertShown = nil
	alertMu.Unlock()
	if hwnd != 0 {
		procDestroyWindow.Call(hwnd)
	}
}

func buildAlertRow(alerts []gateAlert, index int, top int32, parent uintptr, instance uintptr) {
	alert := alerts[index]
	base := 100 + index*10
	addControl(parent, instance, "STATIC", alert.Flight,
		wsChild|wsVisible|ssLeft|ssNoprefix, 24, top+8, 120, 26, base, alertFonts[3], 0x00271811)
	sub := alert.Registration
	if alert.ACType != "" {
		sub += " · " + alert.ACType
	}
	addControl(parent, instance, "STATIC", sub,
		wsChild|wsVisible|ssLeft|ssNoprefix, 150, top+12, 190, 22, base+1, alertFonts[2], 0x00806B6B)
	addControl(parent, instance, "STATIC", alert.OldGate,
		wsChild|wsVisible|ssRight|ssNoprefix, 352, top+10, 70, 24, base+2, alertFonts[4], 0x009CA3AF)
	addControl(parent, instance, "STATIC", "→",
		wsChild|wsVisible|ssLeft|ssNoprefix, 430, top+10, 24, 24, base+3, alertFonts[2], 0x00806B6B)
	addControl(parent, instance, "STATIC", alert.NewGate,
		wsChild|wsVisible|ssLeft|ssNoprefix, 458, top+8, 70, 26, base+4, alertFonts[5], 0x002626DC)
	addControl(parent, instance, "STATIC", "",
		wsChild|wsVisible|ssEtchedHorz, 20, top+52, 518, 2, base+5, 0, 0)
}

func addControl(parent, instance uintptr, class string, text string,
	style uintptr, x, y, w, h int32, id int, font uintptr, color uint32) uintptr {
	classPtr, _ := syscall.UTF16PtrFromString(class)
	textPtr, _ := syscall.UTF16PtrFromString(text)
	ctrl, _, _ := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(classPtr)),
		uintptr(unsafe.Pointer(textPtr)), style,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent, uintptr(id), instance, 0)
	if ctrl != 0 && font != 0 {
		procSendMessageW.Call(ctrl, wmSetFont, font, 1)
	}
	if color != 0 {
		alertColors[id] = color
	}
	return ctrl
}

func rebuildAlertWindow() {
	alertMu.Lock()
	if len(alertQueue) > 0 {
		alertShown = append(alertShown, alertQueue...)
		alertQueue = nil
	}
	alerts := append([]gateAlert(nil), alertShown...)
	old := alertHWND
	alertHWND = 0
	alertMu.Unlock()

	if old != 0 {
		procDestroyWindow.Call(old)
	}
	if len(alerts) == 0 {
		return
	}

	const maxRows = 8
	extra := 0
	if len(alerts) > maxRows {
		extra = len(alerts) - maxRows
		alerts = alerts[:maxRows]
	}
	rowH := int32(58)
	footer := int32(74)
	height := int32(64) + int32(len(alerts))*rowH + footer
	if extra > 0 {
		height += 24
	}
	width := int32(560)

	style := uintptr(wsPopup | wsCaption | wsSysMenu | wsVisible)
	frame := rectT{right: width, bottom: height}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&frame)), style, 0, exTopmost)
	outerW := frame.right - frame.left
	outerH := frame.bottom - frame.top

	var work rectT
	procSystemParametersInfoW.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&work)), 0)
	x := work.left + (work.right-work.left-outerW)/2
	y := work.top + (work.bottom-work.top-outerH)/2

	instance, _, _ := procGetModuleHandleW.Call(0)
	hwnd, _, _ := procCreateWindowExW.Call(
		exTopmost, uintptr(unsafe.Pointer(alertClassUTF16)), uintptr(unsafe.Pointer(alertTitleUTF16)),
		style,
		uintptr(x), uintptr(y), uintptr(outerW), uintptr(outerH),
		0, 0, instance, 0)
	if hwnd == 0 {
		log.Printf("create gate alert window failed")
		return
	}
	log.Printf("gate alert window %d with %d alerts", hwnd, len(alerts))
	alertMu.Lock()
	alertHWND = hwnd
	alertMu.Unlock()

	alertColors = map[int]uint32{}
	addControl(hwnd, instance, "STATIC", fmt.Sprintf("停机位变更 · %d 条", len(alerts)+extra),
		wsChild|wsVisible|ssLeft|ssNoprefix, 24, 16, 320, 30, 1, alertFonts[1], 0x00271811)
	stamp := alerts[0].ObservedAt
	if parsed, err := time.Parse("2006-01-02 15:04:05Z", stamp); err == nil {
		stamp = parsed.Local().Format("2006-01-02 15:04:05")
	}
	addControl(hwnd, instance, "STATIC", stamp,
		wsChild|wsVisible|ssRight|ssNoprefix, 360, 22, 176, 22, 2, alertFonts[2], 0x00806B6B)
	addControl(hwnd, instance, "STATIC", "",
		wsChild|wsVisible|ssEtchedHorz, 20, 50, 518, 2, 3, 0, 0)

	top := int32(60)
	for index := range alerts {
		buildAlertRow(alerts, index, top, hwnd, instance)
		top += rowH
	}
	if extra > 0 {
		addControl(hwnd, instance, "STATIC", fmt.Sprintf("另有 %d 条变更未展开，点“查看历史”查看全部", extra),
			wsChild|wsVisible|ssLeft|ssNoprefix, 24, top, 500, 22, 20, alertFonts[2], 0x00806B6B)
		top += 24
	}

	buttonY := top + 12
	addControl(hwnd, instance, "BUTTON", "查看历史",
		wsChild|wsVisible|wsTabStop|bsPushButton, 300, buttonY, 110, 32, idAlertHistory, alertFonts[2], 0)
	addControl(hwnd, instance, "BUTTON", "知道了",
		wsChild|wsVisible|wsTabStop|bsDefPushButton, 422, buttonY, 110, 32, idAlertOK, alertFonts[2], 0)

	procShowWindow.Call(hwnd, swShow)
	procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpShowWindow)
	procSetForegroundWindow.Call(hwnd)
	procMessageBeep.Call(0x30)
}
