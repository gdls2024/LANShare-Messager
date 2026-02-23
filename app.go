package main

import (
	"archive/zip"
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/gen2brain/beeep"
	"github.com/ra1phdd/systray-on-wails"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed build/appicon.png
var appIconPNG []byte

// DesktopApp is the Wails application binding struct.
// Methods on this struct are exposed to the frontend via window.go.main.DesktopApp.
type DesktopApp struct {
	ctx                context.Context
	node               *P2PNode
	cfg                *AppConfig
	sharingServer      *http.Server
	lastNotifiedChatId string
}

// NewDesktopApp creates a new DesktopApp instance.
func NewDesktopApp(node *P2PNode, cfg *AppConfig) *DesktopApp {
	return &DesktopApp{node: node, cfg: cfg}
}

// APIHandler returns the HTTP handler for Wails AssetServer.
// This reuses the same handler that powers the standalone Web UI.
func (a *DesktopApp) APIHandler() http.Handler {
	return a.node.createHTTPHandler()
}

// startup is called when the Wails app starts.
func (a *DesktopApp) startup(ctx context.Context) {
	tStartup := time.Now()
	Log.Debug("Wails OnStartup 回调开始")
	a.ctx = ctx

	// Default notification app name (JS updates via SetNotificationAppName on theme change)
	beeep.AppName = "LS Messager"

	// Set up event callbacks to push real-time events to frontend
	a.node.OnNewMessage = func(msg ChatMessage) {
		wailsRuntime.EventsEmit(a.ctx, EventNewMessage, msg)
	}
	a.node.OnUserOnline = func(name string) {
		wailsRuntime.EventsEmit(a.ctx, EventUserOnline, name)
	}
	a.node.OnUserOffline = func(name string) {
		wailsRuntime.EventsEmit(a.ctx, EventUserOffline, name)
	}
	a.node.OnUpdateAvailable = func(source updateSource) {
		wailsRuntime.EventsEmit(a.ctx, EventUpdateAvailable, source)
	}
	a.node.OnBeforeRestart = func() {
		if a.sharingServer != nil {
			shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			a.sharingServer.Shutdown(shutCtx)
		}
	}
	a.node.OnQuitApp = func() {
		wailsRuntime.Quit(a.ctx)
	}
	Log.Debug("Wails OnStartup: 事件回调注册完成", "耗时", time.Since(tStartup))

	// Start P2P node (TCP listener, discovery, message handling)
	tStep := time.Now()
	if err := a.node.Start(); err != nil {
		Log.Error("启动P2P节点失败", "error", err, "mode", "desktop")
	}
	Log.Debug("Wails OnStartup: node.Start() 完成", "耗时", time.Since(tStep), "tcpPort", a.node.LocalPort, "ip", a.node.LocalIP)

	// Start LAN sharing server (serves Web UI and WebView2 runtime to LAN peers)
	tStep = time.Now()
	a.startSharingServer()
	Log.Debug("Wails OnStartup: startSharingServer 调度完成", "耗时", time.Since(tStep), "webPort", a.node.WebPort)

	Log.Debug("Wails OnStartup 回调结束", "总耗时", time.Since(tStartup))
}

// onDomReady is called when the WebView2 DOM is fully loaded.
func (a *DesktopApp) onDomReady(ctx context.Context) {
	Log.Debug("Wails OnDomReady 回调触发 — 界面已可交互")
}

// shutdown is called when the Wails app is closing.
func (a *DesktopApp) shutdown(ctx context.Context) {
	// Save current settings to config before exit
	a.cfg.Name = a.node.Name
	a.cfg.WebPort = a.node.WebPort
	a.cfg.BlockedUsers = collectBlockedUsers(a.node)

	// Save window size
	w, h := wailsRuntime.WindowGetSize(ctx)
	Log.Info("shutdown: WindowGetSize", "w", w, "h", h)
	if w > 0 && h > 0 {
		a.cfg.WindowWidth = w
		a.cfg.WindowHeight = h
	}

	Log.Info("shutdown: saving config", "windowWidth", a.cfg.WindowWidth, "windowHeight", a.cfg.WindowHeight)
	if err := SaveConfig(a.cfg); err != nil {
		Log.Error("保存配置失败", "error", err)
	}

	// Clear chat history if save-history is disabled
	if !a.cfg.IsSaveHistory() && a.node.DB != nil {
		a.node.DB.Exec("DELETE FROM messages")
		Log.Info("已清空聊天记录（保存聊天记录已关闭）")
	}

	// Shut down LAN sharing HTTP server
	if a.sharingServer != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		a.sharingServer.Shutdown(shutCtx)
	}

	systray.Quit()
	a.node.Stop()
}

// showWindow brings the application window to the foreground.
func (a *DesktopApp) showWindow() {
	wailsRuntime.Show(a.ctx)
	wailsRuntime.WindowUnminimise(a.ctx)
}

// GetAndClearLastNotifiedChat returns the chatId from the last notification
// and clears it. Called by JS on window focus to auto-switch chat.
func (a *DesktopApp) GetAndClearLastNotifiedChat() string {
	chatId := a.lastNotifiedChatId
	a.lastNotifiedChatId = ""
	return chatId
}

// toggleWindow shows the window if hidden/minimized, hides it if visible.
func (a *DesktopApp) toggleWindow() {
	visible, minimized := isAppWindowVisible()
	if visible && !minimized {
		wailsRuntime.Hide(a.ctx)
	} else {
		a.showWindow()
	}
}

// initSystray sets up the system tray icon and menu.
// Right-click: context menu with "打开界面" and "退出".
// Double-click: toggle window visibility.
func (a *DesktopApp) initSystray() {
	systray.Register(func() {
		systray.SetIcon(trayIcon())
		systray.SetTooltip(fmt.Sprintf("域信 v%s - %s", AppVersion, a.node.Name))

		mShow := systray.AddMenuItem("打开界面", "打开域信主窗口")
		mQuit := systray.AddMenuItem("退出", "退出域信")

		// Subclass the systray window to support double-click toggle
		// and restrict left-click from showing the menu (right-click only).
		subclassSystray(a.toggleWindow)

		go func() {
			for {
				select {
				case <-mShow.ClickedCh:
					a.showWindow()
				case <-mQuit.ClickedCh:
					wailsRuntime.Quit(a.ctx)
					return
				}
			}
		}()
	}, nil)
}

// SetNotificationAppName sets the app name shown in Windows toast notifications.
// Called from JS when theme changes: "即时通" for wisetalk, "LS Messager" for telegram.
func (a *DesktopApp) SetNotificationAppName(name string) {
	beeep.AppName = name
}

// ShowNotification sends a system notification and tracks which chat triggered it.
func (a *DesktopApp) ShowNotification(title, body, chatId string) {
	a.lastNotifiedChatId = chatId
	beeep.Notify(title, body, "")
}

// OpenFileDialog opens a native file selection dialog.
func (a *DesktopApp) OpenFileDialog() (string, error) {
	return wailsRuntime.OpenFileDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "选择文件",
	})
}

// OpenImageDialog opens a native image selection dialog.
func (a *DesktopApp) OpenImageDialog() (string, error) {
	return wailsRuntime.OpenFileDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "选择图片",
		Filters: []wailsRuntime.FileFilter{
			{DisplayName: "图片文件", Pattern: "*.jpg;*.jpeg;*.png;*.gif;*.bmp;*.webp"},
		},
	})
}

// SaveFileDialog opens a native save file dialog.
func (a *DesktopApp) SaveFileDialog(defaultFilename string) (string, error) {
	return wailsRuntime.SaveFileDialog(a.ctx, wailsRuntime.SaveDialogOptions{
		Title:           "保存文件",
		DefaultFilename: defaultFilename,
	})
}

// RevealInExplorer opens the system file explorer at the given file path.
func (a *DesktopApp) RevealInExplorer(filePath string) error {
	switch goruntime.GOOS {
	case "windows":
		return exec.Command("explorer", "/select,", filePath).Start()
	case "darwin":
		return exec.Command("open", "-R", filePath).Start()
	case "linux":
		return exec.Command("xdg-open", filepath.Dir(filePath)).Start()
	default:
		return fmt.Errorf("unsupported OS: %s", goruntime.GOOS)
	}
}

// OpenFile opens a file with its default application.
func (a *DesktopApp) OpenFile(filePath string) error {
	switch goruntime.GOOS {
	case "windows":
		return exec.Command("cmd", "/c", "start", "", filePath).Start()
	case "darwin":
		return exec.Command("open", filePath).Start()
	case "linux":
		return exec.Command("xdg-open", filePath).Start()
	default:
		return fmt.Errorf("unsupported OS: %s", goruntime.GOOS)
	}
}

// startSharingServer starts an HTTP server on the LAN for P2P sharing.
// This serves the WebView2 runtime and Web UI to other devices on the network.
func (a *DesktopApp) startSharingServer() {
	handler := a.node.createHTTPHandler()
	basePort := a.node.WebPort
	go func() {
		// Retry same port with backoff (port may not be released immediately after restart)
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			addr := fmt.Sprintf(":%d", a.node.WebPort)
			srv := &http.Server{Addr: addr, Handler: handler}
			a.sharingServer = srv
			lastErr = srv.ListenAndServe()
			if lastErr == http.ErrServerClosed {
				return // graceful shutdown
			}
			Log.Info("HTTP端口被占用，等待重试", "port", a.node.WebPort, "attempt", attempt+1)
			time.Sleep(time.Duration(attempt+1) * time.Second)
		}
		// Try next ports
		for offset := 1; offset <= 10; offset++ {
			tryPort := basePort + offset
			addr := fmt.Sprintf(":%d", tryPort)
			srv := &http.Server{Addr: addr, Handler: handler}
			a.sharingServer = srv
			lastErr = srv.ListenAndServe()
			if lastErr == http.ErrServerClosed {
				return
			}
			if lastErr == nil {
				a.node.WebPort = tryPort
				Log.Info("HTTP使用备用端口", "originalPort", basePort, "actualPort", tryPort)
				fmt.Printf("HTTP端口 %d 被占用，已切换到 %d\n", basePort, tryPort)
				return
			}
		}
		fmt.Printf("LAN共享服务器启动失败 (端口 %d): %v\n", basePort, lastErr)
		Log.Error("LAN共享服务器启动失败", "port", basePort, "error", lastErr)
	}()
	fmt.Printf("LAN共享服务器已启动: http://%s:%d\n", a.node.LocalIP, a.node.WebPort)
	Log.Info("LAN共享服务器已启动", "ip", a.node.LocalIP, "port", a.node.WebPort)
}

// OpenLogDir opens the log directory in the system file explorer.
func (a *DesktopApp) OpenLogDir() error {
	logDir := LogDir()
	os.MkdirAll(logDir, 0755)
	switch goruntime.GOOS {
	case "windows":
		return exec.Command("explorer", logDir).Start()
	case "darwin":
		return exec.Command("open", logDir).Start()
	case "linux":
		return exec.Command("xdg-open", logDir).Start()
	default:
		return fmt.Errorf("unsupported OS: %s", goruntime.GOOS)
	}
}

// SendFile opens a file dialog and initiates transfer without going through WebView2.
// Returns map with fileId and fileName on success, or error.
func (a *DesktopApp) SendFile(targetName string) (map[string]string, error) {
	filePath, err := wailsRuntime.OpenFileDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "选择要发送的文件",
	})
	if err != nil {
		return nil, err
	}
	if filePath == "" {
		return nil, fmt.Errorf("cancelled")
	}

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("文件不存在: %v", err)
	}

	fileId := a.node.sendFileTransferRequest(filePath, targetName)
	if fileId == "" {
		return nil, fmt.Errorf("发送文件失败")
	}

	return map[string]string{
		"fileId":   fileId,
		"fileName": fileInfo.Name(),
		"fileSize": fmt.Sprintf("%d", fileInfo.Size()),
	}, nil
}

// SendFilePath initiates a file transfer from a local path without going through WebView2.
// Used for drag-and-drop where the path is known.
// If the path is a directory, it is zipped into a temporary archive before sending.
func (a *DesktopApp) SendFilePath(filePath, targetName string) (map[string]string, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("文件不存在: %v", err)
	}

	sendPath := filePath
	if fileInfo.IsDir() {
		// Zip the directory to a temp file
		zipPath, err := zipDirectory(filePath)
		if err != nil {
			return nil, fmt.Errorf("压缩文件夹失败: %v", err)
		}
		sendPath = zipPath
		fileInfo, _ = os.Stat(zipPath)
	}

	fileId := a.node.sendFileTransferRequest(sendPath, targetName)
	if fileId == "" {
		return nil, fmt.Errorf("发送文件失败")
	}

	return map[string]string{
		"fileId":   fileId,
		"fileName": fileInfo.Name(),
		"fileSize": fmt.Sprintf("%d", fileInfo.Size()),
	}, nil
}

// zipDirectory compresses a directory into a temporary .zip file.
// Returns the path to the zip file.
func zipDirectory(dirPath string) (string, error) {
	dirName := filepath.Base(dirPath)
	tmpDir := DataPath("tmp")
	os.MkdirAll(tmpDir, 0755)
	zipPath := filepath.Join(tmpDir, dirName+".zip")

	zipFile, err := os.Create(zipPath)
	if err != nil {
		return "", err
	}
	defer zipFile.Close()

	w := zip.NewWriter(zipFile)
	defer w.Close()

	err = filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Get relative path from the parent of dirPath so the zip contains the folder name
		relPath, err := filepath.Rel(filepath.Dir(dirPath), path)
		if err != nil {
			return err
		}
		// Use forward slashes in zip
		relPath = filepath.ToSlash(relPath)

		if info.IsDir() {
			// Add directory entry
			_, err := w.Create(relPath + "/")
			return err
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = relPath
		header.Method = zip.Deflate

		writer, err := w.CreateHeader(header)
		if err != nil {
			return err
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(writer, file)
		return err
	})

	if err != nil {
		os.Remove(zipPath)
		return "", err
	}

	return zipPath, nil
}

// SendImagePath sends an image from a local file path without going through WebView2.
// Reads the image from disk, saves a copy, and broadcasts/sends to the target peer.
func (a *DesktopApp) SendImagePath(filePath, targetName string) (map[string]string, error) {
	imageData, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("读取图片失败: %v", err)
	}

	fileName := filepath.Base(filePath)
	ext := strings.ToLower(filepath.Ext(fileName))
	if ext == "" {
		ext = ".jpg"
	}

	// Determine MIME type from extension
	contentType := "image/jpeg"
	switch ext {
	case ".png":
		contentType = "image/png"
	case ".gif":
		contentType = "image/gif"
	case ".bmp":
		contentType = "image/bmp"
	case ".webp":
		contentType = "image/webp"
	}

	if len(imageData) > 5<<20 {
		return nil, fmt.Errorf("图片文件不能超过5MB")
	}

	// Save to images directory
	imageDir := DataPath("images")
	os.MkdirAll(imageDir, 0755)
	imageFileName := fmt.Sprintf("%s_%d%s", generateMessageID(), time.Now().Unix(), ext)
	imagePath := filepath.Join(imageDir, imageFileName)
	if err := os.WriteFile(imagePath, imageData, 0644); err != nil {
		return nil, fmt.Errorf("保存图片失败: %v", err)
	}

	imageBase64 := base64.StdEncoding.EncodeToString(imageData)
	imageURL := fmt.Sprintf("/images/%s", imageFileName)
	messageID := generateMessageID()

	imageMsg := Message{
		Type:        "chat",
		From:        a.node.ID,
		Content:     fmt.Sprintf("发送了图片: %s", fileName),
		Timestamp:   time.Now(),
		MessageType: MessageTypeImage,
		MessageID:   messageID,
		FileName:    fileName,
		FileSize:    int64(len(imageData)),
		FileType:    contentType,
		FileURL:     imageURL,
		FileData:    imageBase64,
	}

	if targetName == "all" {
		imageMsg.To = "all"
		a.node.broadcastMessage(imageMsg)
	} else {
		var targetID string
		a.node.PeersMutex.RLock()
		for id, peer := range a.node.Peers {
			if peer.Name == targetName && peer.IsActive {
				targetID = id
				break
			}
		}
		a.node.PeersMutex.RUnlock()

		if targetID == "" {
			return nil, fmt.Errorf("目标用户不在线")
		}

		imageMsg.To = targetID
		if peer, exists := a.node.Peers[targetID]; exists {
			a.node.sendMessageToPeer(peer, imageMsg)
		}
	}

	isPrivate := targetName != "all"
	a.node.addChatMessageWithType(
		a.node.Name, targetName, imageMsg.Content, true, isPrivate,
		MessageTypeImage, messageID, "", "", "", fileName, int64(len(imageData)), contentType, imageURL, "",
	)

	return map[string]string{
		"status":    "success",
		"imageUrl":  imageURL,
		"messageId": messageID,
	}, nil
}

// SendImageBase64 sends an image from base64-encoded data (used for clipboard paste).
// The frontend reads the pasted image as a data URL, strips the prefix, and passes the raw base64.
func (a *DesktopApp) SendImageBase64(base64Data, fileName, fileType, targetName string) (map[string]string, error) {
	imageData, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return nil, fmt.Errorf("解码图片数据失败: %v", err)
	}

	if len(imageData) > 5<<20 {
		return nil, fmt.Errorf("图片文件不能超过5MB")
	}

	if fileName == "" {
		fileName = fmt.Sprintf("paste_%d.png", time.Now().UnixMilli())
	}
	if fileType == "" {
		fileType = "image/png"
	}

	// Determine extension from MIME type
	ext := ".png"
	switch fileType {
	case "image/jpeg":
		ext = ".jpg"
	case "image/gif":
		ext = ".gif"
	case "image/bmp":
		ext = ".bmp"
	case "image/webp":
		ext = ".webp"
	}

	// Save to images directory
	imageDir := DataPath("images")
	os.MkdirAll(imageDir, 0755)
	imageFileName := fmt.Sprintf("%s_%d%s", generateMessageID(), time.Now().Unix(), ext)
	imagePath := filepath.Join(imageDir, imageFileName)
	if err := os.WriteFile(imagePath, imageData, 0644); err != nil {
		return nil, fmt.Errorf("保存图片失败: %v", err)
	}

	imageBase64 := base64Data
	imageURL := fmt.Sprintf("/images/%s", imageFileName)
	messageID := generateMessageID()

	imageMsg := Message{
		Type:        "chat",
		From:        a.node.ID,
		Content:     fmt.Sprintf("发送了图片: %s", fileName),
		Timestamp:   time.Now(),
		MessageType: MessageTypeImage,
		MessageID:   messageID,
		FileName:    fileName,
		FileSize:    int64(len(imageData)),
		FileType:    fileType,
		FileURL:     imageURL,
		FileData:    imageBase64,
	}

	if targetName == "all" {
		imageMsg.To = "all"
		a.node.broadcastMessage(imageMsg)
	} else {
		var targetID string
		a.node.PeersMutex.RLock()
		for id, peer := range a.node.Peers {
			if peer.Name == targetName && peer.IsActive {
				targetID = id
				break
			}
		}
		a.node.PeersMutex.RUnlock()

		if targetID == "" {
			return nil, fmt.Errorf("目标用户不在线")
		}

		imageMsg.To = targetID
		if peer, exists := a.node.Peers[targetID]; exists {
			a.node.sendMessageToPeer(peer, imageMsg)
		}
	}

	isPrivate := targetName != "all"
	a.node.addChatMessageWithType(
		a.node.Name, targetName, imageMsg.Content, true, isPrivate,
		MessageTypeImage, messageID, "", "", "", fileName, int64(len(imageData)), fileType, imageURL, "",
	)

	return map[string]string{
		"status":    "success",
		"imageUrl":  imageURL,
		"messageId": messageID,
	}, nil
}

// SendFileFromBase64 saves base64-encoded file data to a temp file and initiates P2P transfer.
// Used for JS drag-drop and paste where we have blob data, not file paths.
func (a *DesktopApp) SendFileFromBase64(base64Data, fileName, targetName string) (map[string]string, error) {
	Log.Info("SendFileFromBase64", "fileName", fileName, "target", targetName, "base64Len", len(base64Data))

	data, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		Log.Error("SendFileFromBase64: base64解码失败", "error", err, "first100", base64Data[:min(100, len(base64Data))])
		return nil, fmt.Errorf("解码文件数据失败: %v", err)
	}
	Log.Info("SendFileFromBase64: decoded", "bytes", len(data))

	tempDir := DataPath("temp")
	os.MkdirAll(tempDir, 0755)
	tempPath := filepath.Join(tempDir, fileName)
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		Log.Error("SendFileFromBase64: 写入失败", "path", tempPath, "error", err)
		return nil, fmt.Errorf("保存临时文件失败: %v", err)
	}

	// Verify file was written correctly
	if fi, err := os.Stat(tempPath); err == nil {
		Log.Info("SendFileFromBase64: 文件已保存", "path", tempPath, "size", fi.Size())
	}

	fileId := a.node.sendFileTransferRequest(tempPath, targetName)
	if fileId == "" {
		os.Remove(tempPath)
		return nil, fmt.Errorf("发送文件失败")
	}

	Log.Info("SendFileFromBase64: 传输请求已发送", "fileId", fileId)
	return map[string]string{
		"fileId":   fileId,
		"fileName": fileName,
		"fileSize": fmt.Sprintf("%d", len(data)),
	}, nil
}

// SaveWindowSize saves the current window dimensions to config.
// Called from JS on window resize events — more reliable than saving only on shutdown,
// because HideWindowOnClose means the window often hides without triggering shutdown.
func (a *DesktopApp) SaveWindowSize() {
	w, h := wailsRuntime.WindowGetSize(a.ctx)
	if w > 0 && h > 0 {
		a.cfg.WindowWidth = w
		a.cfg.WindowHeight = h
		SaveConfig(a.cfg)
	}
}

// SetWindowTheme switches the window title bar between dark and light.
func (a *DesktopApp) SetWindowTheme(theme string) {
	if theme == "light" {
		wailsRuntime.WindowSetLightTheme(a.ctx)
	} else {
		wailsRuntime.WindowSetDarkTheme(a.ctx)
	}
}

// SetWindowIcon changes the native window icon to match the active skin.
func (a *DesktopApp) SetWindowIcon(skinId string) {
	setWindowIcon(skinId)
}

// GetAppInfo returns application info for the frontend.
func (a *DesktopApp) GetAppInfo() map[string]interface{} {
	return map[string]interface{}{
		"name":      a.node.Name,
		"localIP":   a.node.LocalIP,
		"localPort": a.node.LocalPort,
		"webPort":   a.node.WebPort,
		"id":        a.node.ID,
		"version":   AppVersion,
		"channel":   AppChannel(),
	}
}
