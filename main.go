package main

import (
	"bufio"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

// 生成消息ID
func generateMessageID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)
}

// 创建新的P2P节点
func NewP2PNode(name string, webEnabled bool, localIP string) *P2PNode {
	t := time.Now()
	if localIP == "" {
		localIP = getLocalIP()
	}
	nodeID := fmt.Sprintf("%s_%d", localIP, time.Now().Unix())
	address := fmt.Sprintf("%s:%d", localIP, 8888)

	// Generate node-level ECDH key pair (persistent for node lifetime)
	tStep := time.Now()
	nodePrivKey, nodePubKey, keyErr := generateECDHKeyPair()
	if keyErr != nil {
		Log.Error("节点密钥生成失败", "error", keyErr)
	}
	Log.Debug("ECDH密钥生成完成", "耗时", time.Since(tStep), "pubKeyLen", len(nodePubKey))

	node := &P2PNode{
		LocalIP:        localIP,
		LocalPort:      8888,
		Name:           name,
		ID:             nodeID,
		Address:        address,
		NodePrivateKey: nodePrivKey,
		NodePublicKey:  nodePubKey,
		Peers:          make(map[string]*Peer),
		MessageChan:    make(chan Message, 100),
		StopCh:         make(chan struct{}),
		Running:        false,
		DiscoveryPort:  9999,
		WebPort:        8080,
		Messages:       make([]ChatMessage, 0),
		WebEnabled:     webEnabled,
		FileTransfers:  make(map[string]*FileTransferStatus),
		ACLs:           make(map[string]map[string]bool),
		ACLMutex:       sync.RWMutex{},
	}

	// 初始化数据库
	dbPath := DataPath("message.db")
	tStep = time.Now()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		Log.Error("打开数据库失败", "error", err, "path", dbPath)
		node.DB = nil
		return node
	}
	node.DB = db
	Log.Debug("sql.Open 完成", "耗时", time.Since(tStep), "path", dbPath)

	tStep = time.Now()
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			sender TEXT NOT NULL,
			recipient TEXT,
			content BLOB NOT NULL,
			nonce BLOB,
			is_private BOOLEAN DEFAULT FALSE,
			is_own BOOLEAN DEFAULT FALSE,
			message_type TEXT DEFAULT 'text',
			message_id TEXT,
			reply_to_id TEXT,
			reply_to_content TEXT,
			reply_to_sender TEXT,
			file_name TEXT,
			file_size INTEGER DEFAULT 0,
			file_type TEXT,
			file_url TEXT,
			file_data TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_timestamp ON messages(timestamp DESC);
		CREATE INDEX IF NOT EXISTS idx_chat ON messages(recipient, is_private);
		CREATE INDEX IF NOT EXISTS idx_message_type ON messages(message_type);
		CREATE INDEX IF NOT EXISTS idx_message_id ON messages(message_id);
	`)
	if err != nil {
		Log.Error("创建数据库表失败", "error", err)
		db.Close()
		node.DB = nil
		return node
	}
	Log.Debug("CREATE TABLE 完成", "耗时", time.Since(tStep))

	// Migration: add file_id column (fails silently if already exists)
	db.Exec("ALTER TABLE messages ADD COLUMN file_id TEXT DEFAULT ''")

	// 清理旧消息（保留30天）
	tStep = time.Now()
	result, err := db.Exec("DELETE FROM messages WHERE timestamp < DATETIME('now', '-30 days')")
	if err != nil {
		Log.Error("清理旧消息失败", "error", err)
	} else {
		deleted, _ := result.RowsAffected()
		Log.Debug("清理旧消息完成", "耗时", time.Since(tStep), "deleted", deleted)
	}

	// Now load history with proper key
	tStep = time.Now()
	node.loadHistoryFromDB()
	Log.Debug("loadHistoryFromDB 完成", "耗时", time.Since(tStep), "loadedMessages", len(node.Messages))

	// 设置 WAL 模式以提高并发
	tStep = time.Now()
	var walMode string
	db.QueryRow("PRAGMA journal_mode=WAL;").Scan(&walMode)
	Log.Debug("PRAGMA journal_mode=WAL", "耗时", time.Since(tStep), "result", walMode)

	Log.Debug("NewP2PNode 完成", "总耗时", time.Since(t), "name", name, "localIP", localIP, "nodeID", nodeID)
	return node
}

// clearHistoryIfDisabled clears the message DB on startup if save-history is disabled.
// This handles cases where the app crashed before shutdown cleanup could run.
func (node *P2PNode) clearHistoryIfDisabled() {
	if node.Config != nil && !node.Config.IsSaveHistory() && node.DB != nil {
		node.DB.Exec("DELETE FROM messages")
		node.Messages = node.Messages[:0]
		Log.Info("启动时清空聊天记录（保存聊天记录已关闭）")
	}
}

// ACL 方法实现
func (node *P2PNode) isBlocked(targetAddress string) bool {
	node.ACLMutex.RLock()
	defer node.ACLMutex.RUnlock()
	if acl, exists := node.ACLs[node.Address]; exists {
		if val, ok := acl[targetAddress]; ok {
			return !val
		}
	}
	return false
}

func (node *P2PNode) blockUser(targetAddress string) {
	node.ACLMutex.Lock()
	defer node.ACLMutex.Unlock()
	if node.ACLs[node.Address] == nil {
		node.ACLs[node.Address] = make(map[string]bool)
	}
	node.ACLs[node.Address][targetAddress] = false
	// 查找用户名显示
	displayName := targetAddress
	node.PeersMutex.RLock()
	for _, peer := range node.Peers {
		if peer.Address == targetAddress {
			displayName = peer.Name
			break
		}
	}
	node.PeersMutex.RUnlock()
	fmt.Printf("已屏蔽用户 %s (%s)\n", displayName, targetAddress)
}

func (node *P2PNode) unblockUser(targetAddress string) {
	node.ACLMutex.Lock()
	defer node.ACLMutex.Unlock()
	if node.ACLs[node.Address] == nil {
		node.ACLs[node.Address] = make(map[string]bool)
	}
	node.ACLs[node.Address][targetAddress] = true
	// 查找用户名显示
	displayName := targetAddress
	node.PeersMutex.RLock()
	for _, peer := range node.Peers {
		if peer.Address == targetAddress {
			displayName = peer.Name
			break
		}
	}
	node.PeersMutex.RUnlock()
	fmt.Printf("已解除屏蔽用户 %s (%s)\n", displayName, targetAddress)
}

func (node *P2PNode) showACL() {
	node.ACLMutex.RLock()
	defer node.ACLMutex.RUnlock()
	
	fmt.Println("屏蔽列表:")
	if acl, exists := node.ACLs[node.Address]; exists {
		blocked := 0
		for addr, allowed := range acl {
			if !allowed {
				// 查找用户名
				displayName := addr
				node.PeersMutex.RLock()
				for _, peer := range node.Peers {
					if peer.Address == addr {
						displayName = peer.Name
						break
					}
				}
				node.PeersMutex.RUnlock()
				fmt.Printf(" - %s (%s)\n", displayName, addr)
				blocked++
			}
		}
		if blocked == 0 {
			fmt.Println("  无屏蔽用户")
		}
	} else {
		fmt.Println("  无屏蔽用户")
	}
}

// 启动P2P节点
func (node *P2PNode) Start() error {
	Log.Debug("node.Start() 开始", "localIP", node.LocalIP, "basePort", node.LocalPort)
	// 启动TCP监听器（带重试 + 自动端口递增）
	basePort := node.LocalPort
	var listener net.Listener
	var err error

	// First: retry the same port a few times (port may be releasing after restart)
	tStep := time.Now()
	for attempt := 0; attempt < 3; attempt++ {
		addr := fmt.Sprintf("%s:%d", node.LocalIP, node.LocalPort)
		listener, err = net.Listen("tcp", addr)
		if err == nil {
			Log.Debug("TCP监听成功", "addr", addr, "attempt", attempt+1, "耗时", time.Since(tStep))
			break
		}
		Log.Debug("TCP端口被占用，等待重试", "port", node.LocalPort, "attempt", attempt+1, "error", err)
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}

	// If still failed, try next ports
	if err != nil {
		Log.Debug("TCP主端口全部重试失败，尝试备用端口", "basePort", basePort, "totalRetryTime", time.Since(tStep))
		for offset := 1; offset <= 10; offset++ {
			tryPort := basePort + offset
			addr := fmt.Sprintf("%s:%d", node.LocalIP, tryPort)
			listener, err = net.Listen("tcp", addr)
			if err == nil {
				node.LocalPort = tryPort
				Log.Debug("TCP使用备用端口", "originalPort", basePort, "actualPort", tryPort)
				break
			}
			Log.Debug("TCP备用端口也被占用", "port", tryPort, "error", err)
		}
	}
	if err != nil {
		Log.Error("TCP监听全部失败", "error", err, "总耗时", time.Since(tStep))
		return fmt.Errorf("启动TCP监听失败: %v", err)
	}
	node.Listener = listener
	node.Running = true

	Log.Info("P2P节点启动", "ip", node.LocalIP, "port", node.LocalPort, "name", node.Name, "version", AppVersion)

	// 启动后台goroutines
	go node.checkForUpdates()
	if node.WebEnabled && !node.DesktopMode {
		node.startWebGUI()
	}
	go node.startDiscovery()
	go node.handleMessages()
	go node.acceptConnections()
	go node.periodicBroadcast()

	Log.Debug("node.Start() 所有goroutine已启动")
	return nil
}

// 显示命令帮助信息
func (node *P2PNode) showCommandHelp() {
	fmt.Println("\n===========================================")
	fmt.Println("           LANShare P2P 客户端 - 帮助")
	fmt.Println("===========================================")
	fmt.Println("命令说明:")
	fmt.Println("  直接输入消息 - 公聊")
	fmt.Println("  /to <用户名> <消息> - 私聊")
	fmt.Println("  /send <用户名> <文件路径> - 发送文件")
	fmt.Println("  /accept <文件ID> - 接受文件")
	fmt.Println("  /reject <文件ID> - 拒绝文件")
	fmt.Println("  /transfers - 查看文件传输列表")
	fmt.Println("  /list - 查看在线用户")
	fmt.Println("  /name <新名称> - 更改用户名")
	fmt.Println("  /web [端口] - 打开Web界面 (默认8080)")
	fmt.Println("  /webstop - 关闭Web界面")
	fmt.Println("  /webstatus - 查看Web状态")
	fmt.Println("  /block <用户名> - 屏蔽用户")
	fmt.Println("  /unblock <用户名> - 解除屏蔽")
	fmt.Println("  /acl - 查看屏蔽列表")
	fmt.Println("  /history [用户名] [数量] - 查看历史消息 (默认20条)")
	fmt.Println("  /update - 从局域网获取最新版本")
	fmt.Println("  /version - 显示版本信息")
	fmt.Println("  /help - 显示帮助信息")
	fmt.Println("  /quit - 退出程序")
	fmt.Println("===========================================")
}

// 启动命令行界面
func (node *P2PNode) startCLI() {
	fmt.Println("\n===========================================")
	fmt.Println("           LANShare P2P 客户端")
	fmt.Println("===========================================")
	node.showCommandHelp()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}

		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}

		if strings.HasPrefix(text, "/") {
			if text == "/quit" {
				break
			}
			node.handleCommand(text)
		} else {
			// 公聊消息
			msg := Message{
				Type:      "chat",
				From:      node.ID,
				To:        "all",
				Content:   text,
				Timestamp: time.Now(),
			}
			node.broadcastMessage(msg)
			node.addChatMessage("我", "all", text, true, false)
		}
	}

	node.Stop()
}

// 处理命令
func (node *P2PNode) handleCommand(command string) {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return
	}

	switch parts[0] {
	case "/to":
		if len(parts) < 3 {
			fmt.Println("用法: /to <用户名> <消息>")
			return
		}
		
		targetName := parts[1]
		message := strings.Join(parts[2:], " ")
		
		// 查找目标用户
		var targetID, targetAddress string
		node.PeersMutex.RLock()
		for id, peer := range node.Peers {
			if peer.Name == targetName && peer.IsActive {
				targetID = id
				targetAddress = peer.Address
				break
			}
		}
		node.PeersMutex.RUnlock()
		
		if targetID == "" {
			fmt.Printf("错误: 用户 '%s' 不在线或不存在\n", targetName)
			fmt.Println("提示: 使用 /list 命令查看在线用户")
			return
		}

		if node.isBlocked(targetAddress) {
			fmt.Printf("错误: 用户 '%s' 被屏蔽，无法发送私聊\n", targetName)
			fmt.Println("提示: 使用 /unblock 命令解除屏蔽")
			return
		}
		
		msg := Message{
			Type:      "chat",
			From:      node.ID,
			To:        targetID,
			Content:   message,
			Timestamp: time.Now(),
		}
		
		if peer, exists := node.Peers[targetID]; exists {
			node.sendMessageToPeer(peer, msg)
			node.addChatMessage(node.Name, targetName, message, true, true)
		}
		
	case "/list":
		fmt.Println("在线用户:")
		fmt.Printf("  %s (自己)\n", node.Name)
		
		node.PeersMutex.RLock()
		for _, peer := range node.Peers {
			if peer.IsActive {
				blocked := node.isBlocked(peer.Address)
				status := ""
				if blocked {
					status = " (屏蔽)"
				}
				fmt.Printf("  %s%s (%s)\n", peer.Name, status, peer.Address)
			}
		}
		node.PeersMutex.RUnlock()
		
	case "/name":
		if len(parts) < 2 {
			fmt.Println("用法: /name <新名称>")
			return
		}
		oldName := node.Name
		node.Name = parts[1]
		fmt.Printf("用户名已从 %s 更改为 %s\n", oldName, node.Name)

		// Save to config immediately
		if node.Config != nil {
			node.Config.Name = node.Name
			SaveConfig(node.Config)
		}

		// 广播名称更新消息
		updateMsg := Message{
			Type:    "update_name",
			From:    node.ID,
			To:      "all",
			Content: node.Name,
		}
		node.broadcastMessage(updateMsg)
		
	case "/web":
		if !node.WebEnabled {
			// 动态启用Web界面
			node.WebEnabled = true
			
			// 支持指定端口
			if len(parts) > 1 {
				if port, err := strconv.Atoi(parts[1]); err == nil && port > 0 && port < 65536 {
					node.WebPort = port
					fmt.Printf("Web端口设置为: %d\n", node.WebPort)
				} else {
					fmt.Println("无效端口号，使用默认端口 8080")
				}
			}
			
			node.startWebGUI()
		}
		
	case "/webstop":
		if !node.WebEnabled {
			fmt.Println("Web界面未启用")
			return
		}
		node.stopWebGUI()
		
	case "/block":
		if len(parts) < 2 {
			fmt.Println("用法: /block <用户名>")
			return
		}
		targetName := parts[1]
		// 查找目标地址
		var targetAddress string
		node.PeersMutex.RLock()
		for _, peer := range node.Peers {
			if peer.Name == targetName && peer.IsActive {
				targetAddress = peer.Address
				break
			}
		}
		node.PeersMutex.RUnlock()
		
		if targetAddress == "" {
			fmt.Printf("用户 %s 不在线，无法屏蔽\n", targetName)
			return
		}
		node.blockUser(targetAddress)
		
	case "/unblock":
		if len(parts) < 2 {
			fmt.Println("用法: /unblock <用户名>")
			return
		}
		targetName := parts[1]
		// 查找目标地址
		var targetAddress string
		node.PeersMutex.RLock()
		for _, peer := range node.Peers {
			if peer.Name == targetName && peer.IsActive {
				targetAddress = peer.Address
				break
			}
		}
		node.PeersMutex.RUnlock()
		
		if targetAddress == "" {
			fmt.Printf("用户 %s 不在线，但仍可解除屏蔽\n", targetName)
			// 尝试直接用用户名作为地址（向后兼容，但不推荐）
			node.unblockUser(targetName)
			return
		}
		node.unblockUser(targetAddress)
		
	case "/acl":
		node.showACL()
		
	case "/send":
		if len(parts) < 3 {
			fmt.Println("用法: /send <用户名> <文件路径>")
			return
		}
		targetName := parts[1]
		filePath := strings.Join(parts[2:], " ")
		
		// 查找目标用户
		var targetID, targetAddress string
		node.PeersMutex.RLock()
		for id, peer := range node.Peers {
			if peer.Name == targetName && peer.IsActive {
				targetID = id
				targetAddress = peer.Address
				break
			}
		}
		node.PeersMutex.RUnlock()
		
		if targetID == "" {
			fmt.Printf("错误: 用户 '%s' 不在线或不存在\n", targetName)
			fmt.Println("提示: 使用 /list 命令查看在线用户")
			return
		}

		if node.isBlocked(targetAddress) {
			fmt.Printf("错误: 用户 '%s' 被屏蔽，无法发送文件\n", targetName)
			fmt.Println("提示: 使用 /unblock 命令解除屏蔽")
			return
		}
		node.sendFileTransferRequest(filePath, targetName)
		
	case "/transfers":
		node.showFileTransfers()

	case "/accept":
		if len(parts) < 2 {
			fmt.Println("用法: /accept <文件ID>")
			return
		}
		node.respondToFileTransfer(parts[1], true)

	case "/reject":
		if len(parts) < 2 {
			fmt.Println("用法: /reject <文件ID>")
			return
		}
		node.respondToFileTransfer(parts[1], false)
		
	case "/webstatus":
		if node.WebEnabled {
			webURL := fmt.Sprintf("http://127.0.0.1:%d", node.WebPort)
			fmt.Printf("Web界面已启用\n地址: %s\n端口: %d\n", webURL, node.WebPort)
		} else {
			fmt.Println("Web界面未启用")
		}
		
	case "/update":
		node.performUpdate()

	case "/version":
		channelLabel := "稳定版"
		if AppChannel() == "test" {
			channelLabel = "测试版"
		}
		fmt.Printf("LANShare %s [%s]\n", AppVersion, channelLabel)

	case "/help":
		node.showCommandHelp()

	case "/history":
		chatId := "all"
		limit := 20
		if len(parts) > 1 {
			chatId = parts[1]
		}
		if len(parts) > 2 {
			if l, err := strconv.Atoi(parts[2]); err == nil && l > 0 {
				limit = l
			}
		}

		if node.DB == nil {
			fmt.Println("数据库未初始化")
			return
		}

		var rows *sql.Rows
		var err error

		if chatId == "all" {
			rows, err = node.DB.Query(`
				SELECT sender, recipient, content, nonce, is_private, is_own, timestamp,
					   message_type, message_id, reply_to_id, reply_to_content, reply_to_sender,
					   file_name, file_size, file_type
				FROM messages
				WHERE recipient = 'all' AND is_private = FALSE
				ORDER BY timestamp DESC
				LIMIT ?
			`, limit)
		} else {
			rows, err = node.DB.Query(`
				SELECT sender, recipient, content, nonce, is_private, is_own, timestamp,
					   message_type, message_id, reply_to_id, reply_to_content, reply_to_sender,
					   file_name, file_size, file_type
				FROM messages
				WHERE is_private = TRUE AND (
					(sender = ? AND recipient = ?) OR
					(sender = ? AND recipient = ?)
				)
				ORDER BY timestamp DESC
				LIMIT ?
			`, node.Name, chatId, chatId, node.Name, limit)
		}

		if err != nil {
			fmt.Printf("查询历史消息失败: %v\n", err)
			return
		}
		defer rows.Close()

		fmt.Printf("历史消息 (%s, 最近 %d 条):\n", chatId, limit)
		count := 0
		for rows.Next() {
			var sender, recipient string
			var content, nonce []byte
			var isPrivate, isOwn bool
			var timestamp time.Time
			var messageType, messageID, replyToID, replyToContent, replyToSender, fileName, fileType string
			var fileSize int64

			err = rows.Scan(&sender, &recipient, &content, &nonce, &isPrivate, &isOwn, &timestamp,
				&messageType, &messageID, &replyToID, &replyToContent, &replyToSender,
				&fileName, &fileSize, &fileType)
			if err != nil {
				continue
			}

			// 解密
			plaintext, err := decryptMessage(node.LocalDBKey, content, nonce)
			if err != nil {
				fmt.Printf("解密消息失败: %v\n", err)
				continue
			}

			displayContent := string(plaintext)
			if strings.HasPrefix(displayContent, "emoji:") {
				displayContent = "[表情]"
			} else if messageType == "image" && fileName != "" {
				displayContent = fmt.Sprintf("[图片: %s]", fileName)
			} else if messageType == "file" && fileName != "" {
				displayContent = fmt.Sprintf("[文件: %s (%s)]", fileName, formatFileSize(fileSize))
			} else if messageType == "reply" && replyToSender != "" {
				displayContent = fmt.Sprintf("[回复 %s]: %s", replyToSender, displayContent)
			}

			prefix := ""
			if isPrivate {
				prefix = "(私聊) "
			}
			if isOwn {
				fmt.Printf("[%s] 我 %s%s: %s\n", timestamp.Format("15:04:05"), prefix, recipient, displayContent)
			} else {
				fmt.Printf("[%s] %s %s: %s\n", timestamp.Format("15:04:05"), sender, prefix, displayContent)
			}
			count++
		}
		if count == 0 {
			fmt.Println("无历史消息")
		}
		
	default:
		fmt.Printf("未知命令: %s\n", parts[0])
	}
}

// 清理内存 - 移除旧的完成/失败传输
func (node *P2PNode) cleanupMemory() {
	now := time.Now()

	// 每5分钟清理一次
	if now.Sub(node.lastCleanupTime) < 5*time.Minute {
		return
	}
	node.lastCleanupTime = now

	node.FileTransfersMutex.Lock()
	defer node.FileTransfersMutex.Unlock()

	// 清理完成或失败超过10分钟的传输
	var toDelete []string
	for id, transfer := range node.FileTransfers {
		if (transfer.Status == "completed" || transfer.Status == "failed") &&
		   now.Sub(transfer.EndTime) > 10*time.Minute {
			toDelete = append(toDelete, id)
		}
	}

	for _, id := range toDelete {
		delete(node.FileTransfers, id)
	}

	if len(toDelete) > 0 {
		fmt.Printf("已清理 %d 个旧文件传输记录\n", len(toDelete))
	}
}

// 停止节点 (idempotent — safe to call multiple times)
func (node *P2PNode) Stop() {
	node.Running = false

	// Signal all goroutines to stop before closing connections.
	// If StopCh is already closed, this is a duplicate call — bail out.
	select {
	case <-node.StopCh:
		Log.Debug("node.Stop() 重复调用，跳过")
		return
	default:
		close(node.StopCh)
	}

	if node.Listener != nil {
		node.Listener.Close()
	}

	if node.BroadcastConn != nil {
		node.BroadcastConn.Close()
	}

	node.PeersMutex.Lock()
	for _, peer := range node.Peers {
		peer.Conn.Close()
	}
	node.PeersMutex.Unlock()

	close(node.MessageChan)
	if node.DB != nil {
		node.DB.Close()
	}
	fmt.Println("P2P节点已停止")
	Log.Info("P2P节点已停止", "name", node.Name)
}

func (node *P2PNode) loadHistoryFromDB() {
	if node.DB == nil {
		return
	}

	rows, err := node.DB.Query(`
		SELECT sender, recipient, content, nonce, is_private, is_own, timestamp,
			   message_type, message_id, reply_to_id, reply_to_content, reply_to_sender,
			   file_name, file_size, file_type, file_url, file_data, COALESCE(file_id, '')
		FROM messages
		ORDER BY timestamp DESC
		LIMIT 20
	`)
	if err != nil {
		fmt.Printf("加载历史消息失败: %v\n", err)
		Log.Error("加载历史消息失败", "error", err)
		return
	}
	defer rows.Close()

	var dbMsgs []ChatMessage
	for rows.Next() {
		var sender, recipient string
		var content, nonce []byte
		var isPrivate, isOwn bool
		var ts time.Time
		var messageType, messageID, replyToID, replyToContent, replyToSender, fileName, fileType string
		var fileSize int64

		var fileURL string
		var fileData string
		var fileID string
		if err := rows.Scan(&sender, &recipient, &content, &nonce, &isPrivate, &isOwn, &ts,
			&messageType, &messageID, &replyToID, &replyToContent, &replyToSender,
			&fileName, &fileSize, &fileType, &fileURL, &fileData, &fileID); err != nil {
			continue
		}

		plaintext, err := decryptMessage(node.LocalDBKey, content, nonce)
		if err != nil {
			fmt.Printf("解密历史消息失败: %v\n", err)
			continue
		}

		cm := ChatMessage{
			Sender:        sender,
			Recipient:     recipient,
			Content:       string(plaintext),
			Timestamp:     ts,
			IsOwn:         isOwn,
			IsPrivate:     isPrivate,
			MessageType:   messageType,
			MessageID:     messageID,
			ReplyToID:     replyToID,
			ReplyToContent: replyToContent,
			ReplyToSender: replyToSender,
			FileName:      fileName,
			FileSize:      fileSize,
			FileType:      fileType,
			FileURL:       fileURL,
			FileID:        fileID,
		}
		dbMsgs = append(dbMsgs, cm)
	}

	// Reverse to get oldest first (ASC)
	for i, j := 0, len(dbMsgs)-1; i < j; i, j = i+1, j-1 {
		dbMsgs[i], dbMsgs[j] = dbMsgs[j], dbMsgs[i]
	}

	node.MessagesMutex.Lock()
	node.Messages = dbMsgs
	node.MessagesMutex.Unlock()
}

func main() {
	appStart := time.Now()
	var name string
	var cliMode bool
	var webPort int
	var showHelp bool
	var logLevel string
	var restartDelay int

	flag.StringVar(&name, "name", "", "指定用户名")
	flag.BoolVar(&cliMode, "cli", false, "CLI模式（默认为桌面应用模式）")
	flag.IntVar(&webPort, "port", 0, "Web/应用端口")
	flag.BoolVar(&showHelp, "help", false, "显示帮助信息")
	flag.StringVar(&logLevel, "loglevel", "", "日志级别: error, info, debug")
	flag.IntVar(&restartDelay, "restart-delay", 0, "启动前等待秒数（重启用）")
	flag.Parse()

	// Prevent multiple instances.
	// When restartDelay > 0 (restart scenario), ensureSingleInstance will retry
	// the mutex acquisition for up to restartDelay seconds, giving the old process
	// time to fully exit and release the mutex.
	t := time.Now()
	cleanup := ensureSingleInstance(restartDelay)
	defer cleanup()
	fmt.Printf("单实例检查完成 (%v)\n", time.Since(t))

	// Load persistent config; CLI flags override saved values
	t = time.Now()
	cfg := LoadConfig()
	fmt.Printf("配置加载完成 (%v)\n", time.Since(t))
	if name != "" {
		cfg.Name = name
	}
	if webPort != 0 {
		cfg.WebPort = webPort
	}
	if logLevel != "" {
		cfg.LogLevel = logLevel
	}
	// Ensure defaults for zero values
	if cfg.WebPort == 0 {
		cfg.WebPort = 8080
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "debug"
	}

	dataDir := AppDataDir()
	fmt.Printf("数据目录: %s\n", dataDir)

	// 清理旧版本遗留文件
	cleanupOldExecutable()

	t = time.Now()
	logFile, err := InitLogger(cfg.LogLevel)
	if err != nil {
		fmt.Printf("初始化日志失败: %v\n", err)
	} else {
		defer logFile.Close()
	}
	// From here on, Log.Debug is available
	Log.Debug("日志系统初始化完成", "耗时", time.Since(t), "级别", cfg.LogLevel)
	Log.Debug("main() 启动参数", "name", name, "cli", cliMode, "webPort", webPort, "logLevel", logLevel, "restartDelay", restartDelay)
	Log.Debug("main() 最终配置", "cfgName", cfg.Name, "cfgWebPort", cfg.WebPort, "cfgLogLevel", cfg.LogLevel, "dataDir", dataDir)
	Log.Debug("main() 预初始化耗时", "elapsed", time.Since(appStart))

	if showHelp {
		fmt.Printf("LANShare P2P v%s [%s] - 局域网即时通信工具\n", AppVersion, AppChannel())
		fmt.Println()
		fmt.Println("用法:")
		fmt.Printf("  %s [选项]\n", os.Args[0])
		fmt.Println()
		fmt.Println("选项:")
		fmt.Println("  -name string    指定用户名")
		fmt.Println("  -cli            CLI模式（默认为桌面应用模式）")
		fmt.Println("  -port int       Web/应用端口 (默认 8080)")
		fmt.Println("  -loglevel string 日志级别: error, info, debug (默认 error)")
		fmt.Println("  -help           显示此帮助信息")
		fmt.Println()
		fmt.Println("模式:")
		fmt.Println("  默认模式: 启动桌面应用（Wails WebView窗口）")
		fmt.Println("  -cli模式: 命令行界面 + 可选Web界面")
		fmt.Println()
		fmt.Println("网络端口:")
		fmt.Println("  P2P通信: 8888 (TCP)")
		fmt.Println("  服务发现: 9999 (UDP)")
		fmt.Println()
		fmt.Printf("数据目录: %s\n", AppDataDir())
		return
	}

	if cfg.Name == "" {
		if len(flag.Args()) > 0 {
			cfg.Name = flag.Args()[0]
		}
	}

	if cliMode {
		startCLIMode(cfg)
	} else {
		startDesktopMode(cfg)
	}
}

// CLI模式启动（保留交互式提示）
func startCLIMode(cfg *AppConfig) {
	fmt.Println("===========================================")
	fmt.Println("           LANShare P2P 启动器 (CLI)")
	fmt.Println("===========================================")

	if cfg.Name == "" {
		fmt.Print("请输入您的用户名 (留空使用默认): ")
		var inputName string
		fmt.Scanln(&inputName)
		if inputName != "" {
			cfg.Name = inputName
		} else {
			cfg.Name = "用户_" + strconv.Itoa(int(time.Now().Unix()%10000))
		}
	}

	localIP := getLocalIP()

	webMode := false
	fmt.Print("是否启用Web界面? (y/N): ")
	var webInput string
	fmt.Scanln(&webInput)
	if strings.ToLower(strings.TrimSpace(webInput)) == "y" {
		webMode = true
	}

	node := NewP2PNode(cfg.Name, webMode, localIP)
	node.Config = cfg
	node.clearHistoryIfDisabled()

	if webMode {
		node.WebPort = cfg.WebPort
		fmt.Print("请输入Web端口 (默认" + strconv.Itoa(cfg.WebPort) + "): ")
		var portInput string
		fmt.Scanln(&portInput)
		if portInput != "" {
			if port, err := strconv.Atoi(portInput); err == nil && port > 0 && port < 65536 {
				node.WebPort = port
				fmt.Printf("Web端口设置为: %d\n", port)
			} else {
				fmt.Println("无效端口，使用默认端口")
			}
		}
	}

	// Apply blocked users from config
	applyBlockedUsers(node, cfg)

	if err := node.Start(); err != nil {
		fmt.Printf("启动P2P节点失败: %v\n", err)
		Log.Error("启动P2P节点失败", "error", err, "mode", "cli")
		return
	}

	// Save config with current name
	cfg.Name = node.Name
	cfg.WebPort = node.WebPort
	SaveConfig(cfg)

	node.startCLI()
}

// applyBlockedUsers restores blocked users from config into the node's ACL.
func applyBlockedUsers(node *P2PNode, cfg *AppConfig) {
	if len(cfg.BlockedUsers) == 0 {
		return
	}
	node.ACLMutex.Lock()
	defer node.ACLMutex.Unlock()
	if node.ACLs[node.Address] == nil {
		node.ACLs[node.Address] = make(map[string]bool)
	}
	for _, addr := range cfg.BlockedUsers {
		node.ACLs[node.Address][addr] = false
	}
}

// collectBlockedUsers extracts blocked user addresses from the node's ACL.
func collectBlockedUsers(node *P2PNode) []string {
	node.ACLMutex.RLock()
	defer node.ACLMutex.RUnlock()
	var blocked []string
	if acl, exists := node.ACLs[node.Address]; exists {
		for addr, allowed := range acl {
			if !allowed {
				blocked = append(blocked, addr)
			}
		}
	}
	return blocked
}

// 桌面应用模式启动（Wails）
func startDesktopMode(cfg *AppConfig) {
	tDesktop := time.Now()

	hostname, _ := os.Hostname()
	if cfg.Name == "" {
		if hostname != "" {
			cfg.Name = hostname
		} else {
			cfg.Name = "用户_" + strconv.Itoa(int(time.Now().Unix()%10000))
		}
	}
	Log.Debug("桌面模式: 用户名确定", "name", cfg.Name, "hostname", hostname)

	tStep := time.Now()
	localIP := getLocalIPAuto()
	Log.Debug("桌面模式: 本地IP获取", "耗时", time.Since(tStep), "localIP", localIP)

	tStep = time.Now()
	node := NewP2PNode(cfg.Name, true, localIP)
	Log.Debug("桌面模式: NewP2PNode 返回", "耗时", time.Since(tStep))

	node.DesktopMode = true
	node.Config = cfg
	node.clearHistoryIfDisabled()
	node.WebPort = cfg.WebPort

	applyBlockedUsers(node, cfg)
	Log.Debug("桌面模式: 屏蔽列表已应用", "blockedCount", len(cfg.BlockedUsers))

	// Persist config immediately so it survives non-graceful exits (e.g., Task Manager kill, crash, PC reboot).
	// Without this, users who never do a graceful exit lose their generated name/port on every launch.
	if err := SaveConfig(cfg); err != nil {
		Log.Error("初始配置保存失败", "error", err)
	} else {
		Log.Debug("桌面模式: 初始配置已保存", "path", configPath())
	}

	app := NewDesktopApp(node, cfg)

	tStep = time.Now()
	app.initSystray()
	Log.Debug("桌面模式: initSystray 完成", "耗时", time.Since(tStep))

	tStep = time.Now()
	webviewPaths := buildWebViewPathCandidates(localIP, cfg.WebPort)
	pathLabels := make([]string, len(webviewPaths))
	for i, wp := range webviewPaths {
		pathLabels[i] = wp.label
	}
	Log.Debug("桌面模式: WebView2候选路径", "耗时", time.Since(tStep), "candidates", pathLabels)

	Log.Debug("桌面模式: wails.Run 之前总耗时", "elapsed", time.Since(tDesktop))

	var lastErr error
	for i, wp := range webviewPaths {
		label := wp.label
		path := wp.path
		Log.Debug("桌面模式: 尝试启动 Wails", "strategy", label, "path", path, "attempt", i+1, "windowSize", fmt.Sprintf("%dx%d", cfg.WindowWidth, cfg.WindowHeight))
		tWails := time.Now()

		lastErr = wails.Run(&options.App{
			Title:             "LS Messager",
			Width:             cfg.WindowWidth,
			Height:            cfg.WindowHeight,
			MinWidth:          380,
			MinHeight:         400,
			HideWindowOnClose: true,
			AssetServer: &assetserver.Options{
				Handler: app.APIHandler(),
			},
			DragAndDrop: &options.DragAndDrop{
				EnableFileDrop:     true,
				DisableWebViewDrop: true,
				CSSDropProperty:    "--wails-drop-target",
				CSSDropValue:       "drop",
			},
			Windows: &windows.Options{
				WebviewIsTransparent:                false,
				WindowIsTranslucent:                 false,
				WebviewBrowserPath:                  path,
				WebviewDisableRendererCodeIntegrity: true,
				CustomTheme: &windows.ThemeSettings{
					// Dark mode (Telegram skin): standard dark title bar
					DarkModeTitleBar:           windows.RGB(23, 33, 43),
					DarkModeTitleBarInactive:   windows.RGB(23, 33, 43),
					DarkModeTitleText:          windows.RGB(200, 200, 200),
					DarkModeTitleTextInactive:  windows.RGB(120, 120, 120),
					DarkModeBorder:             windows.RGB(23, 33, 43),
					DarkModeBorderInactive:     windows.RGB(23, 33, 43),
					// Light mode (WiseTalk skin): blue title bar (#0089FF)
					LightModeTitleBar:          windows.RGB(0, 137, 255),
					LightModeTitleBarInactive:  windows.RGB(0, 106, 200),
					LightModeTitleText:         windows.RGB(255, 255, 255),
					LightModeTitleTextInactive: windows.RGB(200, 220, 255),
					LightModeBorder:            windows.RGB(0, 137, 255),
					LightModeBorderInactive:    windows.RGB(0, 106, 200),
				},
			},
			OnStartup:  app.startup,
			OnDomReady: app.onDomReady,
			OnShutdown: app.shutdown,
			Bind:       []interface{}{app},
		})
		if lastErr == nil {
			Log.Debug("桌面模式: wails.Run 正常退出", "strategy", label, "运行时长", time.Since(tWails))
			return
		}
		Log.Warn("Wails 启动失败，尝试下一策略", "strategy", label, "error", lastErr, "耗时", time.Since(tWails))
	}

	// 所有策略都失败
	fmt.Printf("所有启动策略均失败: %v\n", lastErr)
	Log.Error("所有 WebView2 策略均失败", "error", lastErr)
	errSplash := NewBootstrapSplash()
	errMsg := fmt.Sprintf("启动失败: %v\n\n请尝试以下方案：\n1. 确保局域网中有其他域信节点在运行后重试\n2. 手动下载 WebView2 运行时:\n   https://developer.microsoft.com/en-us/microsoft-edge/webview2/\n3. 使用 -cli 参数启动命令行模式", lastErr)
	errSplash.ShowError(errMsg)
}

// webviewCandidate represents one WebView2 startup strategy.
type webviewCandidate struct {
	label string // human-readable description
	path  string // WebviewBrowserPath ("" = system auto-detect)
}

// buildWebViewPathCandidates returns an ordered list of WebView2 paths to try.
// Priority: system auto-detect → local Fixed Version → LAN bootstrap → empty (last resort).
func buildWebViewPathCandidates(localIP string, webPort int) []webviewCandidate {
	var candidates []webviewCandidate

	tStep := time.Now()
	systemInstalled := isWebView2SystemInstalled()
	Log.Debug("WebView2系统检测", "耗时", time.Since(tStep), "installed", systemInstalled)

	tStep = time.Now()
	localPath := detectWebView2Runtime()
	Log.Debug("WebView2本地检测", "耗时", time.Since(tStep), "localPath", localPath)

	if systemInstalled {
		// 策略1: 系统 WebView2（空路径，Wails 自动查找）
		candidates = append(candidates, webviewCandidate{
			label: "系统 WebView2 (自动检测)",
			path:  "",
		})
	}

	if localPath != "" {
		// 策略2: 本地 Fixed Version（exe 旁边的 WebView2Runtime/）
		candidates = append(candidates, webviewCandidate{
			label: fmt.Sprintf("本地 Fixed Version (%s)", localPath),
			path:  localPath,
		})
	}

	if !systemInstalled && localPath == "" {
		// 没有任何 WebView2，先尝试从局域网获取
		splash := NewBootstrapSplash()
		splash.Show()
		splash.SetText("正在从内网搜索必须的运行时，请稍候...")

		if bootstrapWebView2(localIP, webPort, splash) {
			splash.Close()
			if bootstrapPath := detectWebView2Runtime(); bootstrapPath != "" {
				candidates = append(candidates, webviewCandidate{
					label: fmt.Sprintf("局域网获取 (%s)", bootstrapPath),
					path:  bootstrapPath,
				})
			}
		} else {
			splash.Close()
			Log.Warn("WebView2 局域网引导失败")
			fmt.Println("局域网未找到可用运行时")
		}
	}

	// 最后兜底：空路径直接尝试（某些系统可能通过非标准途径提供 WebView2）
	if len(candidates) == 0 || (len(candidates) > 0 && candidates[0].path != "") {
		// 确保空路径在候选列表中（如果还没有的话）
		hasEmpty := false
		for _, c := range candidates {
			if c.path == "" {
				hasEmpty = true
				break
			}
		}
		if !hasEmpty {
			candidates = append(candidates, webviewCandidate{
				label: "直接启动 (兜底方案)",
				path:  "",
			})
		}
	}

	return candidates
}

// detectWebView2Runtime 检测 exe 同目录下的 WebView2Runtime/ 文件夹
// WebviewBrowserPath 需要指向包含 msedgewebview2.exe 的目录
func detectWebView2Runtime() string {
	exePath, err := os.Executable()
	if err != nil {
		return ""
	}
	exeDir := filepath.Dir(exePath)
	wv2Dir := filepath.Join(exeDir, "WebView2Runtime")

	info, err := os.Stat(wv2Dir)
	if err != nil || !info.IsDir() {
		return ""
	}

	// Check if msedgewebview2.exe is directly in WebView2Runtime/
	if _, err := os.Stat(filepath.Join(wv2Dir, "msedgewebview2.exe")); err == nil {
		fmt.Printf("检测到本地 WebView2 运行时: %s\n", wv2Dir)
		return wv2Dir
	}

	// Check subdirectories (CAB extraction creates a version-named subfolder)
	entries, err := os.ReadDir(wv2Dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			subDir := filepath.Join(wv2Dir, entry.Name())
			if _, err := os.Stat(filepath.Join(subDir, "msedgewebview2.exe")); err == nil {
				fmt.Printf("检测到本地 WebView2 运行时: %s\n", subDir)
				return subDir
			}
		}
	}

	return ""
}

// 自动选择本地IP（非交互式，用于桌面模式）
func getLocalIPAuto() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					return ipnet.IP.String()
				}
			}
		}
	}

	return "127.0.0.1"
}
