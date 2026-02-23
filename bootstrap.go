package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// discoveredPeer holds info about a peer found during bootstrap discovery.
type discoveredPeer struct {
	IP      string
	Name    string
	WebPort int
}

// bootstrapWebView2 attempts to find and download WebView2 runtime from LAN peers.
// The splash window is updated with progress. Returns true if successful.
func bootstrapWebView2(localIP string, webPort int, splash *BootstrapSplash) bool {
	splash.SetText("正在搜索局域网中的 LANShare 节点...")

	// Discover peers via UDP broadcast
	peers := discoverPeersForBootstrap(localIP, webPort)
	if len(peers) == 0 {
		Log.Info("WebView2引导: 未发现局域网节点")
		return false
	}

	splash.SetText(fmt.Sprintf("发现 %d 个节点，正在获取运行时...", len(peers)))

	// Determine target directory (next to exe)
	exePath, err := os.Executable()
	if err != nil {
		Log.Error("无法确定程序路径", "error", err)
		return false
	}
	targetDir := filepath.Join(filepath.Dir(exePath), "WebView2Runtime")

	// Try each peer
	for _, peer := range peers {
		url := fmt.Sprintf("http://%s:%d/webview2runtime", peer.IP, peer.WebPort)
		splash.SetText(fmt.Sprintf("正在从 %s (%s) 获取运行时...", peer.Name, peer.IP))

		if downloadAndExtractWebView2(url, targetDir, splash) {
			Log.Info("WebView2运行时获取成功", "source", peer.Name, "ip", peer.IP)
			return true
		}
	}

	return false
}

// discoverPeersForBootstrap sends a UDP broadcast and collects responses from LAN peers.
func discoverPeersForBootstrap(localIP string, defaultWebPort int) []discoveredPeer {
	tempID := fmt.Sprintf("bootstrap_%s_%d", localIP, time.Now().Unix())

	// Listen on UDP port 9999 for responses
	listenAddr, err := net.ResolveUDPAddr("udp", ":9999")
	if err != nil {
		return nil
	}
	listenConn, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		// Port in use means another LANShare might be running on this machine
		fmt.Println("  (UDP 端口 9999 已被占用，可能有其他 LANShare 实例正在运行)")
		return nil
	}
	defer listenConn.Close()

	// Send discovery broadcast
	msg := DiscoveryMessage{
		Type:    "announce",
		ID:      tempID,
		Name:    "bootstrap",
		IP:      localIP,
		Port:    0, // Not listening for TCP connections
		WebPort: 0,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil
	}

	broadcastAddr, err := net.ResolveUDPAddr("udp", "255.255.255.255:9999")
	if err != nil {
		return nil
	}
	sendConn, err := net.DialUDP("udp", nil, broadcastAddr)
	if err != nil {
		return nil
	}
	sendConn.Write(data)
	sendConn.Close()

	// Send again after 1 second for reliability
	go func() {
		time.Sleep(1 * time.Second)
		conn, err := net.DialUDP("udp", nil, broadcastAddr)
		if err != nil {
			return
		}
		conn.Write(data)
		conn.Close()
	}()

	// Collect responses for 5 seconds
	listenConn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var peers []discoveredPeer
	seen := make(map[string]bool)
	buffer := make([]byte, 1024)

	for {
		n, _, err := listenConn.ReadFromUDP(buffer)
		if err != nil {
			break // Timeout or error
		}

		var resp DiscoveryMessage
		if json.Unmarshal(buffer[:n], &resp) != nil {
			continue
		}

		// Skip our own message
		if resp.ID == tempID {
			continue
		}

		// Skip duplicates
		if seen[resp.IP] {
			continue
		}
		seen[resp.IP] = true

		wp := resp.WebPort
		if wp == 0 {
			wp = defaultWebPort
		}

		peers = append(peers, discoveredPeer{
			IP:      resp.IP,
			Name:    resp.Name,
			WebPort: wp,
		})

		fmt.Printf("    发现节点: %s (%s)\n", resp.Name, resp.IP)
	}

	return peers
}

// downloadAndExtractWebView2 downloads the WebView2 runtime zip from a peer and extracts it.
func downloadAndExtractWebView2(url, targetDir string, splash *BootstrapSplash) bool {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		Log.Error("WebView2下载连接失败", "url", url, "error", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false
	}

	totalSize := resp.ContentLength

	// Download to temp file
	tmpFile, err := os.CreateTemp("", "webview2-*.zip")
	if err != nil {
		return false
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	// Download with progress
	var downloaded int64
	buf := make([]byte, 64*1024)
	lastUpdate := time.Now()

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := tmpFile.Write(buf[:n]); writeErr != nil {
				tmpFile.Close()
				return false
			}
			downloaded += int64(n)

			if time.Since(lastUpdate) > 500*time.Millisecond {
				if totalSize > 0 {
					pct := float64(downloaded) / float64(totalSize) * 100
					splash.SetText(fmt.Sprintf("正在下载运行时... %.0f%%\n(%.1f MB / %.1f MB)",
						pct, float64(downloaded)/1024/1024, float64(totalSize)/1024/1024))
				} else {
					splash.SetText(fmt.Sprintf("正在下载运行时... %.1f MB", float64(downloaded)/1024/1024))
				}
				lastUpdate = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			tmpFile.Close()
			return false
		}
	}
	tmpFile.Close()

	// Extract zip
	splash.SetText("正在解压运行时...")
	if err := extractZip(tmpPath, targetDir); err != nil {
		os.RemoveAll(targetDir)
		return false
	}

	return true
}

// extractZip extracts a zip file to the target directory.
func extractZip(zipPath, targetDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("打开zip文件失败: %w", err)
	}
	defer r.Close()

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("创建目标目录失败: %w", err)
	}

	for _, f := range r.File {
		fpath := filepath.Join(targetDir, f.Name)

		// Security: prevent zip slip (path traversal)
		if !isSubPath(targetDir, fpath) {
			continue
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, 0755)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(fpath), 0755); err != nil {
			return err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		rc.Close()
		outFile.Close()
		if err != nil {
			return err
		}
	}

	return nil
}

// isSubPath checks if child is under parent directory (prevents zip slip attacks).
func isSubPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// findServableWebView2Dir returns a WebView2 runtime directory that can be shared over LAN.
// Priority: 1) local WebView2Runtime/ folder next to exe, 2) system Evergreen Runtime.
func findServableWebView2Dir() string {
	// 1. Local bundled Fixed Version (preferred)
	if exePath, err := os.Executable(); err == nil {
		localDir := filepath.Join(filepath.Dir(exePath), "WebView2Runtime")
		if info, err := os.Stat(localDir); err == nil && info.IsDir() {
			return localDir
		}
	}

	// 2. System-installed WebView2 Evergreen Runtime
	candidates := []string{
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("ProgramFiles"),
		os.Getenv("LOCALAPPDATA"),
	}
	for _, root := range candidates {
		if root == "" {
			continue
		}
		appDir := filepath.Join(root, "Microsoft", "EdgeWebView", "Application")
		entries, err := os.ReadDir(appDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(appDir, entry.Name())
			if _, err := os.Stat(filepath.Join(dir, "msedgewebview2.exe")); err == nil {
				return dir
			}
		}
	}

	return ""
}

// serveWebView2Runtime handles HTTP requests for the WebView2 runtime zip.
// Serves from local WebView2Runtime/ folder or system-installed Evergreen Runtime.
func serveWebView2Runtime(w http.ResponseWriter, r *http.Request) {
	wv2Dir := findServableWebView2Dir()
	if wv2Dir == "" {
		http.Error(w, "WebView2Runtime not available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=WebView2Runtime.zip")

	zw := zip.NewWriter(w)
	defer zw.Close()

	filepath.Walk(wv2Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(wv2Dir, path)
		if err != nil {
			return err
		}

		// Create zip entry with compression
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relPath)
		header.Method = zip.Deflate

		fw, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(fw, f)
		return err
	})
}
