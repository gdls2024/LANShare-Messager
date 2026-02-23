//go:build windows

package main

import (
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32/COM APIs for custom drag-drop handling.
// We bypass WebView2's built-in IDropTarget (which crashes on large files)
// and register our own lightweight IDropTarget on Chrome_WidgetWin_0.
// Our handler only reads CF_HDROP (file paths as strings) — no file content loading.
var (
	modOle32  = windows.NewLazySystemDLL("ole32.dll")
	modShell32 = windows.NewLazySystemDLL("shell32.dll")

	procOleInitialize    = modOle32.NewProc("OleInitialize")
	procRevokeDragDrop   = modOle32.NewProc("RevokeDragDrop")
	procRegisterDragDrop = modOle32.NewProc("RegisterDragDrop")
	procReleaseStgMedium = modOle32.NewProc("ReleaseStgMedium")
	procDragQueryFileW   = modShell32.NewProc("DragQueryFileW")

	// user32 procs already declared in trayicon_windows.go (pFindWindowW, etc.)
	pEnumChildWindows = user32dll.NewProc("EnumChildWindows")
	pGetClassNameW    = user32dll.NewProc("GetClassNameW")
)

// COM constants
const (
	cfHDROP        = 15
	dropEffectNone = 0
	dropEffectCopy = 1
	tymedHGlobal   = 1
	dvaspectContent = 1
	comSOK         = 0
	comENoInterface = 0x80004002
)

// COM GUIDs
var (
	iidIUnknown    = syscall.GUID{Data1: 0x00000000, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidIDropTarget = syscall.GUID{Data1: 0x00000122, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
)

// FORMATETC — matches Win64 layout with padding for pointer alignment.
type formatETC struct {
	cfFormat uint16
	_pad     [6]byte
	ptd      uintptr
	dwAspect uint32
	lindex   int32
	tymed    uint32
	_pad2    [4]byte
}

// STGMEDIUM — Win64 layout.
type stgMEDIUM struct {
	tymed          uint32
	_pad           uint32
	hGlobal        uintptr
	pUnkForRelease uintptr
}

// dropTargetVtbl is the COM vtable for IDropTarget.
type dropTargetVtbl struct {
	QueryInterface uintptr
	AddRef         uintptr
	Release        uintptr
	DragEnter      uintptr
	DragOver       uintptr
	DragLeave      uintptr
	Drop           uintptr
}

// goDropTarget implements IDropTarget. The first field (lpVtbl) must be the
// vtable pointer for COM interop.
type goDropTarget struct {
	lpVtbl   *dropTargetVtbl
	refCount int32
	onDrop   func([]string) // called with file paths
	accepted bool           // set in DragEnter, used by DragOver/Drop
}

// Global references to prevent GC collection while COM objects are alive.
var (
	gDropTarget     *goDropTarget
	gDropTargetVtbl *dropTargetVtbl
)

// ── COM method implementations ─────────────────────────────────────────────

func dtQueryInterface(this, riid, ppvObject uintptr) uintptr {
	if ppvObject == 0 {
		return comENoInterface
	}
	guid := (*syscall.GUID)(unsafe.Pointer(riid))
	if *guid == iidIUnknown || *guid == iidIDropTarget {
		*(*uintptr)(unsafe.Pointer(ppvObject)) = this
		dtAddRef(this)
		return comSOK
	}
	*(*uintptr)(unsafe.Pointer(ppvObject)) = 0
	return comENoInterface
}

func dtAddRef(this uintptr) uintptr {
	dt := (*goDropTarget)(unsafe.Pointer(this))
	return uintptr(atomic.AddInt32(&dt.refCount, 1))
}

func dtRelease(this uintptr) uintptr {
	dt := (*goDropTarget)(unsafe.Pointer(this))
	return uintptr(atomic.AddInt32(&dt.refCount, -1))
}

// DragEnter: check if drag data contains files (CF_HDROP).
// On x64: this=RCX, pDataObj=RDX, grfKeyState=R8, pt=R9 (POINTL packed), pdwEffect=stack
func dtDragEnter(this, pDataObj, grfKeyState, pt, pdwEffect uintptr) uintptr {
	dt := (*goDropTarget)(unsafe.Pointer(this))
	dt.accepted = false
	if pDataObj != 0 && dataObjHasHDROP(pDataObj) {
		dt.accepted = true
		*(*uint32)(unsafe.Pointer(pdwEffect)) = dropEffectCopy
	} else {
		*(*uint32)(unsafe.Pointer(pdwEffect)) = dropEffectNone
	}
	return comSOK
}

// DragOver: continue accepting if DragEnter accepted.
func dtDragOver(this, grfKeyState, pt, pdwEffect uintptr) uintptr {
	dt := (*goDropTarget)(unsafe.Pointer(this))
	if dt.accepted {
		*(*uint32)(unsafe.Pointer(pdwEffect)) = dropEffectCopy
	} else {
		*(*uint32)(unsafe.Pointer(pdwEffect)) = dropEffectNone
	}
	return comSOK
}

func dtDragLeave(this uintptr) uintptr {
	return comSOK
}

// Drop: extract file paths from CF_HDROP and invoke callback.
func dtDrop(this, pDataObj, grfKeyState, pt, pdwEffect uintptr) uintptr {
	Log.Info("dtDrop: called", "pDataObj", pDataObj)
	dt := (*goDropTarget)(unsafe.Pointer(this))
	*(*uint32)(unsafe.Pointer(pdwEffect)) = dropEffectNone

	if pDataObj == 0 || !dt.accepted {
		Log.Info("dtDrop: rejected", "pDataObj_zero", pDataObj == 0, "accepted", dt.accepted)
		return comSOK
	}

	paths := extractHDROPPaths(pDataObj)
	Log.Info("dtDrop: extracted paths", "count", len(paths))
	for i, p := range paths {
		Log.Info("dtDrop: path", "index", i, "path", p)
	}
	if len(paths) > 0 && dt.onDrop != nil {
		*(*uint32)(unsafe.Pointer(pdwEffect)) = dropEffectCopy
		// Run callback in goroutine to avoid blocking COM
		cb := dt.onDrop
		go cb(paths)
	}
	return comSOK
}

// ── IDataObject helpers ────────────────────────────────────────────────────

// dataObjHasHDROP calls IDataObject::QueryGetData (vtable index 5) to check
// if CF_HDROP format is available.
func dataObjHasHDROP(pDataObj uintptr) bool {
	fe := formatETC{
		cfFormat: cfHDROP,
		dwAspect: dvaspectContent,
		lindex:   -1,
		tymed:    tymedHGlobal,
	}
	// vtable pointer is the first field of the COM object
	vtblPtr := *(*uintptr)(unsafe.Pointer(pDataObj))
	// QueryGetData is at vtable index 5
	queryGetData := *(*uintptr)(unsafe.Pointer(vtblPtr + 5*unsafe.Sizeof(uintptr(0))))
	ret, _, _ := syscall.SyscallN(queryGetData, pDataObj, uintptr(unsafe.Pointer(&fe)))
	return ret == comSOK
}

// extractHDROPPaths calls IDataObject::GetData (vtable index 3) with CF_HDROP,
// then uses DragQueryFileW to extract file paths. Only reads path strings, never
// file content — safe for any file size.
func extractHDROPPaths(pDataObj uintptr) []string {
	fe := formatETC{
		cfFormat: cfHDROP,
		dwAspect: dvaspectContent,
		lindex:   -1,
		tymed:    tymedHGlobal,
	}
	var medium stgMEDIUM

	vtblPtr := *(*uintptr)(unsafe.Pointer(pDataObj))
	getData := *(*uintptr)(unsafe.Pointer(vtblPtr + 3*unsafe.Sizeof(uintptr(0))))
	ret, _, _ := syscall.SyscallN(getData, pDataObj, uintptr(unsafe.Pointer(&fe)), uintptr(unsafe.Pointer(&medium)))
	Log.Info("extractHDROPPaths: GetData", "hresult", ret)
	if ret != comSOK {
		return nil
	}

	hdrop := medium.hGlobal
	count, _, _ := procDragQueryFileW.Call(hdrop, 0xFFFFFFFF, 0, 0)

	var paths []string
	buf := make([]uint16, 4096)

	for i := uintptr(0); i < count; i++ {
		n, _, _ := procDragQueryFileW.Call(hdrop, i, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n > 0 {
			paths = append(paths, syscall.UTF16ToString(buf[:n]))
		}
	}

	procReleaseStgMedium.Call(uintptr(unsafe.Pointer(&medium)))
	return paths
}

// ── Setup ──────────────────────────────────────────────────────────────────

// setupNativeFileDrop registers a custom IDropTarget on the WebView2 content
// window. This intercepts OLE file drops at the Win32 level — our handler
// only reads CF_HDROP file paths (never file content), preventing the
// Chromium OOM/STATUS_BREAKPOINT crash on large files.
func setupNativeFileDrop(onDrop func([]string)) {
	// OleInitialize is required for RegisterDragDrop. Wails only calls
	// CoInitializeEx which is insufficient for OLE drag-drop. Calling
	// OleInitialize when COM is already initialized returns S_FALSE (OK).
	ret, _, _ := procOleInitialize.Call(0)
	Log.Info("OleInitialize", "hresult", ret)

	// Find the Wails window by title
	title, _ := windows.UTF16PtrFromString("LS Messager")
	hwnd, _, _ := pFindWindowW.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		Log.Warn("setupNativeFileDrop: 找不到主窗口")
		return
	}

	// Find all Chrome_WidgetWin_0 descendant windows
	chromeHwnds := findAllChromeWidgetChildren(hwnd)
	Log.Info("找到 Chrome_WidgetWin_0 窗口", "count", len(chromeHwnds))
	if len(chromeHwnds) == 0 {
		Log.Warn("setupNativeFileDrop: 找不到 Chrome_WidgetWin_0")
		return
	}

	// Create our custom IDropTarget
	gDropTargetVtbl = &dropTargetVtbl{
		QueryInterface: syscall.NewCallback(dtQueryInterface),
		AddRef:         syscall.NewCallback(dtAddRef),
		Release:        syscall.NewCallback(dtRelease),
		DragEnter:      syscall.NewCallback(dtDragEnter),
		DragOver:       syscall.NewCallback(dtDragOver),
		DragLeave:      syscall.NewCallback(dtDragLeave),
		Drop:           syscall.NewCallback(dtDrop),
	}
	gDropTarget = &goDropTarget{
		lpVtbl:   gDropTargetVtbl,
		refCount: 1,
		onDrop:   onDrop,
	}

	// Try each Chrome_WidgetWin_0: revoke any existing IDropTarget, then register ours.
	for _, ch := range chromeHwnds {
		procRevokeDragDrop.Call(ch) // ignore error — may not have one
		ret, _, _ = procRegisterDragDrop.Call(ch, uintptr(unsafe.Pointer(gDropTarget)))
		Log.Info("RegisterDragDrop", "hwnd", ch, "hresult", ret)
		if ret == comSOK {
			Log.Info("自定义拖放处理器注册成功", "hwnd", ch)
			return
		}
	}

	// Fallback: try the Wails window itself
	procRevokeDragDrop.Call(hwnd)
	ret, _, _ = procRegisterDragDrop.Call(hwnd, uintptr(unsafe.Pointer(gDropTarget)))
	Log.Info("RegisterDragDrop on Wails window", "hwnd", hwnd, "hresult", ret)
}

// _chromeHwnds collects all Chrome_WidgetWin_0 HWNDs found by EnumChildWindows.
var _chromeHwnds []uintptr

func findAllChromeWidgetChildren(parentHwnd uintptr) []uintptr {
	_chromeHwnds = nil
	cb := syscall.NewCallback(func(childHwnd, lParam uintptr) uintptr {
		var className [256]uint16
		pGetClassNameW.Call(childHwnd, uintptr(unsafe.Pointer(&className[0])), 256)
		if syscall.UTF16ToString(className[:]) == "Chrome_WidgetWin_0" {
			_chromeHwnds = append(_chromeHwnds, childHwnd)
		}
		return 1 // continue to find all
	})
	pEnumChildWindows.Call(parentHwnd, cb, 0)
	result := make([]uintptr, len(_chromeHwnds))
	copy(result, _chromeHwnds)
	return result
}
