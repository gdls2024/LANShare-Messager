package main

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// 启动服务发现
func (node *P2PNode) startDiscovery() {
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
		fmt.Printf("发现新节点: %s (%s:%d)\n", msg.Name, msg.IP, msg.Port)
		Log.Info("发现新节点", "name", msg.Name, "ip", msg.IP, "port", msg.Port)
		// 确定性连接：只有ID较小的节点主动发起TCP连接，避免双向同时连接导致的重连循环
		if node.ID < msg.ID {
			go node.connectToPeer(msg.IP, msg.Port, msg.ID, msg.Name, msg.WebPort)
		}
		// 始终发送响应，让对方知道我们的存在
		node.sendDiscoveryResponse(remoteAddr.IP.String())

	case "response":
		Log.Info("收到发现响应", "name", msg.Name, "ip", msg.IP)
		// 确定性连接：只有ID较小的节点主动发起TCP连接
		if node.ID < msg.ID {
			fmt.Printf("收到来自 %s 的响应，发起连接...\n", msg.Name)
			go node.connectToPeer(msg.IP, msg.Port, msg.ID, msg.Name, msg.WebPort)
		} else {
			fmt.Printf("收到来自 %s 的响应，等待对方连接\n", msg.Name)
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

	broadcastAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("255.255.255.255:%d", node.DiscoveryPort))
	if err != nil {
		Log.Error("解析广播地址失败", "error", err)
		return
	}

	conn, err := net.DialUDP("udp", nil, broadcastAddr)
	if err != nil {
		Log.Error("创建UDP广播连接失败", "error", err)
		return
	}
	defer conn.Close()

	_, err = conn.Write(data)
	if err != nil {
		Log.Error("发送广播失败", "error", err)
	} else {
		Log.Info("发送发现广播", "type", msgType, "localIP", node.LocalIP)
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

	conn, err := net.DialUDP("udp", nil, addr)
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
