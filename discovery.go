package main

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// 根据本地IP计算子网定向广播地址
func getSubnetBroadcastAddr(localIP string) (string, error) {
	targetIP := net.ParseIP(localIP)
	if targetIP == nil {
		return "255.255.255.255", fmt.Errorf("无法解析IP: %s", localIP)
	}

	interfaces, err := net.Interfaces()
	if err != nil {
		return "255.255.255.255", fmt.Errorf("获取网络接口失败: %v", err)
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
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ipnet.IP.Equal(targetIP) {
				ip := ipnet.IP.To4()
				mask := ipnet.Mask
				if ip == nil || len(mask) != 4 {
					continue
				}
				broadcast := make(net.IP, 4)
				for i := 0; i < 4; i++ {
					broadcast[i] = ip[i] | ^mask[i]
				}
				return broadcast.String(), nil
			}
		}
	}

	return "255.255.255.255", fmt.Errorf("未找到匹配 %s 的网络接口", localIP)
}

// 启动服务发现
func (node *P2PNode) startDiscovery() {
	// 计算子网广播地址
	broadcastAddr, err := getSubnetBroadcastAddr(node.LocalIP)
	if err != nil {
		Log.Warn("计算广播地址失败，使用 255.255.255.255", "error", err)
	}
	node.BroadcastAddr = broadcastAddr
	Log.Info("使用广播地址", "addr", node.BroadcastAddr)

	go node.listenBroadcast()
	time.Sleep(500 * time.Millisecond)
	// Send multiple rapid announces at startup for fast peer discovery
	// (UDP is unreliable, single packet may be lost)
	for i := 0; i < 3; i++ {
		node.sendDiscoveryBroadcast("announce")
		time.Sleep(1 * time.Second)
	}
}

// 监听UDP广播
func (node *P2PNode) listenBroadcast() {
	defer func() {
		if r := recover(); r != nil {
			Log.Error("listenBroadcast panic", "panic", fmt.Sprintf("%v", r))
		}
	}()
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", node.DiscoveryPort))
	if err != nil {
		fmt.Printf("解析UDP地址失败: %v\n", err)
		Log.Error("解析UDP地址失败", "port", node.DiscoveryPort, "error", err)
		return
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Printf("监听UDP失败: %v\n", err)
		Log.Error("监听UDP失败", "port", node.DiscoveryPort, "error", err)
		return
	}
	defer conn.Close()

	buffer := make([]byte, 1024)
	for node.Running {
		n, remoteAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			continue
		}

		var discoveryMsg DiscoveryMessage
		if err := json.Unmarshal(buffer[:n], &discoveryMsg); err != nil {
			continue
		}

		if discoveryMsg.ID == node.ID {
			continue
		}

		node.handleDiscoveryMessage(discoveryMsg, remoteAddr)
	}
}

// 处理服务发现消息
func (node *P2PNode) handleDiscoveryMessage(msg DiscoveryMessage, remoteAddr *net.UDPAddr) {
	// 检查是否是已知且活跃的节点
	node.PeersMutex.RLock()
	existingPeer, exists := node.Peers[msg.ID]
	isActive := exists && existingPeer.IsActive
	node.PeersMutex.RUnlock()
	if isActive {
		return // 已有活跃连接，忽略
	}

	switch msg.Type {
	case "announce":
		if exists {
			fmt.Printf("[发现] 重新发现已断开的节点: %s (%s:%d)\n", msg.Name, msg.IP, msg.Port)
			Log.Info("重新发现已断开的节点", "name", msg.Name, "ip", msg.IP, "port", msg.Port)
			node.PeersMutex.Lock()
			delete(node.Peers, msg.ID)
			node.PeersMutex.Unlock()
		} else {
			fmt.Printf("[发现] 发现新节点: %s (%s:%d)\n", msg.Name, msg.IP, msg.Port)
			Log.Info("发现新节点", "name", msg.Name, "ip", msg.IP, "port", msg.Port)
		}
		// 确定性连接：只有ID较小的节点主动发起TCP连接，避免双向同时连接导致的重连循环
		if node.ID < msg.ID {
			go node.connectToPeer(msg.IP, msg.Port, msg.ID, msg.Name, msg.WebPort)
		}
		// 始终发送响应，让对方知道我们的存在
		node.sendDiscoveryResponse(remoteAddr.IP.String())

	case "response":
		if exists {
			fmt.Printf("[发现] 收到已断开节点 %s 的响应，重新连接...\n", msg.Name)
			Log.Info("收到已断开节点的响应", "name", msg.Name)
			node.PeersMutex.Lock()
			delete(node.Peers, msg.ID)
			node.PeersMutex.Unlock()
		} else {
			Log.Info("收到发现响应", "name", msg.Name, "ip", msg.IP)
		}
		// 确定性连接：只有ID较小的节点主动发起TCP连接
		if node.ID < msg.ID {
			fmt.Printf("[发现] 收到来自 %s 的响应，发起连接...\n", msg.Name)
			go node.connectToPeer(msg.IP, msg.Port, msg.ID, msg.Name, msg.WebPort)
		} else {
			fmt.Printf("[发现] 收到来自 %s 的响应，等待对方连接\n", msg.Name)
		}
	}
}

// 发送服务发现广播
func (node *P2PNode) sendDiscoveryBroadcast(msgType string) {
	msg := DiscoveryMessage{
		Type:    msgType,
		ID:      node.ID,
		Name:    node.Name,
		IP:      node.LocalIP,
		Port:    node.LocalPort,
		WebPort: node.WebPort,
		Version: AppVersion,
		PubKey:  node.NodePublicKey[:],
	}

	data, err := json.Marshal(msg)
	if err != nil {
		Log.Error("序列化发现消息失败", "error", err)
		return
	}

	// 使用子网定向广播地址
	broadcastIP := node.BroadcastAddr
	if broadcastIP == "" {
		broadcastIP = "255.255.255.255"
	}

	broadcastAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", broadcastIP, node.DiscoveryPort))
	if err != nil {
		Log.Error("解析广播地址失败", "addr", broadcastIP, "error", err)
		return
	}

	// 绑定到选定的网卡
	localAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:0", node.LocalIP))
	if err != nil {
		Log.Error("解析本地地址失败", "localIP", node.LocalIP, "error", err)
		return
	}

	conn, err := net.DialUDP("udp", localAddr, broadcastAddr)
	if err != nil {
		Log.Error("创建UDP广播连接失败", "error", err)
		return
	}
	defer conn.Close()

	_, err = conn.Write(data)
	if err != nil {
		Log.Error("发送广播失败", "error", err)
	} else {
		Log.Info("发送发现广播", "type", msgType, "localIP", node.LocalIP, "broadcastAddr", broadcastIP)
	}
}

// 发送服务发现响应
func (node *P2PNode) sendDiscoveryResponse(targetIP string) {
	msg := DiscoveryMessage{
		Type:    "response",
		ID:      node.ID,
		Name:    node.Name,
		IP:      node.LocalIP,
		Port:    node.LocalPort,
		WebPort: node.WebPort,
		Version: AppVersion,
		PubKey:  node.NodePublicKey[:],
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", targetIP, node.DiscoveryPort))
	if err != nil {
		return
	}

	// 绑定到选定的网卡
	localAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:0", node.LocalIP))
	if err != nil {
		return
	}

	conn, err := net.DialUDP("udp", localAddr, addr)
	if err != nil {
		return
	}
	defer conn.Close()

	conn.Write(data)
}

// 定期广播
func (node *P2PNode) periodicBroadcast() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-node.StopCh:
			return
		case <-ticker.C:
			if node.Running {
				node.sendDiscoveryBroadcast("announce")
			}
		}
	}
}
