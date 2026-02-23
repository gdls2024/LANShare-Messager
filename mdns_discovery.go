package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
)

// 启动mDNS服务发现
func (node *P2PNode) startMDNSDiscovery() {
	// 注册mDNS服务
	info := []string{
		fmt.Sprintf("id=%s", node.ID),
		fmt.Sprintf("name=%s", node.Name),
	}

	service, err := mdns.NewMDNSService(
		node.ID,          // instance name
		"_lanshare._tcp", // service type
		"",               // domain (default: local)
		"",               // host (default: hostname)
		node.LocalPort,   // port
		nil,              // IPs (nil = all interfaces)
		info,             // TXT records
	)
	if err != nil {
		fmt.Printf("[mDNS] 创建mDNS服务失败: %v\n", err)
		return
	}

	server, err := mdns.NewServer(&mdns.Config{Zone: service})
	if err != nil {
		fmt.Printf("[mDNS] 启动mDNS服务器失败: %v\n", err)
		return
	}
	node.MdnsServer = server
	fmt.Println("[mDNS] mDNS服务已注册: _lanshare._tcp")

	// 立即执行一次查询
	node.queryMDNS()

	// 定期查询
	go node.periodicMDNSQuery()
}

// 定期查询mDNS服务
func (node *P2PNode) periodicMDNSQuery() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if !node.Running {
				return
			}
			node.queryMDNS()
		}
	}
}

// 查询mDNS服务
func (node *P2PNode) queryMDNS() {
	entriesCh := make(chan *mdns.ServiceEntry, 8)

	go func() {
		for entry := range entriesCh {
			node.handleMDNSEntry(entry)
		}
	}()

	params := &mdns.QueryParam{
		Service: "_lanshare._tcp",
		Timeout: 3 * time.Second,
		Entries: entriesCh,
	}

	if err := mdns.Query(params); err != nil {
		fmt.Printf("[mDNS] 查询失败: %v\n", err)
	}
	close(entriesCh)
}

// 处理mDNS发现的服务条目
func (node *P2PNode) handleMDNSEntry(entry *mdns.ServiceEntry) {
	// 从TXT记录中提取ID和名称
	var peerID, peerName string
	for _, txt := range entry.InfoFields {
		if strings.HasPrefix(txt, "id=") {
			peerID = strings.TrimPrefix(txt, "id=")
		} else if strings.HasPrefix(txt, "name=") {
			peerName = strings.TrimPrefix(txt, "name=")
		}
	}

	// 忽略自己
	if peerID == "" || peerID == node.ID {
		return
	}

	if peerName == "" {
		peerName = "unknown"
	}

	// 检查是否已知且活跃
	node.PeersMutex.RLock()
	existingPeer, exists := node.Peers[peerID]
	isActive := exists && existingPeer.IsActive
	node.PeersMutex.RUnlock()

	if isActive {
		return
	}

	// 获取IP地址
	ip := ""
	if entry.AddrV4 != nil {
		ip = entry.AddrV4.String()
	} else if entry.AddrV6 != nil {
		ip = entry.AddrV6.String()
	}

	if ip == "" {
		return
	}

	port := entry.Port

	if exists {
		fmt.Printf("[mDNS] 重新发现已断开的节点: %s (%s:%d)\n", peerName, ip, port)
		node.PeersMutex.Lock()
		delete(node.Peers, peerID)
		node.PeersMutex.Unlock()
	} else {
		fmt.Printf("[mDNS] 发现新节点: %s (%s:%d)\n", peerName, ip, port)
	}

	go node.connectToPeer(ip, port, peerID, peerName)
}

// 停止mDNS服务
func (node *P2PNode) stopMDNS() {
	if node.MdnsServer != nil {
		if err := node.MdnsServer.Shutdown(); err != nil {
			fmt.Printf("[mDNS] 关闭mDNS服务器失败: %v\n", err)
		} else {
			fmt.Println("[mDNS] mDNS服务已停止")
		}
		node.MdnsServer = nil
	}
}
