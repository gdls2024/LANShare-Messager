package main

import (
	"database/sql"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/mdns"
	_ "modernc.org/sqlite"
)

// P2PNode结构体 - 主节点结构
type P2PNode struct {
	LocalIP   string
	LocalPort int
	Name      string
	ID        string
	Address   string // 新增：本地地址 "IP:port"

	Listener   net.Listener
	Peers      map[string]*Peer
	PeersMutex sync.RWMutex

	MessageChan chan Message
	StopCh      chan struct{} // Closed on shutdown to signal all goroutines
	Running     bool

	DiscoveryPort int
	BroadcastConn *net.UDPConn
	BroadcastAddr string
	MdnsServer    *mdns.Server

	// Web GUI相关
	WebPort      int
	Messages     []ChatMessage
	MessagesMutex sync.RWMutex
	WebEnabled   bool
	WebServer    *http.Server

	// 文件传输相关
	FileTransfers     map[string]*FileTransferStatus
	FileTransfersMutex sync.RWMutex
	ACLs              map[string]map[string]bool
	ACLMutex          sync.RWMutex
	DB                *sql.DB
	LocalDBKey        [32]byte

	// Node-level ECDH keys (persistent for node lifetime)
	NodePrivateKey [32]byte
	NodePublicKey  [32]byte

	// 内存管理
	lastCleanupTime   time.Time

	// Desktop mode (Wails)
	DesktopMode       bool
	OnNewMessage      func(ChatMessage)
	OnUserOnline      func(string)
	OnUserOffline     func(string)
	OnUpdateAvailable func(updateSource)
	OnBeforeRestart   func() // Called before restart to clean up desktop resources
	OnQuitApp         func() // Called to properly quit the app (triggers Wails shutdown)

	// Auto-update
	AvailableUpdate *updateSource
	UpdateStatus    string // "", "downloading", "completed", "failed"
	UpdateError     string

	// Config reference for runtime settings
	Config *AppConfig
}

// Peer结构体 - 对等节点结构
type Peer struct {
	ID            string
	Name          string
	Address       string
	Conn          net.Conn
	WriteMutex    sync.Mutex // 保护TCP连接写入，防止并发写入破坏JSON流
	IsActive      bool
	LastSeen      time.Time
	SharedKey     []byte    // 共享密钥 (derived from node private + peer public)
	PublicKey     [32]byte  // 对端公钥 (remote peer's public key)
	ReconnectAttempts int   // 重连尝试次数
	LastReconnectTime time.Time // 上次重连尝试时间
	IP            string    // IP地址
	Port          int       // 端口号
	WebPort       int       // HTTP端口号（用于更新检查等）
}

// Message结构体 - 通用消息结构
type Message struct {
	Type        string      `json:"type"`
	From        string      `json:"from"`
	To          string      `json:"to"`
	Content     string      `json:"content"`
	Timestamp   time.Time   `json:"timestamp"`
	Data        interface{} `json:"data,omitempty"`
	Encrypted   bool        `json:"encrypted"`
	Nonce       []byte      `json:"nonce,omitempty"`
	Ciphertext  []byte      `json:"ciphertext,omitempty"`
	SenderPubKey []byte     `json:"sender_pub_key,omitempty"`

	// 扩展字段：消息类型相关
	MessageType    string `json:"messageType,omitempty"`    // text, image, file, reply
	MessageID      string `json:"messageId,omitempty"`      // 消息唯一ID
	ReplyToID      string `json:"replyToId,omitempty"`      // 回复的消息ID
	ReplyToContent string `json:"replyToContent,omitempty"` // 回复的消息内容
	ReplyToSender  string `json:"replyToSender,omitempty"`  // 被回复消息的发送者
	FileName       string `json:"fileName,omitempty"`       // 文件名
	FileSize       int64  `json:"fileSize,omitempty"`       // 文件大小
	FileType       string `json:"fileType,omitempty"`       // 文件类型
	FileURL        string `json:"fileUrl,omitempty"`        // 文件URL
	FileData       string `json:"fileData,omitempty"`       // 文件base64数据（用于图片等小文件）
	FileID         string `json:"fileId,omitempty"`         // 文件传输ID（关联FileTransferStatus）
}

// DiscoveryMessage结构体 - 服务发现消息结构
type DiscoveryMessage struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	IP      string `json:"ip"`
	Port    int    `json:"port"`
	WebPort int    `json:"webPort,omitempty"` // HTTP port for LAN sharing
	Version string `json:"version,omitempty"` // App version for auto-update
	PubKey  []byte `json:"pubKey,omitempty"`  // Node public key for ECDH
}

// ChatMessage结构体 - 聊天消息结构
type ChatMessage struct {
	Sender    string    `json:"sender"`
	Recipient string    `json:"recipient"` // "all" for public, or username for private
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
	IsOwn     bool      `json:"isOwn"`
	IsPrivate bool      `json:"isPrivate"`

	// 扩展字段：消息类型相关
	MessageType    string `json:"messageType"`              // text, image, file, reply
	MessageID      string `json:"messageId"`                // 消息唯一ID
	ReplyToID      string `json:"replyToId,omitempty"`      // 回复的消息ID
	ReplyToContent string `json:"replyToContent,omitempty"` // 回复的消息内容
	ReplyToSender  string `json:"replyToSender,omitempty"`  // 被回复消息的发送者
	FileName       string `json:"fileName,omitempty"`       // 文件名
	FileSize       int64  `json:"fileSize,omitempty"`       // 文件大小
	FileType       string `json:"fileType,omitempty"`       // 文件类型
	FileURL        string `json:"fileUrl,omitempty"`        // 文件URL（用于Web界面）
	FileID         string `json:"fileId,omitempty"`         // 文件传输ID（关联FileTransferStatus）
}

// FileTransferRequest结构体 - 文件传输请求
type FileTransferRequest struct {
	Type        string    `json:"type"`
	FileID      string    `json:"fileId"`
	FileName    string    `json:"fileName"`
	FileSize    int64     `json:"fileSize"`
	From        string    `json:"from"`
	To          string    `json:"to"`
	Timestamp   time.Time `json:"timestamp"`
}

// FileTransferResponse结构体 - 文件传输响应
type FileTransferResponse struct {
	Type      string    `json:"type"`
	FileID    string    `json:"fileId"`
	Accepted  bool      `json:"accepted"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// FileChunk结构体 - 文件数据块
type FileChunk struct {
	Type        string    `json:"type"`
	FileID      string    `json:"fileId"`
	ChunkNum    int       `json:"chunkNum"`
	TotalChunks int       `json:"totalChunks"`
	Data        []byte    `json:"data,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	Encrypted   bool      `json:"encrypted"`
	Nonce       []byte    `json:"nonce,omitempty"`
	Ciphertext  []byte    `json:"ciphertext,omitempty"`
}

// ECDHKeyPair结构体 - ECDH密钥对
type ECDHKeyPair struct {
	PrivateKey [32]byte
	PublicKey  [32]byte
}

type FileTransferStatus struct {
	FileID         string    `json:"fileId"`
	FileName       string    `json:"fileName"`
	FilePath       string    `json:"-"` // 发送方的文件完整路径，不进行json序列化
	FileSize       int64     `json:"fileSize"`
	Progress       int64     `json:"progress"`
	Status         string    `json:"status"` // pending, transferring, completed, failed
	Direction      string    `json:"direction"` // send, receive
	PeerName       string    `json:"peerName"` // 对方的用户名
	PeerID         string    `json:"-"`        // 对方的peer ID，用于获取共享密钥
	FromID         string    `json:"-"`
	StartTime      time.Time `json:"startTime"`
	EndTime        time.Time `json:"endTime"`
	Speed          float64   `json:"speed"`          // 传输速度 (bytes/second)
	ETA            int64     `json:"eta"`            // 预计剩余时间 (seconds)
	LastUpdateTime time.Time `json:"-"`              // 上次更新时间，用于计算速度
	SavePath       string    `json:"savePath,omitempty"` // 接收文件保存路径
}

// 应用版本
// Stable: "1.0.0", "1.1.0"  |  Test: "1.0.1-beta", "1.1.0-rc1", "2.0.0-dev"
const AppVersion = "1.2.27-dev"

// AppChannel returns "stable" or "test" based on the version string.
func AppChannel() string {
	for _, c := range AppVersion {
		if c == '-' {
			return "test"
		}
	}
	return "stable"
}

// 消息类型常量
const (
	MessageTypeText  = "text"
	MessageTypeImage = "image"
	MessageTypeFile  = "file"
	MessageTypeReply = "reply"
)

// ImageMessage结构体 - 图片消息
type ImageMessage struct {
	FileName   string `json:"fileName"`
	FileSize   int64  `json:"fileSize"`
	ImageData  []byte `json:"-"` // 图片二进制数据，不进行JSON序列化
	ImageURL   string `json:"imageUrl,omitempty"` // Web访问URL
	Width      int    `json:"width,omitempty"`    // 图片宽度
	Height     int    `json:"height,omitempty"`   // 图片高度
	Thumbnail  []byte `json:"-"` // 缩略图数据
}

// FileMessage结构体 - 文件消息
type FileMessage struct {
	FileName string `json:"fileName"`
	FileSize int64  `json:"fileSize"`
	FileType string `json:"fileType"` // MIME类型或文件扩展名
	FileURL  string `json:"fileUrl,omitempty"` // Web访问URL
}

// ReplyMessage结构体 - 回复消息
type ReplyMessage struct {
	OriginalMessageID   string `json:"originalMessageId"`
	OriginalSender      string `json:"originalSender"`
	OriginalContent     string `json:"originalContent"`
	OriginalMessageType string `json:"originalMessageType"`
	ReplyContent        string `json:"replyContent"`
}
