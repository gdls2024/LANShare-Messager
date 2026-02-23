package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"golang.org/x/crypto/curve25519"
)

// enableTCPKeepAlive 在TCP连接上启用keepalive，用于快速检测死连接
func enableTCPKeepAlive(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(15 * time.Second)
	}
}

// ECDH密钥生成
func generateECDHKeyPair() (privateKey [32]byte, publicKey [32]byte, err error) {
	_, err = rand.Read(privateKey[:])
	if err != nil {
		return
	}
	var publicKeyBytes [32]byte
	curve25519.ScalarBaseMult(&publicKeyBytes, &privateKey)
	copy(publicKey[:], publicKeyBytes[:])
	return
}

// 派生共享密钥
func deriveSharedKey(privateKey [32]byte, remotePubKey [32]byte) [32]byte {
	var shared [32]byte
	var remotePub [32]byte
	copy(remotePub[:], remotePubKey[:])
	curve25519.ScalarMult(&shared, &privateKey, &remotePub)
	return shared
}

// 加密消息
func encryptMessage(key [32]byte, plaintext []byte) (ciphertext []byte, nonce []byte, err error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return
	}
	nonce = make([]byte, aesGCM.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return
	}
	ciphertext = aesGCM.Seal(nil, nonce, plaintext, nil)
	return
}

// 解密消息
func decryptMessage(key [32]byte, ciphertext []byte, nonce []byte) ([]byte, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aesGCM.Open(nil, nonce, ciphertext, nil)
}

// 连接到对等节点（带重试机制）
func (node *P2PNode) connectToPeer(ip string, port int, id, name string, webPort ...int) {
	// Skip invalid port (old CLI versions may broadcast port 0)
	if port <= 0 {
		return
	}

	node.PeersMutex.RLock()
	if ep, exists := node.Peers[id]; exists && ep.IsActive {
		node.PeersMutex.RUnlock()
		return
	}
	node.PeersMutex.RUnlock()

	if id == node.ID {
		return
	}

	address := fmt.Sprintf("%s:%d", ip, port)
	maxRetries := 3
	baseDelay := 1 * time.Second

	for attempt := 0; attempt < maxRetries; attempt++ {
		conn, err := net.Dial("tcp", address)
		if err != nil {
			if attempt < maxRetries-1 {
				delay := time.Duration(attempt+1) * baseDelay
				fmt.Printf("连接到 %s (%s) 失败，重试 %d/%d，等待 %v: %v\n",
					name, address, attempt+1, maxRetries, delay, err)
				Log.Debug("连接失败，重试中", "peer", name, "address", address, "attempt", attempt+1, "error", err)
				time.Sleep(delay)
				continue
			} else {
				fmt.Printf("连接到 %s (%s) 失败，已达到最大重试次数: %v\n", name, address, err)
				Log.Error("连接失败，已达到最大重试次数", "peer", name, "address", address, "error", err)
				return
			}
		}
		enableTCPKeepAlive(conn)

		peerWebPort := 0
		if len(webPort) > 0 {
			peerWebPort = webPort[0]
		}
		peer := &Peer{
			ID:       id,
			Name:     name,
			Address:  address,
			Conn:     conn,
			IsActive: true,
			LastSeen: time.Now(),
			IP:       ip,
			Port:     port,
			WebPort:  peerWebPort,
		}

		node.PeersMutex.Lock()
		if existingPeer, exists := node.Peers[id]; exists && existingPeer.IsActive {
			// 已有活跃连接（可能由incoming方向建立），放弃本次outbound连接
			node.PeersMutex.Unlock()
			Log.Debug("已有活跃连接，取消outbound连接", "peer", name)
			conn.Close()
			return
		}
		node.Peers[id] = peer
		node.PeersMutex.Unlock()

		fmt.Printf("成功连接到节点: %s (%s)\n", name, address)
		Log.Info("成功连接到节点", "peer", name, "address", address)
		node.emitUserOnline(name)

		// Use node-level persistent keys for handshake
		handshakeMsg := Message{
			Type:        "handshake",
			From:        node.ID,
			Content:     node.Name,
			Timestamp:   time.Now(),
			SenderPubKey: node.NodePublicKey[:],
			Data:        map[string]interface{}{"webPort": node.WebPort, "tcpPort": node.LocalPort},
		}
		node.sendMessageToPeer(peer, handshakeMsg)

		go node.handlePeerConnection(peer)
		return
	}
}

// 接受连接
func (node *P2PNode) acceptConnections() {
	defer func() {
		if r := recover(); r != nil {
			Log.Error("acceptConnections panic", "panic", fmt.Sprintf("%v", r))
		}
	}()
	for node.Running {
		conn, err := node.Listener.Accept()
		if err != nil {
			if node.Running {
				fmt.Printf("接受连接失败: %v\n", err)
				Log.Error("接受TCP连接失败", "error", err)
			}
			continue
		}

		enableTCPKeepAlive(conn)
		go node.handleIncomingConnection(conn)
	}
}

// 处理传入连接
func (node *P2PNode) handleIncomingConnection(conn net.Conn) {
	decoder := json.NewDecoder(conn)
	var handshakeMsg Message
	
	if err := decoder.Decode(&handshakeMsg); err != nil {
		conn.Close()
		return
	}

	if handshakeMsg.Type != "handshake" {
		conn.Close()
		return
	}

	// 提取远程公钥
	var remotePubKey [32]byte
	if len(handshakeMsg.SenderPubKey) == 32 {
		copy(remotePubKey[:], handshakeMsg.SenderPubKey)
	} else {
		conn.Close()
		return
	}

	peer := &Peer{
		Conn: conn,
	}

	// Use node-level persistent keys; derive shared key from node.private × peer.public
	peer.PublicKey = remotePubKey // Store remote peer's public key
	shared := deriveSharedKey(node.NodePrivateKey, remotePubKey)
	peer.SharedKey = shared[:]

	peer.ID = handshakeMsg.From
	peer.Name = handshakeMsg.Content
	peer.IsActive = true
	peer.LastSeen = time.Now()
	peer.ReconnectAttempts = 0

	// 解析IP（从连接的远程地址获取）
	if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		peer.IP = addr.IP.String()
	}

	// 从握手消息中提取WebPort和tcpPort（监听端口）
	if data, ok := handshakeMsg.Data.(map[string]interface{}); ok {
		if wp, ok := data["webPort"].(float64); ok {
			peer.WebPort = int(wp)
		}
		if tp, ok := data["tcpPort"].(float64); ok && int(tp) > 0 {
			peer.Port = int(tp)
		}
	}
	// 使用对端的监听端口构建重连地址（而非连接的临时端口）
	if peer.Port > 0 {
		peer.Address = fmt.Sprintf("%s:%d", peer.IP, peer.Port)
	} else {
		// 兼容旧版本：没有tcpPort字段时使用连接地址
		peer.Address = conn.RemoteAddr().String()
	}

	node.PeersMutex.Lock()
	oldPeer, alreadyKnown := node.Peers[peer.ID]
	if alreadyKnown && oldPeer.IsActive {
		if node.ID < peer.ID {
			// 本机是发起方（小ID），不应该收到对方的incoming → 拒绝
			node.PeersMutex.Unlock()
			Log.Debug("拒绝重复连接：本机应为发起方", "peer", peer.Name)
			conn.Close()
			return
		}
		// 本机是被动方（大ID），对方是发起方 → 信任对方的重连判断，替换旧连接
		Log.Info("替换旧连接：接受发起方的新连接", "peer", peer.Name)
	}
	if alreadyKnown && oldPeer.Conn != nil {
		oldPeer.Conn.Close()
	}
	node.Peers[peer.ID] = peer
	node.PeersMutex.Unlock()

	fmt.Printf("接受来自节点的连接: %s (%s)\n", peer.Name, peer.Address)
	Log.Info("接受来自节点的连接", "peer", peer.Name, "address", peer.Address)
	if !alreadyKnown {
		node.emitUserOnline(peer.Name)
	}

	// 发送握手响应 (with node-level public key)
	responseMsg := Message{
		Type:        "handshake_response",
		From:        node.ID,
		Content:     node.Name,
		Timestamp:   time.Now(),
		SenderPubKey: node.NodePublicKey[:],
		Data:        map[string]interface{}{"webPort": node.WebPort, "tcpPort": node.LocalPort},
	}
	node.sendMessageToPeer(peer, responseMsg)

	go node.handlePeerConnection(peer)
}

// 处理对等节点连接（带重连机制）
func (node *P2PNode) handlePeerConnection(peer *Peer) {
	defer func() {
		if r := recover(); r != nil {
			Log.Error("handlePeerConnection panic", "peer", peer.Name, "panic", fmt.Sprintf("%v", r))
		}
		peer.Conn.Close()
	}()

	for node.Running {
		decoder := json.NewDecoder(peer.Conn)

		// 读取消息循环
		for node.Running {
			var msg Message
			if err := decoder.Decode(&msg); err != nil {
				if err != io.EOF && err.Error() != "use of closed network connection" {
					fmt.Printf("从节点 %s 读取消息失败: %v\n", peer.Name, err)
					Log.Error("读取消息失败", "peer", peer.Name, "error", err)
				}
				break
			}

			peer.LastSeen = time.Now()
			peer.ReconnectAttempts = 0 // 重置重连计数
			// Safe send: use StopCh to avoid send-on-closed-channel panic
			select {
			case node.MessageChan <- msg:
			case <-node.StopCh:
				return
			}
		}

		// 连接断开
		if !node.Running {
			break
		}

		// Check if we've been replaced by a newer connection
		node.PeersMutex.RLock()
		currentPeer, stillInMap := node.Peers[peer.ID]
		node.PeersMutex.RUnlock()
		if !stillInMap || currentPeer != peer {
			Log.Info("连接已被新连接替换，退出旧协程", "peer", peer.Name)
			return
		}

		peer.IsActive = false
		node.emitUserOffline(peer.Name)

		// If the disconnecting peer was the update source, clear the update banner
		node.PeersMutex.RLock()
		if node.AvailableUpdate != nil && node.AvailableUpdate.IP == peer.IP {
			node.PeersMutex.RUnlock()
			node.PeersMutex.Lock()
			node.AvailableUpdate = nil
			node.PeersMutex.Unlock()
			node.emitUpdateCleared()
			Log.Info("更新源下线，清除更新提示", "peer", peer.Name)
		} else {
			node.PeersMutex.RUnlock()
		}

		fmt.Printf("节点 %s 断开连接，等待发现协议重连\n", peer.Name)
		Log.Info("节点断开连接", "peer", peer.Name, "address", peer.Address)
		break // 不主动重连，依赖发现协议（每30秒广播）自动重建连接
	}

	// 清理
	node.PeersMutex.Lock()
	// 只有当前peer仍在map中时才删除（避免删除已被替换的新连接）
	if cp, ok := node.Peers[peer.ID]; ok && cp == peer {
		delete(node.Peers, peer.ID)
	}
	node.PeersMutex.Unlock()
	fmt.Printf("节点 %s 连接协程退出\n", peer.Name)
	Log.Info("节点连接协程退出", "peer", peer.Name, "id", peer.ID)
}

// 处理消息
func (node *P2PNode) handleMessages() {
	defer func() {
		if r := recover(); r != nil {
			Log.Error("handleMessages panic", "panic", fmt.Sprintf("%v", r))
		}
	}()
	cleanupTicker := time.NewTicker(5 * time.Minute)
	defer cleanupTicker.Stop()

	for {
		select {
		case msg, ok := <-node.MessageChan:
			if !ok {
				return // 通道已关闭
			}
			Log.Debug("收到消息", "from", msg.From, "type", msg.Type)

			switch msg.Type {
		case "chat":
			// 解密聊天消息
			var content string
			if msg.Encrypted && len(msg.Nonce) > 0 && len(msg.Ciphertext) > 0 {
				// 查找发送方 peer 以获取共享密钥
				node.PeersMutex.RLock()
				senderPeer, exists := node.Peers[msg.From]
				node.PeersMutex.RUnlock()
				if exists && len(senderPeer.SharedKey) > 0 {
					plaintext, err := decryptMessage([32]byte(senderPeer.SharedKey), msg.Ciphertext, msg.Nonce)
					if err == nil {
						content = string(plaintext)
					} else {
						fmt.Printf("解密失败: %v\n", err)
						Log.Error("消息解密失败", "from", msg.From, "error", err)
						content = "[解密失败]"
					}
				} else {
					content = "[无密钥]"
				}
			} else {
				content = msg.Content
			}

			senderPeer, exists := node.Peers[msg.From]
			if !exists {
				continue
			}
			senderName := node.getPeerName(msg.From)
			if msg.To == "" || msg.To == "all" {
				// 公聊消息
				if node.isBlocked(senderPeer.Address) {
					continue
				}
				fileURL := node.processReceivedFile(msg)
				node.addChatMessageWithType(senderName, "all", content, false, false,
					msg.MessageType, msg.MessageID, msg.ReplyToID, msg.ReplyToContent, msg.ReplyToSender,
					msg.FileName, msg.FileSize, msg.FileType, fileURL, msg.FileID)
			} else if msg.To == node.ID {
				// 私聊消息
				if node.isBlocked(senderPeer.Address) {
					continue
				}
				fileURL := node.processReceivedFile(msg)
				node.addChatMessageWithType(senderName, node.Name, content, false, true,
					msg.MessageType, msg.MessageID, msg.ReplyToID, msg.ReplyToContent, msg.ReplyToSender,
					msg.FileName, msg.FileSize, msg.FileType, fileURL, msg.FileID)
			}
		case "file_complete":
			// 文件传输完成确认（接收方→发送方）
			node.handleFileComplete(msg.Content)
		case "file_cancel":
			// 文件传输取消
			node.handleFileTransferCancel(msg.Content)
		case "handshake":
			// 握手消息已在连接处理中处理
		case "handshake_response":
			// 握手响应 - 使用节点持久密钥派生共享密钥
			node.PeersMutex.Lock()
			var peer *Peer
			var exists bool
			peer, exists = node.Peers[msg.From]
			if exists && len(msg.SenderPubKey) == 32 {
				var remotePub [32]byte
				copy(remotePub[:], msg.SenderPubKey)
				peer.PublicKey = remotePub // Store remote peer's public key
				shared := deriveSharedKey(node.NodePrivateKey, remotePub)
				peer.SharedKey = shared[:]
				// 从握手响应中提取WebPort和tcpPort
				if data, ok := msg.Data.(map[string]interface{}); ok {
					if wp, ok := data["webPort"].(float64); ok {
						peer.WebPort = int(wp)
					}
					if tp, ok := data["tcpPort"].(float64); ok && int(tp) > 0 {
						peer.Port = int(tp)
						peer.Address = fmt.Sprintf("%s:%d", peer.IP, peer.Port)
					}
				}
				fmt.Printf("与 %s 建立加密连接\n", peer.Name)
				Log.Info("建立加密连接", "peer", peer.Name)
			}
			node.PeersMutex.Unlock()
		case "file_request":
			// 文件传输请求
			if data, ok := msg.Data.(map[string]interface{}); ok {
				jsonData, _ := json.Marshal(data)
				var request FileTransferRequest
				if err := json.Unmarshal(jsonData, &request); err == nil {
					node.handleFileTransferRequest(request)
				}
			}
		case "file_response":
			// 文件传输响应
			if data, ok := msg.Data.(map[string]interface{}); ok {
				jsonData, _ := json.Marshal(data)
				var response FileTransferResponse
				if err := json.Unmarshal(jsonData, &response); err == nil {
					node.handleFileTransferResponse(response)
				}
			}
		case "file_chunk":
			// 文件数据块
			if data, ok := msg.Data.(map[string]interface{}); ok {
				jsonData, _ := json.Marshal(data)
				var chunk FileChunk
				if err := json.Unmarshal(jsonData, &chunk); err == nil {
					node.handleFileChunk(chunk)
				}
			}
		case "update_name":
			// 用户名更新
			node.PeersMutex.Lock()
			var oldName string
			if peer, exists := node.Peers[msg.From]; exists {
				oldName = peer.Name
				peer.Name = msg.Content
				fmt.Printf("用户 %s 已更名为 %s\n", oldName, peer.Name)
			}
			node.PeersMutex.Unlock()

			// Merge old name's messages into new name
			if oldName != "" && oldName != msg.Content {
				node.renamePeerInMessages(oldName, msg.Content)
			}
			}
		case <-cleanupTicker.C:
			node.cleanupMemory()
		}
	}
}

// 获取对等节点名称
func (node *P2PNode) getPeerName(peerID string) string {
	node.PeersMutex.RLock()
	defer node.PeersMutex.RUnlock()
	
	if peer, exists := node.Peers[peerID]; exists {
		return peer.Name
	}
	return peerID
}

// 发送消息到对等节点
func (node *P2PNode) sendMessageToPeer(peer *Peer, msg Message) error {
	if len(peer.SharedKey) > 0 && msg.Type == "chat" {
		// 加密聊天消息
		plaintext := []byte(msg.Content)
		ciphertext, nonce, err := encryptMessage([32]byte(peer.SharedKey), plaintext)
		if err != nil {
			return err
		}
		msg.Encrypted = true
		msg.Nonce = nonce
		msg.Ciphertext = ciphertext
		msg.Content = "" // 清空明文
	}

	// Serialize writes to prevent concurrent JSON encoder interleaving
	peer.WriteMutex.Lock()
	defer peer.WriteMutex.Unlock()
	encoder := json.NewEncoder(peer.Conn)
	return encoder.Encode(msg)
}

// 广播消息到所有对等节点
func (node *P2PNode) broadcastMessage(msg Message) {
	node.PeersMutex.RLock()
	defer node.PeersMutex.RUnlock()

	for _, peer := range node.Peers {
		if peer.IsActive {
			go func(p *Peer) {
				if err := node.sendMessageToPeer(p, msg); err != nil {
					Log.Error("发送消息失败", "peer", p.Name, "type", msg.Type, "error", err)
				}
			}(peer)
		}
	}
}

// 获取本地IP地址
func getLocalIP() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}

	var availableIPs []string
	var interfaceNames []string

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
					ip := ipnet.IP.String()
					availableIPs = append(availableIPs, ip)
					interfaceNames = append(interfaceNames, iface.Name)
				}
			}
		}
	}

	if len(availableIPs) == 0 {
		return "127.0.0.1"
	}

	if len(availableIPs) == 1 {
		fmt.Printf("使用网络接口: %s (%s)\n", interfaceNames[0], availableIPs[0])
		return availableIPs[0]
	}

	// 多个网卡时让用户选择
	fmt.Println("检测到的网络接口:")
	for i, ip := range availableIPs {
		fmt.Printf("  %d. %s: %s\n", i+1, interfaceNames[i], ip)
	}

	var choice int
	for {
		fmt.Print("请选择网络接口 (1-" + strconv.Itoa(len(availableIPs)) + "): ")
		_, err := fmt.Scanln(&choice)
		if err == nil && choice >= 1 && choice <= len(availableIPs) {
			break
		}
		fmt.Println("无效选择，请重试。")
	}

	selectedIP := availableIPs[choice-1]
	selectedName := interfaceNames[choice-1]
	fmt.Printf("使用网络接口: %s (%s)\n", selectedName, selectedIP)
	return selectedIP
}

// 处理接收到的文件数据
func (node *P2PNode) processReceivedFile(msg Message) string {
	if msg.MessageType == MessageTypeImage && msg.FileData != "" {
		// 对于图片消息，解码base64数据并保存到本地
		imageData, err := base64.StdEncoding.DecodeString(msg.FileData)
		if err != nil {
			fmt.Printf("解码图片数据失败: %v\n", err)
			Log.Error("解码图片数据失败", "fileName", msg.FileName, "error", err)
			return ""
		}

		// 创建images目录
		imageDir := DataPath("images")
		if err := os.MkdirAll(imageDir, 0755); err != nil {
			fmt.Printf("创建图片目录失败: %v\n", err)
			Log.Error("创建图片目录失败", "error", err)
			return ""
		}

		// 生成唯一文件名
		ext := filepath.Ext(msg.FileName)
		if ext == "" {
			ext = ".jpg" // 默认扩展名
		}
		imageFileName := fmt.Sprintf("%s_%d%s", generateMessageID(), time.Now().Unix(), ext)
		imagePath := filepath.Join(imageDir, imageFileName)

		// 保存图片文件
		if err := os.WriteFile(imagePath, imageData, 0644); err != nil {
			fmt.Printf("保存接收到的图片失败: %v\n", err)
			Log.Error("保存接收到的图片失败", "path", imagePath, "error", err)
			return ""
		}

		return fmt.Sprintf("/images/%s", imageFileName)
	}

	// 对于其他类型的文件，返回空字符串（暂时不支持）
	return ""
}
