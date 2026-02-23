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
				// 找到了匹配的接口，计算广播地址
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
		fmt.Printf("[发现] 计算广播地址失败，使用 255.255.255.255: %v\n", err)
	}
	node.BroadcastAddr = broadcastAddr
	fmt.Printf("[发现] 使用广播地址: %s\n", node.BroadcastAddr)

	go node.listenBroadcast()
	time.Sleep(1 * time.Second)
	node.sendDiscoveryBroadcast("announce")
}

// 监听UDP广播
func (node *P2PNode) listenBroadcast() {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", node.DiscoveryPort))
	if err != nil {
		fmt.Printf("[发现] 解析UDP地址失败: %v\n", err)
		return
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Printf("[发现] 监听UDP失败: %v\n", err)
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
		return // 已知且活跃的节点，忽略
	}

	switch msg.Type {
	case "announce":
		if exists {
			fmt.Printf("[发现] 重新发现已断开的节点: %s (%s:%d)\n", msg.Name, msg.IP, msg.Port)
			// 清除旧的 peer 记录
			node.PeersMutex.Lock()
			delete(node.Peers, msg.ID)
			node.PeersMutex.Unlock()
		} else {
			fmt.Printf("[发现] 发现新节点: %s (%s:%d)\n", msg.Name, msg.IP, msg.Port)
		}
		go node.connectToPeer(msg.IP, msg.Port, msg.ID, msg.Name)
		node.sendDiscoveryResponse(remoteAddr.IP.String())

	case "response":
		if exists {
			fmt.Printf("[发现] 收到已断开节点 %s 的响应，重新连接...\n", msg.Name)
			node.PeersMutex.Lock()
			delete(node.Peers, msg.ID)
			node.PeersMutex.Unlock()
		} else {
			fmt.Printf("[发现] 收到来自 %s 的响应，尝试连接...\n", msg.Name)
		}
		go node.connectToPeer(msg.IP, msg.Port, msg.ID, msg.Name)
	}
}

// 发送服务发现广播
func (node *P2PNode) sendDiscoveryBroadcast(msgType string) {
	msg := DiscoveryMessage{
		Type: msgType,
		ID:   node.ID,
		Name: node.Name,
		IP:   node.LocalIP,
		Port: node.LocalPort,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		fmt.Printf("[发现] 序列化广播消息失败: %v\n", err)
		return
	}

	// 使用子网定向广播地址
	broadcastIP := node.BroadcastAddr
	if broadcastIP == "" {
		broadcastIP = "255.255.255.255"
	}

	broadcastAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", broadcastIP, node.DiscoveryPort))
	if err != nil {
		fmt.Printf("[发现] 解析广播地址 %s 失败: %v\n", broadcastIP, err)
		return
	}

	// 绑定到选定的网卡
	localAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:0", node.LocalIP))
	if err != nil {
		fmt.Printf("[发现] 解析本地地址 %s 失败: %v\n", node.LocalIP, err)
		return
	}

	conn, err := net.DialUDP("udp", localAddr, broadcastAddr)
	if err != nil {
		fmt.Printf("[发现] 发送广播失败 (目标 %s): %v\n", broadcastIP, err)
		return
	}
	defer conn.Close()

	if _, err := conn.Write(data); err != nil {
		fmt.Printf("[发现] 写入广播数据失败: %v\n", err)
	}
}

// 发送服务发现响应
func (node *P2PNode) sendDiscoveryResponse(targetIP string) {
	msg := DiscoveryMessage{
		Type: "response",
		ID:   node.ID,
		Name: node.Name,
		IP:   node.LocalIP,
		Port: node.LocalPort,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		fmt.Printf("[发现] 序列化响应消息失败: %v\n", err)
		return
	}

	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", targetIP, node.DiscoveryPort))
	if err != nil {
		fmt.Printf("[发现] 解析目标地址 %s 失败: %v\n", targetIP, err)
		return
	}

	// 绑定到选定的网卡
	localAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:0", node.LocalIP))
	if err != nil {
		fmt.Printf("[发现] 解析本地地址 %s 失败: %v\n", node.LocalIP, err)
		return
	}

	conn, err := net.DialUDP("udp", localAddr, addr)
	if err != nil {
		fmt.Printf("[发现] 发送响应到 %s 失败: %v\n", targetIP, err)
		return
	}
	defer conn.Close()

	if _, err := conn.Write(data); err != nil {
		fmt.Printf("[发现] 写入响应数据失败: %v\n", err)
	}
}

// 定期广播
func (node *P2PNode) periodicBroadcast() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if node.Running {
				node.sendDiscoveryBroadcast("announce")
			}
		}
	}
}
