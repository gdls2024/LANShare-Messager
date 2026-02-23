package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed all:web emoji_gifs.json
var webFS embed.FS

// 创建HTTP请求处理器（供Web服务器和Wails AssetHandler共用）
func (node *P2PNode) createHTTPHandler() http.Handler {
	// 创建新的路由器
	mux := http.NewServeMux()

	// 创建一个子文件系统，根目录为 'web'
	subFS, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("无法创建嵌入式文件子系统: %v", err)
	}

	// 从嵌入式文件系统读取HTML模板
	tmpl, err := template.ParseFS(subFS, "index.html")
	if err != nil {
		log.Fatalf("从嵌入式文件系统读取HTML模板失败: %v", err)
	}

	// 主页处理器
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if err := tmpl.Execute(w, node); err != nil {
			log.Printf("执行模板失败: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	})

	// 静态文件服务器
	// 请求 /static/style.css -> 在 subFS 中查找 style.css
	staticServer := http.FileServer(http.FS(subFS))
	mux.Handle("/static/", http.StripPrefix("/static/", staticServer))

	// GIF 表情文件服务器
	// 请求 /emoji-gifs/heart.gif -> 从 ~/.lanshare/assets/emoji-gifs/heart.gif 服务
	emojiGifServer := http.FileServer(http.Dir(DataPath("assets", "emoji-gifs")))
	mux.Handle("/emoji-gifs/", http.StripPrefix("/emoji-gifs/", emojiGifServer))

	// 图片文件服务器
	// 请求 /images/filename.jpg -> 从 ~/.lanshare/images/ 服务
	imageServer := http.FileServer(http.Dir(DataPath("images")))
	mux.Handle("/images/", http.StripPrefix("/images/", imageServer))

	// 获取 GIF 表情列表处理器
	mux.HandleFunc("/emoji-gifs-list", func(w http.ResponseWriter, r *http.Request) {
		// 读取嵌入的 emoji_gifs.json 文件
		data, err := webFS.Open("emoji_gifs.json")
		if err != nil {
			// 如果文件不存在，返回空列表
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]interface{}{})
			return
		}
		defer data.Close()

		// 直接转发文件内容
		w.Header().Set("Content-Type", "application/json")
		io.Copy(w, data)
	})

	// 获取所有历史聊天伙伴（用于聊天列表显示离线用户）
	mux.HandleFunc("/chatpartners", func(w http.ResponseWriter, r *http.Request) {
		partners := []string{}
		if node.DB != nil {
			rows, err := node.DB.Query(`
				SELECT DISTINCT CASE
					WHEN is_own = TRUE THEN recipient
					ELSE sender
				END AS partner
				FROM messages
				WHERE is_private = TRUE AND partner != 'all'
			`)
			if err == nil {
				defer rows.Close()
				for rows.Next() {
					var p string
					if rows.Scan(&p) == nil && p != "" && p != node.Name {
						partners = append(partners, p)
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"partners": partners})
	})

	// 加载历史消息处理器 (for web frontend)
	mux.HandleFunc("/loadhistory", func(w http.ResponseWriter, r *http.Request) {
		if node.DB == nil {
			http.Error(w, "Database not available", http.StatusInternalServerError)
			return
		}

		chatId := r.URL.Query().Get("chatId")
		if chatId == "" {
			chatId = "all"
		}
		limitStr := r.URL.Query().Get("limit")
		limit := 20
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
		offsetStr := r.URL.Query().Get("offset")
		offset := 0
		if o, err := strconv.Atoi(offsetStr); err == nil {
			offset = o
		}

		var rows *sql.Rows
		var err error
		var query string
		var args []interface{}

		if chatId == "all" {
			query = `
				SELECT sender, recipient, content, nonce, is_private, is_own, timestamp,
					   message_type, message_id, reply_to_id, reply_to_content, reply_to_sender,
					   file_name, file_size, file_type, file_url, file_data, COALESCE(file_id, '')
				FROM messages
				WHERE recipient = 'all' AND is_private = FALSE
				ORDER BY timestamp ASC
				LIMIT ? OFFSET ?
			`
			args = []interface{}{limit, offset}
		} else {
			query = `
				SELECT sender, recipient, content, nonce, is_private, is_own, timestamp,
					   message_type, message_id, reply_to_id, reply_to_content, reply_to_sender,
					   file_name, file_size, file_type, file_url, file_data, COALESCE(file_id, '')
				FROM messages
				WHERE is_private = TRUE AND (
					(sender = ? AND recipient = ?) OR
					(sender = ? AND recipient = ?)
				)
				ORDER BY timestamp ASC
				LIMIT ? OFFSET ?
			`
			args = []interface{}{node.Name, chatId, chatId, node.Name, limit, offset}
		}

		rows, err = node.DB.Query(query, args...)
		if err != nil {
			http.Error(w, "Query failed", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		type HistoryMsg struct {
			ChatMessage
			SenderName string `json:"senderName"`
		}

		var history []HistoryMsg
		for rows.Next() {
			var sender, recipient string
			var content, nonce []byte
			var isPrivate, isOwn bool
			var tsStr string
			var messageType, messageID, replyToID, replyToContent, replyToSender, fileName, fileType, fileURL, fileData, fileID string
			var fileSize int64

			err = rows.Scan(&sender, &recipient, &content, &nonce, &isPrivate, &isOwn, &tsStr,
				&messageType, &messageID, &replyToID, &replyToContent, &replyToSender,
				&fileName, &fileSize, &fileType, &fileURL, &fileData, &fileID)
			if err != nil {
				continue
			}

			plaintext, err := decryptMessage(node.LocalDBKey, content, nonce)
			if err != nil {
				fmt.Printf("解密历史消息失败: %v\n", err)
				Log.Error("解密历史消息失败", "error", err)
				continue
			}

			ts, err := time.Parse("2006-01-02 15:04:05", tsStr)
			if err != nil {
				ts = time.Now()
			}

			senderName := sender
			if sender == node.Name {
				senderName = "我"
			}

			hm := HistoryMsg{
				ChatMessage: ChatMessage{
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
				},
				SenderName: senderName,
			}
			history = append(history, hm)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"messages": history,
		})
	})


	// Ping处理器，用于检查Web服务器是否在线
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// 发送消息处理器
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Message string `json:"message"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		node.handleWebMessage(req.Message)
		w.WriteHeader(http.StatusOK)
	})

	// 获取消息处理器
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		node.MessagesMutex.RLock()
		messages := make([]ChatMessage, len(node.Messages))
		copy(messages, node.Messages)
		node.MessagesMutex.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"messages": messages,
		})
	})

	// 获取用户列表处理器
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		users := []string{node.Name + " (自己)"}
		
		node.PeersMutex.RLock()
		for _, peer := range node.Peers {
			if peer.IsActive {
				status := ""
				if node.isBlocked(peer.Address) {
					status = " (屏蔽)"
				}
				users = append(users, peer.Name + status)
			}
		}
		node.PeersMutex.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"users": users,
		})
	})

	// 获取屏蔽列表处理器
	mux.HandleFunc("/acl", func(w http.ResponseWriter, r *http.Request) {
		node.ACLMutex.RLock()
		defer node.ACLMutex.RUnlock()
		
		blocked := []string{}
		if acl, exists := node.ACLs[node.Address]; exists {
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
					blocked = append(blocked, displayName)
				}
			}
		}
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"blocked": blocked,
		})
	})

	// 发送文件处理器
	mux.HandleFunc("/sendfile", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var fileData []byte
		var fileName, targetName string

		// Support both JSON (Wails) and multipart form (browser)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			var req struct {
				File       string `json:"file"`       // base64
				FileName   string `json:"fileName"`
				TargetName string `json:"targetName"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "请求格式错误", http.StatusBadRequest)
				return
			}
			var err error
			fileData, err = base64.StdEncoding.DecodeString(req.File)
			if err != nil {
				http.Error(w, "文件数据解码失败", http.StatusBadRequest)
				return
			}
			fileName = req.FileName
			targetName = req.TargetName
		} else {
			// Multipart form (browser mode)
			if err := r.ParseMultipartForm(1 << 30); err != nil { // 1GB multipart limit
				http.Error(w, "文件太大或格式错误", http.StatusBadRequest)
				return
			}
			file, handler, err := r.FormFile("file")
			if err != nil {
				http.Error(w, "无法获取文件", http.StatusBadRequest)
				return
			}
			defer file.Close()
			fileData, err = io.ReadAll(file)
			if err != nil {
				http.Error(w, "读取文件失败", http.StatusInternalServerError)
				return
			}
			fileName = handler.Filename
			targetName = r.FormValue("targetName")
		}

		if targetName == "" {
			http.Error(w, "请选择目标用户", http.StatusBadRequest)
			return
		}

		// 保存到uploads目录
		uploadDir := DataPath("uploads")
		if err := os.MkdirAll(uploadDir, 0755); err != nil {
			http.Error(w, "无法创建上传目录", http.StatusInternalServerError)
			return
		}

		filePath := filepath.Join(uploadDir, fileName)
		if err := os.WriteFile(filePath, fileData, 0644); err != nil {
			http.Error(w, "保存文件失败", http.StatusInternalServerError)
			return
		}

		fileId := node.sendFileTransferRequest(filePath, targetName)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "success",
			"message": "文件传输请求已发送",
			"fileId":  fileId,
		})
	})

	// 发送图片处理器
	mux.HandleFunc("/sendimage", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var imageData []byte
		var fileName, contentType, targetName string
		var fileSize int64

		// Support both JSON (Wails) and multipart form (browser)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			var req struct {
				Image      string `json:"image"`      // base64
				FileName   string `json:"fileName"`
				FileType   string `json:"fileType"`
				FileSize   int64  `json:"fileSize"`
				TargetName string `json:"targetName"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "请求格式错误", http.StatusBadRequest)
				return
			}
			var err error
			imageData, err = base64.StdEncoding.DecodeString(req.Image)
			if err != nil {
				http.Error(w, "图片数据解码失败", http.StatusBadRequest)
				return
			}
			fileName = req.FileName
			contentType = req.FileType
			fileSize = req.FileSize
			targetName = req.TargetName
		} else {
			// Multipart form (browser mode)
			if err := r.ParseMultipartForm(1 << 30); err != nil { // 1GB multipart limit
				http.Error(w, "文件太大或格式错误", http.StatusBadRequest)
				return
			}
			file, handler, err := r.FormFile("image")
			if err != nil {
				http.Error(w, "无法获取图片文件", http.StatusBadRequest)
				return
			}
			defer file.Close()
			imageData, err = io.ReadAll(file)
			if err != nil {
				http.Error(w, "读取图片数据失败", http.StatusInternalServerError)
				return
			}
			fileName = handler.Filename
			contentType = handler.Header.Get("Content-Type")
			fileSize = handler.Size
			targetName = r.FormValue("targetName")
		}

		if !strings.HasPrefix(contentType, "image/") {
			http.Error(w, "只支持图片文件", http.StatusBadRequest)
			return
		}
		if len(imageData) > 5<<20 {
			http.Error(w, "图片文件不能超过5MB", http.StatusBadRequest)
			return
		}
		if targetName == "" {
			http.Error(w, "请选择目标用户", http.StatusBadRequest)
			return
		}

		// 创建images目录并保存
		imageDir := DataPath("images")
		if err := os.MkdirAll(imageDir, 0755); err != nil {
			http.Error(w, "无法创建图片目录", http.StatusInternalServerError)
			return
		}

		ext := filepath.Ext(fileName)
		if ext == "" {
			ext = ".jpg"
		}
		imageFileName := fmt.Sprintf("%s_%d%s", generateMessageID(), time.Now().Unix(), ext)
		imagePath := filepath.Join(imageDir, imageFileName)

		if err := os.WriteFile(imagePath, imageData, 0644); err != nil {
			http.Error(w, "保存图片失败", http.StatusInternalServerError)
			return
		}

		imageBase64 := base64.StdEncoding.EncodeToString(imageData)
		imageURL := fmt.Sprintf("/images/%s", imageFileName)
		messageID := generateMessageID()

		imageMsg := Message{
			Type:        "chat",
			From:        node.ID,
			Content:     fmt.Sprintf("发送了图片: %s", fileName),
			Timestamp:   time.Now(),
			MessageType: MessageTypeImage,
			MessageID:   messageID,
			FileName:    fileName,
			FileSize:    fileSize,
			FileType:    contentType,
			FileURL:     imageURL,
			FileData:    imageBase64,
		}

		if targetName == "all" {
			imageMsg.To = "all"
			node.broadcastMessage(imageMsg)
		} else {
			var targetID string
			node.PeersMutex.RLock()
			for id, peer := range node.Peers {
				if peer.Name == targetName && peer.IsActive {
					targetID = id
					break
				}
			}
			node.PeersMutex.RUnlock()

			if targetID == "" {
				http.Error(w, "目标用户不在线", http.StatusBadRequest)
				return
			}

			imageMsg.To = targetID
			if peer, exists := node.Peers[targetID]; exists {
				node.sendMessageToPeer(peer, imageMsg)
			}
		}

		isPrivate := targetName != "all"
		node.addChatMessageWithType(
			node.Name, targetName, imageMsg.Content, true, isPrivate,
			MessageTypeImage, messageID, "", "", "", fileName, fileSize, contentType, imageURL, "",
		)

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status":   "success",
			"imageUrl": imageURL,
			"messageId": messageID,
		})
	})

	// 发送文件消息处理器（用于文件预览）
	mux.HandleFunc("/sendfilemsg", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			TargetName string `json:"targetName"`
			FileName   string `json:"fileName"`
			FileSize   int64  `json:"fileSize"`
			FileType   string `json:"fileType"`
			FileID     string `json:"fileId"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		if req.TargetName == "" || req.FileName == "" {
			http.Error(w, "缺少必要参数", http.StatusBadRequest)
			return
		}

		// 查找目标用户ID
		var targetID string
		node.PeersMutex.RLock()
		for id, peer := range node.Peers {
			if peer.Name == req.TargetName && peer.IsActive {
				targetID = id
				break
			}
		}
		node.PeersMutex.RUnlock()

		if targetID == "" {
			http.Error(w, "目标用户不在线", http.StatusBadRequest)
			return
		}

		messageID := generateMessageID()
		content := fmt.Sprintf("分享了文件: %s", req.FileName)

		// 创建文件消息
		fileMsg := Message{
			Type:        "chat",
			From:        node.ID,
			To:          targetID,
			Content:     content,
			Timestamp:   time.Now(),
			MessageType: MessageTypeFile,
			MessageID:   messageID,
			FileName:    req.FileName,
			FileSize:    req.FileSize,
			FileType:    req.FileType,
			FileID:      req.FileID,
		}

		// 发送消息
		if peer, exists := node.Peers[targetID]; exists {
			node.sendMessageToPeer(peer, fileMsg)
		}

		// 添加到本地消息列表
		isPrivate := targetID != "all"
		node.addChatMessageWithType(
			node.Name, req.TargetName, content, true, isPrivate,
			MessageTypeFile, messageID, "", "", "", req.FileName, req.FileSize, req.FileType, "", req.FileID,
		)

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status":    "success",
			"messageId": messageID,
		})
	})

	// 发送回复消息处理器
	mux.HandleFunc("/sendreply", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			TargetName     string `json:"targetName"`
			ReplyContent   string `json:"replyContent"`
			OriginalMsgID  string `json:"originalMsgId"`
			OriginalSender string `json:"originalSender"`
			OriginalContent string `json:"originalContent"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		if req.TargetName == "" || req.ReplyContent == "" || req.OriginalMsgID == "" {
			http.Error(w, "缺少必要参数", http.StatusBadRequest)
			return
		}

		// 查找目标用户ID
		var targetID string
		node.PeersMutex.RLock()
		for id, peer := range node.Peers {
			if peer.Name == req.TargetName && peer.IsActive {
				targetID = id
				break
			}
		}
		node.PeersMutex.RUnlock()

		if targetID == "" {
			http.Error(w, "目标用户不在线", http.StatusBadRequest)
			return
		}

		messageID := generateMessageID()
		content := req.ReplyContent

		// 创建回复消息
		replyMsg := Message{
			Type:            "chat",
			From:            node.ID,
			To:              targetID,
			Content:         content,
			Timestamp:       time.Now(),
			MessageType:     MessageTypeReply,
			MessageID:       messageID,
			ReplyToID:       req.OriginalMsgID,
			ReplyToContent:  req.OriginalContent,
			ReplyToSender:   req.OriginalSender,
		}

		// 发送消息
		if peer, exists := node.Peers[targetID]; exists {
			node.sendMessageToPeer(peer, replyMsg)
		}

		// 添加到本地消息列表
		isPrivate := targetID != "all"
		node.addChatMessageWithType(
			node.Name, req.TargetName, content, true, isPrivate,
			MessageTypeReply, messageID, req.OriginalMsgID, req.OriginalContent, req.OriginalSender,
			"", 0, "", "", "",
		)

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status":    "success",
			"messageId": messageID,
		})
	})

	// 获取文件传输列表处理器
	mux.HandleFunc("/filetransfers", func(w http.ResponseWriter, r *http.Request) {
		node.FileTransfersMutex.RLock()
		transfers := make([]*FileTransferStatus, 0, len(node.FileTransfers))
		for _, transfer := range node.FileTransfers {
			transfers = append(transfers, transfer)
		}
		node.FileTransfersMutex.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"transfers": transfers,
		})
	})

	// 处理文件传输响应处理器
	mux.HandleFunc("/fileresponse", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			FileID   string `json:"fileId"`
			Accepted bool   `json:"accepted"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		// 调用核心逻辑来处理响应
		node.respondToFileTransfer(req.FileID, req.Accepted)

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// 取消文件传输处理器
	mux.HandleFunc("/filecancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			FileID string `json:"fileId"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		node.cancelFileTransfer(req.FileID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// 关闭Web服务器处理器
	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// 简单的身份验证 - 只允许本地访问
		if r.RemoteAddr != "127.0.0.1" && !strings.HasPrefix(r.RemoteAddr, "[::1]") {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Web服务器正在关闭..."))
		
		// 异步关闭Web服务器
		go node.stopWebGUI()
	})

	// 版本信息 - 供其他节点检查更新
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"version":     AppVersion,
			"channel":     AppChannel(),
			"name":        node.Name,
			"logLevel":    GetLogLevel(),
			"saveHistory": node.Config.IsSaveHistory(),
		})
	})

	// 修改日志级别处理器
	mux.HandleFunc("/loglevel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Level string `json:"level"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		level := strings.ToLower(req.Level)
		if level != "error" && level != "info" && level != "debug" {
			http.Error(w, "Invalid level. Use: error, info, debug", http.StatusBadRequest)
			return
		}

		SetLogLevel(level)
		Log.Info("日志级别已更改", "level", level)

		// Persist to config
		if node.Config != nil {
			node.Config.LogLevel = level
			SaveConfig(node.Config)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "level": level})
	})

	// 打开日志目录处理器
	mux.HandleFunc("/open-logs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		logDir := LogDir()
		os.MkdirAll(logDir, 0755)
		var err error
		switch runtime.GOOS {
		case "windows":
			err = exec.Command("explorer", logDir).Start()
		case "darwin":
			err = exec.Command("open", logDir).Start()
		case "linux":
			err = exec.Command("xdg-open", logDir).Start()
		}
		if err != nil {
			http.Error(w, "无法打开日志目录", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "path": logDir})
	})

	// 保存聊天记录开关
	mux.HandleFunc("/save-history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(map[string]bool{"saveHistory": node.Config.IsSaveHistory()})
			return
		}
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			SaveHistory bool `json:"saveHistory"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
		node.Config.SaveHistory = &req.SaveHistory
		SaveConfig(node.Config)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// 删除指定聊天的历史记录
	mux.HandleFunc("/delete-chat-history", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ChatID string `json:"chatId"` // "all" for public chat, or peer name
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		// Delete from in-memory messages
		node.MessagesMutex.Lock()
		filtered := make([]ChatMessage, 0, len(node.Messages))
		for _, msg := range node.Messages {
			keep := true
			if req.ChatID == "all" {
				if !msg.IsPrivate {
					keep = false
				}
			} else {
				if msg.IsPrivate && (msg.Sender == req.ChatID || msg.Recipient == req.ChatID) {
					keep = false
				}
			}
			if keep {
				filtered = append(filtered, msg)
			}
		}
		node.Messages = filtered
		node.MessagesMutex.Unlock()

		// Delete from SQLite
		if node.DB != nil {
			if req.ChatID == "all" {
				node.DB.Exec("DELETE FROM messages WHERE is_private = 0")
			} else {
				node.DB.Exec("DELETE FROM messages WHERE is_private = 1 AND (sender = ? OR recipient = ?)", req.ChatID, req.ChatID)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// 打开文件（使用默认应用）
	mux.HandleFunc("/open-file", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
		var err error
		switch runtime.GOOS {
		case "windows":
			err = exec.Command("cmd", "/c", "start", "", req.Path).Start()
		case "darwin":
			err = exec.Command("open", req.Path).Start()
		case "linux":
			err = exec.Command("xdg-open", req.Path).Start()
		}
		if err != nil {
			http.Error(w, "无法打开文件", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// 在文件管理器中显示文件
	mux.HandleFunc("/open-folder", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
		var err error
		switch runtime.GOOS {
		case "windows":
			err = exec.Command("explorer", "/select,", req.Path).Start()
		case "darwin":
			err = exec.Command("open", "-R", req.Path).Start()
		case "linux":
			err = exec.Command("xdg-open", filepath.Dir(req.Path)).Start()
		}
		if err != nil {
			http.Error(w, "无法打开文件夹", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// 程序更新下载 - 供其他节点获取最新版本
	mux.HandleFunc("/update", func(w http.ResponseWriter, r *http.Request) {
		exePath, err := os.Executable()
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		exePath, _ = filepath.EvalSymlinks(exePath)
		http.ServeFile(w, r, exePath)
	})

	// 更新检查状态 - 供Web前端查询
	mux.HandleFunc("/check-update", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		node.PeersMutex.RLock()
		update := node.AvailableUpdate
		node.PeersMutex.RUnlock()
		if update != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"available":    true,
				"version":      update.Version,
				"channel":      update.Channel,
				"source":       update.Name,
				"crossChannel": isCrossChannel(update.Version),
			})
		} else {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"available": false,
				"version":   AppVersion,
				"channel":   AppChannel(),
			})
		}
	})

	// 执行更新处理器 - 供Web前端触发自动更新
	mux.HandleFunc("/perform-update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		node.PeersMutex.RLock()
		update := node.AvailableUpdate
		node.PeersMutex.RUnlock()
		if update == nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "没有可用更新"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "updating", "version": update.Version})
		go node.performUpdate()
	})

	// 更新进度状态
	mux.HandleFunc("/update-status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": node.UpdateStatus,
			"error":  node.UpdateError,
		})
	})

	// 重启应用
	mux.HandleFunc("/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "restarting"})
		go func() {
			time.Sleep(500 * time.Millisecond)
			// restartApplication starts the new process, saves config, then os.Exit(0).
			// No need to call node.Stop() — os.Exit terminates everything and
			// Windows releases the mutex handle automatically.
			if err := restartApplication(node); err != nil {
				Log.Error("重启失败", "error", err)
			}
		}()
	})

	// WebView2 运行时分享 - 供局域网内其他节点下载（支持本地文件夹和系统安装）
	mux.HandleFunc("/webview2runtime", serveWebView2Runtime)

	return mux
}

// 启动Web GUI服务器
func (node *P2PNode) startWebGUI() {
	if !node.WebEnabled {
		return
	}

	handler := node.createHTTPHandler()

	node.WebServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", node.WebPort),
		Handler: handler,
	}

	go func() {
		webURL := fmt.Sprintf("http://127.0.0.1:%d", node.WebPort)
		fmt.Printf("Web界面已启动: %s\n", webURL)
		fmt.Println("请手动在浏览器中打开上述URL访问Web界面")
		Log.Info("Web界面已启动", "url", webURL, "port", node.WebPort)

		if err := node.WebServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Web服务器启动失败: %v", err)
			Log.Error("Web服务器启动失败", "port", node.WebPort, "error", err)
		}
	}()
}

// 停止Web GUI服务器
func (node *P2PNode) stopWebGUI() {
	if node.WebServer != nil {
		fmt.Println("正在关闭Web服务器...")
		
		// 创建关闭上下文，最多等待5秒
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		
		if err := node.WebServer.Shutdown(ctx); err != nil {
			log.Printf("Web服务器关闭失败: %v", err)
			Log.Error("Web服务器关闭失败", "error", err)
		} else {
			fmt.Println("Web服务器已关闭")
			Log.Info("Web服务器已关闭")
		}
		
		node.WebEnabled = false
		node.WebServer = nil
	}
}

// 打开浏览器
func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		fmt.Printf("请手动在浏览器中打开: %s\n", url)
		return
	}
	if err != nil {
		fmt.Printf("无法自动打开浏览器，请手动访问: %s\n", url)
	}
}

// 处理Web消息
func (node *P2PNode) handleWebMessage(text string) {
	if strings.HasPrefix(text, "/") {
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

// renamePeerInMessages updates all in-memory and DB messages from oldName to newName.
// Called when a peer renames so conversations merge under the new name.
func (node *P2PNode) renamePeerInMessages(oldName, newName string) {
	// Update in-memory messages
	node.MessagesMutex.Lock()
	for i := range node.Messages {
		if node.Messages[i].Sender == oldName {
			node.Messages[i].Sender = newName
		}
		if node.Messages[i].Recipient == oldName {
			node.Messages[i].Recipient = newName
		}
	}
	node.MessagesMutex.Unlock()

	// Update SQLite
	if node.DB != nil {
		node.DB.Exec("UPDATE messages SET sender = ? WHERE sender = ?", newName, oldName)
		node.DB.Exec("UPDATE messages SET recipient = ? WHERE recipient = ?", newName, oldName)
	}
	Log.Info("已合并聊天记录", "from", oldName, "to", newName)
}

// 添加聊天消息（扩展版）
func (node *P2PNode) addChatMessage(sender, recipient, content string, isOwn, isPrivate bool) {
	node.addChatMessageWithType(sender, recipient, content, isOwn, isPrivate, MessageTypeText, "", "", "", "", "", 0, "", "", "")
}

// 添加聊天消息（完整版）
func (node *P2PNode) addChatMessageWithType(sender, recipient, content string, isOwn, isPrivate bool,
	messageType, messageID, replyToID, replyToContent, replyToSender, fileName string, fileSize int64, fileType, fileURL, fileID string) {

	// 生成消息ID（如果未提供）
	if messageID == "" {
		messageID = generateMessageID()
	}

	msg := ChatMessage{
		Sender:         sender,
		Recipient:      recipient,
		Content:        content,
		Timestamp:      time.Now(),
		IsOwn:          isOwn,
		IsPrivate:      isPrivate,
		MessageType:    messageType,
		MessageID:      messageID,
		ReplyToID:      replyToID,
		ReplyToContent: replyToContent,
		ReplyToSender:  replyToSender,
		FileName:       fileName,
		FileSize:       fileSize,
		FileType:       fileType,
		FileURL:        fileURL,
		FileID:         fileID,
	}

	if node.WebEnabled {
		node.MessagesMutex.Lock()
		node.Messages = append(node.Messages, msg)
		if len(node.Messages) > 500 {
			node.Messages = node.Messages[1:]
		}
		node.MessagesMutex.Unlock()
	}

	// 保存到数据库（会话中始终写入，退出时按设置决定是否清空）
	if node.DB != nil {
		ciphertext, nonce, err := encryptMessage(node.LocalDBKey, []byte(content))
		if err != nil {
			fmt.Printf("加密消息失败: %v\n", err)
			Log.Error("加密消息失败", "error", err)
		} else {
			_, err = node.DB.Exec(`
				INSERT INTO messages (
					sender, recipient, content, nonce, is_private, is_own,
					message_type, message_id, reply_to_id, reply_to_content,
					reply_to_sender, file_name, file_size, file_type, file_url, file_data, file_id
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				sender, recipient, ciphertext, nonce, isPrivate, isOwn,
				messageType, messageID, replyToID, replyToContent,
				replyToSender, fileName, fileSize, fileType, fileURL, "", fileID)
			if err != nil {
				fmt.Printf("保存消息到数据库失败: %v\n", err)
				Log.Error("保存消息到数据库失败", "sender", sender, "error", err)
			}
		}
	}

	// 命令行显示
	timestamp := time.Now().Format("15:04:05")
	displayContent := content
	if strings.HasPrefix(content, "emoji:") {
		emojiId := strings.TrimPrefix(content, "emoji:")
		if strings.HasPrefix(emojiId, "gif-") {
			emojiId = strings.TrimPrefix(emojiId, "gif-")
		}
		displayContent = "[发送了表情]"
		
		// 从 emoji_gifs.json 查找表情名称
		data, err := webFS.Open("emoji_gifs.json")
		if err == nil {
			defer data.Close()
			type EmojiEntry struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			var entries []EmojiEntry
			if json.NewDecoder(data).Decode(&entries) == nil {
				for _, e := range entries {
					if e.ID == emojiId {
						displayContent = fmt.Sprintf("[emoji: %s]", e.Name)
						break
					}
				}
			}
		}
	}
	if isPrivate {
		fmt.Printf("[%s] %s (私聊): %s\n", timestamp, sender, displayContent)
	} else {
		fmt.Printf("[%s] %s: %s\n", timestamp, sender, displayContent)
	}

	// Emit event for desktop app
	node.emitNewMessage(msg)
}
