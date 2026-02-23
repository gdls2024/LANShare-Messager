package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// versionBase extracts the base semver (e.g., "1.0.1" from "1.0.1-beta").
func versionBase(v string) string {
	if idx := strings.Index(v, "-"); idx != -1 {
		return v[:idx]
	}
	return v
}

// versionChannel returns "stable" or "test" for a given version string.
func versionChannel(v string) string {
	if strings.Contains(v, "-") {
		return "test"
	}
	return "stable"
}

// compareVersions compares two semver strings (ignoring pre-release labels).
// Returns: 1 if a > b, -1 if a < b, 0 if equal.
func compareVersions(a, b string) int {
	aBase := versionBase(a)
	bBase := versionBase(b)

	aParts := strings.Split(aBase, ".")
	bParts := strings.Split(bBase, ".")

	maxLen := len(aParts)
	if len(bParts) > maxLen {
		maxLen = len(bParts)
	}

	for i := 0; i < maxLen; i++ {
		var aNum, bNum int
		if i < len(aParts) {
			aNum, _ = strconv.Atoi(aParts[i])
		}
		if i < len(bParts) {
			bNum, _ = strconv.Atoi(bParts[i])
		}
		if aNum > bNum {
			return 1
		}
		if aNum < bNum {
			return -1
		}
	}

	// Same base version: stable > test (e.g., "1.0.0" > "1.0.0-beta")
	aCh := versionChannel(a)
	bCh := versionChannel(b)
	if aCh == "stable" && bCh == "test" {
		return 1
	}
	if aCh == "test" && bCh == "stable" {
		return -1
	}

	return 0
}

// updateSource holds info about a peer that has a newer version.
type updateSource struct {
	IP      string `json:"ip"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Channel string `json:"channel"`
	WebPort int    `json:"webPort"`
}

// isNewer checks if source version is newer (any channel).
func isNewer(sourceVersion string) bool {
	return compareVersions(sourceVersion, AppVersion) > 0
}

// isCrossChannel returns true if the source version is in a different channel.
func isCrossChannel(sourceVersion string) bool {
	return versionChannel(sourceVersion) != AppChannel()
}

// checkForUpdates scans LAN peers for newer versions.
// Called after the P2P node starts, runs periodically in background.
func (node *P2PNode) checkForUpdates() {
	// Wait a few seconds for discovery to find peers
	time.Sleep(8 * time.Second)

	for {
		node.checkForUpdatesOnce()
		time.Sleep(60 * time.Second)
	}
}

// checkForUpdatesOnce performs a single update check against all known peers.
func (node *P2PNode) checkForUpdatesOnce() {
	var newestSource *updateSource

	node.PeersMutex.RLock()
	for _, peer := range node.Peers {
		if !peer.IsActive {
			continue
		}
		wp := peer.WebPort
		if wp == 0 {
			wp = node.WebPort // fallback to local port if peer's not known
		}
		source := checkPeerVersion(peer.IP, wp, peer.Name)
		if source != nil && isNewer(source.Version) {
			if newestSource == nil || compareVersions(source.Version, newestSource.Version) > 0 {
				newestSource = source
			}
		}
	}
	node.PeersMutex.RUnlock()

	if newestSource != nil {
		node.PeersMutex.RLock()
		alreadyKnown := node.AvailableUpdate != nil && node.AvailableUpdate.Version == newestSource.Version
		node.PeersMutex.RUnlock()

		if !alreadyKnown {
			Log.Info("发现新版本", "version", newestSource.Version, "source", newestSource.Name)
			channelLabel := "稳定版"
			if newestSource.Channel == "test" {
				channelLabel = "测试版"
			}
			fmt.Printf("\n╔═══════════════════════════════════════════╗\n")
			fmt.Printf("║  发现新版本: %s [%s]                  \n", newestSource.Version, channelLabel)
			fmt.Printf("║  当前版本: %s [%s]                    \n", AppVersion, AppChannel())
			fmt.Printf("║  来自: %s (%s)                        \n", newestSource.Name, newestSource.IP)
			fmt.Printf("║  使用 /update 命令更新                     \n")
			fmt.Printf("╚═══════════════════════════════════════════╝\n\n")

			// Reset update status so the new version can be downloaded
			node.UpdateStatus = ""
			node.UpdateError = ""

			// Emit Wails event for desktop UI
			node.emitUpdateAvailable(*newestSource)
		}

		node.PeersMutex.Lock()
		node.AvailableUpdate = newestSource
		node.PeersMutex.Unlock()
	}
}

// checkPeerVersion queries a peer's /version endpoint.
func checkPeerVersion(ip string, webPort int, name string) *updateSource {
	client := &http.Client{Timeout: 3 * time.Second}
	url := fmt.Sprintf("http://%s:%d/version", ip, webPort)
	resp, err := client.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	var info struct {
		Version string `json:"version"`
		Channel string `json:"channel"`
		Name    string `json:"name"`
	}
	if json.NewDecoder(resp.Body).Decode(&info) != nil {
		return nil
	}

	return &updateSource{
		IP:      ip,
		Name:    info.Name,
		Version: info.Version,
		Channel: info.Channel,
		WebPort: webPort,
	}
}

// performUpdate downloads the latest version from a peer and replaces the current exe.
func (node *P2PNode) performUpdate() {
	node.UpdateStatus = "downloading"
	node.UpdateError = ""

	node.PeersMutex.RLock()
	source := node.AvailableUpdate
	node.PeersMutex.RUnlock()

	if source == nil {
		source = findUpdateSource(node)
		if source == nil {
			node.UpdateStatus = "failed"
			node.UpdateError = "当前没有可用的更新"
			fmt.Println("当前没有可用的更新")
			return
		}
	}

	channelLabel := "稳定版"
	if source.Channel == "test" {
		channelLabel = "测试版"
	}
	fmt.Printf("正在从 %s (%s) 下载 %s %s...\n", source.Name, source.IP, channelLabel, source.Version)

	url := fmt.Sprintf("http://%s:%d/update", source.IP, source.WebPort)
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		node.UpdateStatus = "failed"
		node.UpdateError = fmt.Sprintf("下载失败: %v", err)
		fmt.Printf("下载失败: %v\n", err)
		Log.Error("更新下载失败", "url", url, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		node.UpdateStatus = "failed"
		node.UpdateError = fmt.Sprintf("下载失败: HTTP %d", resp.StatusCode)
		fmt.Printf("下载失败: HTTP %d\n", resp.StatusCode)
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		node.UpdateStatus = "failed"
		node.UpdateError = "获取程序路径失败"
		return
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	newPath := exePath + ".new"
	tmpFile, err := os.Create(newPath)
	if err != nil {
		node.UpdateStatus = "failed"
		node.UpdateError = "创建临时文件失败"
		return
	}

	totalSize := resp.ContentLength
	var downloaded int64
	buf := make([]byte, 64*1024)
	lastPrint := time.Now()

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := tmpFile.Write(buf[:n]); writeErr != nil {
				tmpFile.Close()
				os.Remove(newPath)
				node.UpdateStatus = "failed"
				node.UpdateError = "写入文件失败"
				return
			}
			downloaded += int64(n)

			if time.Since(lastPrint) > 500*time.Millisecond {
				if totalSize > 0 {
					pct := float64(downloaded) / float64(totalSize) * 100
					fmt.Printf("\r  下载中: %.1f MB / %.1f MB (%.0f%%)    ",
						float64(downloaded)/1024/1024, float64(totalSize)/1024/1024, pct)
				} else {
					fmt.Printf("\r  下载中: %.1f MB    ", float64(downloaded)/1024/1024)
				}
				lastPrint = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			tmpFile.Close()
			os.Remove(newPath)
			node.UpdateStatus = "failed"
			node.UpdateError = "下载中断"
			return
		}
	}
	tmpFile.Close()
	fmt.Printf("\r  下载完成: %.1f MB                              \n", float64(downloaded)/1024/1024)

	oldPath := exePath + ".old"
	os.Remove(oldPath)

	if err := os.Rename(exePath, oldPath); err != nil {
		Log.Error("重命名当前程序失败", "error", err)
		os.Remove(newPath)
		node.UpdateStatus = "failed"
		node.UpdateError = "替换程序文件失败"
		return
	}

	if err := os.Rename(newPath, exePath); err != nil {
		Log.Error("安装新版本失败", "error", err)
		os.Rename(oldPath, exePath)
		node.UpdateStatus = "failed"
		node.UpdateError = "安装新版本失败"
		return
	}

	fmt.Printf("\n更新成功! %s → %s\n", AppVersion, source.Version)
	Log.Info("更新成功", "oldVersion", AppVersion, "newVersion", source.Version)
	node.UpdateStatus = "completed"
}

// cleanupOldExecutable removes leftover .old files and restart scripts from previous updates.
func cleanupOldExecutable() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	// Clean up restart helper script
	scriptPath := filepath.Join(filepath.Dir(exePath), "_lanshare_restart.bat")
	os.Remove(scriptPath)

	// Clean up .old file with retries (may still be locked briefly on Windows)
	oldPath := exePath + ".old"
	if _, err := os.Stat(oldPath); err != nil {
		return
	}
	for i := 0; i < 5; i++ {
		if err := os.Remove(oldPath); err == nil {
			Log.Info("已清理旧版本文件", "path", oldPath)
			return
		}
		time.Sleep(time.Duration(i+1) * time.Second)
	}
	Log.Warn("无法清理旧版本文件", "path", oldPath)
}

// startNewProcess starts the new exe as a detached process.
// Returns the child PID on success. Does NOT exit the current process —
// the caller is responsible for triggering a proper shutdown.
func startNewProcess() (int, error) {
	exePath, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("获取程序路径失败: %v", err)
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	// After self-update, the running exe was renamed to .old while the new
	// version was placed at the original path. On Windows, os.Executable()
	// may return the renamed (.old) path. We must start the NEW exe instead.
	if strings.HasSuffix(exePath, ".old") {
		exePath = strings.TrimSuffix(exePath, ".old")
	}

	// Verify the target exe exists
	if _, err := os.Stat(exePath); err != nil {
		return 0, fmt.Errorf("目标程序不存在: %s (%v)", exePath, err)
	}

	Log.Info("启动新进程", "exePath", exePath)

	// Pass -restart-delay flag so the new process retries mutex acquisition
	// for up to N seconds, giving the old process time to fully shut down.
	cmd := exec.Command(exePath, "-restart-delay", "10")
	cmd.Dir = filepath.Dir(exePath)
	// Detach child process so it survives parent exit (platform-specific)
	setDetachedProcess(cmd)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("启动新版本失败: %v", err)
	}
	Log.Info("已启动新版本进程", "pid", cmd.Process.Pid)
	return cmd.Process.Pid, nil
}

// restartApplication starts the new exe and force-exits the current process.
// On Windows, uses a helper batch script to wait for this process to fully die
// (including WebView2 children), clean up .old files, then launch the new exe.
// On other platforms, falls back to direct child process launch.
func restartApplication(node *P2PNode) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取程序路径失败: %v", err)
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	// Resolve actual target path (running exe may be the .old renamed copy)
	targetPath := exePath
	if strings.HasSuffix(exePath, ".old") {
		targetPath = strings.TrimSuffix(exePath, ".old")
	}
	if _, err := os.Stat(targetPath); err != nil {
		return fmt.Errorf("目标程序不存在: %s (%v)", targetPath, err)
	}

	// Save config before exiting
	if node != nil && node.Config != nil {
		SaveConfig(node.Config)
	}

	// Clean up desktop resources (sharing server, etc.)
	if node != nil && node.OnBeforeRestart != nil {
		node.OnBeforeRestart()
	}

	Log.Info("正在重启到新版本", "targetPath", targetPath)

	// Try platform-specific restart helper (batch script on Windows).
	// Falls back to direct child process launch if helper is unavailable.
	if err := launchRestartHelper(targetPath); err != nil {
		Log.Warn("重启脚本不可用，使用直接启动", "error", err)
		pid, startErr := startNewProcess()
		if startErr != nil {
			return startErr
		}
		Log.Info("已直接启动新进程", "pid", pid)
	}

	time.Sleep(200 * time.Millisecond)
	os.Exit(0)
	return nil
}

// findUpdateSource searches LAN peers for one with a newer version in the same channel.
func findUpdateSource(node *P2PNode) *updateSource {
	var best *updateSource

	// First check known peers
	node.PeersMutex.RLock()
	for _, peer := range node.Peers {
		if !peer.IsActive {
			continue
		}
		wp := peer.WebPort
		if wp == 0 {
			wp = node.WebPort
		}
		source := checkPeerVersion(peer.IP, wp, peer.Name)
		if source != nil && isNewer(source.Version) {
			if best == nil || compareVersions(source.Version, best.Version) > 0 {
				best = source
			}
		}
	}
	node.PeersMutex.RUnlock()

	if best != nil {
		return best
	}

	// If no known peers have updates, try UDP discovery
	fmt.Println("正在搜索局域网中的更新源...")
	peers := discoverPeersForUpdate(node.LocalIP, node.WebPort)
	for _, peer := range peers {
		source := checkPeerVersion(peer.IP, peer.WebPort, peer.Name)
		if source != nil && isNewer(source.Version) {
			if best == nil || compareVersions(source.Version, best.Version) > 0 {
				best = source
			}
		}
	}

	return best
}

// discoverPeersForUpdate is a lightweight peer discovery for update checking.
func discoverPeersForUpdate(localIP string, defaultWebPort int) []discoveredPeer {
	tempID := fmt.Sprintf("updater_%s_%d", localIP, time.Now().Unix())

	msg := DiscoveryMessage{
		Type:    "announce",
		ID:      tempID,
		Name:    "updater",
		IP:      localIP,
		Port:    0,
		Version: AppVersion,
	}
	data, _ := json.Marshal(msg)

	broadcastAddr, _ := net.ResolveUDPAddr("udp", "255.255.255.255:9999")
	sendConn, err := net.DialUDP("udp", nil, broadcastAddr)
	if err != nil {
		return nil
	}
	sendConn.Write(data)
	sendConn.Close()

	// Listen for responses (use a random port to avoid conflicts)
	listenAddr, _ := net.ResolveUDPAddr("udp", ":0")
	listenConn, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return nil
	}
	defer listenConn.Close()

	listenConn.SetReadDeadline(time.Now().Add(3 * time.Second))

	var peers []discoveredPeer
	seen := make(map[string]bool)
	buffer := make([]byte, 1024)

	for {
		n, _, err := listenConn.ReadFromUDP(buffer)
		if err != nil {
			break
		}

		var resp DiscoveryMessage
		if json.Unmarshal(buffer[:n], &resp) != nil {
			continue
		}
		if resp.ID == tempID || seen[resp.IP] {
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
	}

	return peers
}
