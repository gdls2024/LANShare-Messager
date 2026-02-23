// LANShare - Telegram-style Web Client

// =================================
// Centralized State
// =================================
const AppState = {
    localUsername: '',
    currentChatId: null,      // null | 'all' | username
    allMessages: [],
    onlineUsers: [],          // current online users
    previousOnlineUsers: [],  // previous poll (for detecting changes)
    isFirstUserLoad: true,    // skip online notifications on first load
    isWails: false,           // Wails desktop mode flag
    blockedUsers: new Set(),
    fileTransfers: [],
    replyingTo: null,
    searchQuery: '',
    isMobile: window.innerWidth <= 768,
    showConversation: false,
    gifEmojis: [],
    allEmojis: [],
    historyOffset: 0,
    shownPendingTransfers: new Set(),
    shownFailedTransfers: new Set(),
    shownCompletedTransfers: new Set(),
    knownPartners: [],        // all chat partners from DB history (for offline users)
    selectedFile: null,
    mentionActive: false,
    mentionStartPos: -1,
    mentionIndex: 0,
    titleFlashInterval: null,
    originalTitle: 'LS Messager',
    // Settings (loaded from localStorage)
    settings: {
        fontSize: 15,
        msgNotify: true,
        onlineNotify: true,
        badgeCount: true,
        skin: 'telegram',
        sendMode: 'enter',  // 'enter' or 'ctrlenter'
    },
};

// Avatar color palette (Telegram-style)
const AVATAR_COLORS = [
    '#e17076', '#eda86c', '#a695e7', '#7bc862',  // 粉红、橙、紫、绿
    '#6ec9cb', '#65aadd', '#ee7aae', '#c9956b',  // 青、蓝、玫红、棕
    '#d4a03c', '#5bab6e', '#7b8be0', '#cf6a4e',  // 金黄、深绿、靛蓝、赤橙
    '#9c6ad0', '#4eafa6', '#d06a9c', '#6b89b5',  // 深紫、深青、桃红、钢蓝
];

const HISTORY_LIMIT = 50;

// =================================
// Initialization
// =================================
async function init() {
    AppState.localUsername = APP_DATA.name;
    AppState.originalTitle = document.title;

    // Load emoji list
    await loadGifEmojis();
    AppState.allEmojis = [...AppState.gifEmojis];
    createEmojiGrid();

    // Initial data load
    await loadBlockedUsers();
    loadUsers();
    loadChatPartners();
    loadHistory();
    loadMessages();
    loadFileTransfers();

    // Wails environment detection
    const isWails = typeof window.go !== 'undefined';
    AppState.isWails = isWails;

    if (isWails) {
        // Real-time events from Go - trigger immediate re-polls
        window.runtime.EventsOn("new-message", (msg) => {
            // Push message directly — avoids polling detection race conditions
            if (msg && msg.messageId) {
                const exists = AppState.allMessages.some(m => m.messageId === msg.messageId);
                if (!exists) {
                    AppState.allMessages.push(msg);
                    displayMessages();
                    renderChatList();
                    updateTitleBadge();
                }
            } else {
                loadMessages();
            }
            // System notification via Go binding for non-own messages
            if (!msg.isOwn && AppState.settings.msgNotify) {
                const chatId = msg.isPrivate ? msg.sender : 'all';
                if (chatId !== AppState.currentChatId || !document.hasFocus()) {
                    const preview = (msg.content || '').substring(0, 100);
                    window.go.main.DesktopApp.ShowNotification(
                        'LS Messager - ' + (msg.sender || ''),
                        preview,
                        chatId
                    );
                }
            }
            // @ mention notification in public chat
            if (!msg.isOwn && !msg.isPrivate && msg.content &&
                msg.content.includes('@' + AppState.localUsername)) {
                const mentionChatId = 'all';
                if (mentionChatId !== AppState.currentChatId || !document.hasFocus()) {
                    window.go.main.DesktopApp.ShowNotification(
                        '你被提到了 - ' + (msg.sender || ''),
                        (msg.content || '').substring(0, 100),
                        mentionChatId
                    );
                }
            }
        });
        const _recentOnlineEvents = {};
        window.runtime.EventsOn("user-online", (name) => {
            loadUsers();
            // Skip self and deduplicate within 5 seconds
            if (name === AppState.localUsername) return;
            const now = Date.now();
            if (_recentOnlineEvents[name] && now - _recentOnlineEvents[name] < 5000) return;
            _recentOnlineEvents[name] = now;
            insertSystemMessage(name + ' 已上线');
            if (AppState.settings.onlineNotify) {
                showToast(name + ' 已上线', 'info');
            }
        });
        window.runtime.EventsOn("user-offline", (name) => {
            loadUsers();
            if (name === AppState.localUsername) return;
            delete _recentOnlineEvents[name];
            insertSystemMessage(name + ' 已离线');
        });
        // When window gains focus, check if there's a pending notification chat to switch to.
        // This handles: systray double-click, Alt-Tab, taskbar click, etc.
        window.addEventListener('focus', () => {
            window.go.main.DesktopApp.GetAndClearLastNotifiedChat().then(chatId => {
                if (chatId) selectChat(chatId);
            });
        });
        window.runtime.EventsOn("update-available", (source) => {
            const banner = document.getElementById('updateBanner');
            const text = document.getElementById('updateBannerText');
            const btn = document.getElementById('updateBannerBtn');
            if (source && source.version) {
                const label = channelLabel(source.channel);
                text.textContent = `新版本 v${source.version} [${label}] (来自 ${source.name})`;
                banner.style.display = 'flex';
                _lastUpdateCrossChannel = (source.channel !== _localChannel);
                if (_lastUpdateVersion !== source.version) {
                    _lastUpdateVersion = source.version;
                    btn.disabled = false;
                    btn.textContent = '更新';
                }
            } else {
                // Update source peer went offline — hide the banner
                banner.style.display = 'none';
                _lastUpdateVersion = null;
            }
        });

        // Wails built-in file drop — gives file paths directly (same speed as paperclip)
        // Supports both files and folders (folders are auto-zipped by Go side).
        window.runtime.OnFileDrop((x, y, paths) => {
            if (!paths || paths.length === 0) return;
            if (!AppState.currentChatId) {
                showToast('请先选择一个聊天', 'warning');
                return;
            }
            const target = AppState.currentChatId;
            const IMAGE_EXTS = ['jpg', 'jpeg', 'png', 'gif', 'bmp', 'webp'];
            for (const filePath of paths) {
                const ext = filePath.split('.').pop().toLowerCase();
                // Only treat as image if path has a dot (not a folder) and ext matches
                if (filePath.includes('.') && IMAGE_EXTS.includes(ext)) {
                    window.go.main.DesktopApp.SendImagePath(filePath, target)
                        .then(r => { if (r && r.status === 'success') { showToast('图片发送成功', 'success'); loadMessages(); } })
                        .catch(err => showToast('图片发送失败: ' + err, 'error'));
                } else {
                    if (target === 'all') { showToast('文件传输需要在私聊中使用', 'warning'); continue; }
                    if (!AppState.onlineUsers.includes(target)) { showToast('对方不在线', 'warning'); continue; }
                    showToast('正在处理...', 'info');
                    window.go.main.DesktopApp.SendFilePath(filePath, target)
                        .then(r => { if (r && r.fileId) postFileMsgAfterSend(target, r.fileName, parseInt(r.fileSize) || 0, '', r.fileId); })
                        .catch(err => showToast('发送失败: ' + err, 'error'));
                }
            }
        }, true);

        // Paste handler: images → Go binding, files → JSON POST
        document.addEventListener('paste', (e) => {
            const files = e.clipboardData && e.clipboardData.files;
            if (!files || files.length === 0) return;
            e.preventDefault();
            if (!AppState.currentChatId) { showToast('请先选择一个聊天', 'warning'); return; }
            for (const file of files) {
                if (isImageFile(file)) {
                    sendImage(file);
                } else {
                    sendDroppedFile(file);
                }
            }
        });

        // Save window size on resize (config save can't rely on shutdown alone
        // because HideWindowOnClose hides the window without triggering shutdown)
        let _resizeSaveTimer = null;
        window.addEventListener('resize', () => {
            clearTimeout(_resizeSaveTimer);
            _resizeSaveTimer = setTimeout(() => {
                window.go.main.DesktopApp.SaveWindowSize();
            }, 500); // debounce 500ms
        });

        // Reduced polling in Wails mode (events handle most updates)
        setInterval(loadMessages, 5000);
        setInterval(() => {
            loadBlockedUsers();
            loadUsers();
        }, 10000);
        startFileTransferPolling();
        setInterval(checkConnection, 10000);
    } else {
        // Browser mode: standard polling
        setInterval(loadMessages, 2000);
        setInterval(() => {
            loadBlockedUsers();
            loadUsers();
        }, 3000);
        startFileTransferPolling();
        setInterval(checkConnection, 5000);
    }

    // Update check (both modes)
    setTimeout(checkForUpdate, 10000);
    setInterval(checkForUpdate, 30000);

    // Initialize UI modules
    loadSettings();
    initSettings();
    initChatSwitching();
    initInputHandlers();
    initEmojiPicker();
    initFileTransfer();
    initAttachMenu();
    initSearchFilter();
    initResponsive();
    initDragAndDrop();
    initUpdateBanner();

    // Notification permission (browser mode only)
    if (!isWails && 'Notification' in window && Notification.permission === 'default') {
        Notification.requestPermission();
    }

    console.log('LANShare Telegram UI initialized' + (isWails ? ' (Wails desktop mode)' : ''));
}

// =================================
// Utility Functions
// =================================
function hashCode(str) {
    let hash = 0;
    for (let i = 0; i < str.length; i++) {
        hash = ((hash << 5) - hash) + str.charCodeAt(i);
        hash |= 0;
    }
    return Math.abs(hash);
}

function getAvatarColor(name) {
    return AVATAR_COLORS[hashCode(name) % AVATAR_COLORS.length];
}

function getAvatarLetter(name) {
    if (!name) return '?';
    return name.charAt(0).toUpperCase();
}

function formatTime(date) {
    return date.toLocaleTimeString('zh-CN', { hour12: false, hour: '2-digit', minute: '2-digit' });
}

function formatDate(date) {
    const today = new Date();
    const yesterday = new Date(today);
    yesterday.setDate(yesterday.getDate() - 1);

    if (date.toDateString() === today.toDateString()) return '今天';
    if (date.toDateString() === yesterday.toDateString()) return '昨天';
    return date.toLocaleDateString('zh-CN', { year: 'numeric', month: 'long', day: 'numeric' });
}

function formatChatTime(date) {
    const now = new Date();
    const diff = now - date;
    if (diff < 86400000 && date.toDateString() === now.toDateString()) {
        return formatTime(date);
    }
    const yesterday = new Date(now);
    yesterday.setDate(yesterday.getDate() - 1);
    if (date.toDateString() === yesterday.toDateString()) return '昨天';
    return date.toLocaleDateString('zh-CN', { month: 'numeric', day: 'numeric' });
}

function formatBytes(bytes, decimals = 1) {
    if (bytes === 0) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB', 'GB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(decimals)) + ' ' + sizes[i];
}

function formatSpeed(bytesPerSecond) {
    if (bytesPerSecond < 1024) return `${bytesPerSecond.toFixed(0)} B/s`;
    if (bytesPerSecond < 1024 * 1024) return `${(bytesPerSecond / 1024).toFixed(1)} KB/s`;
    return `${(bytesPerSecond / (1024 * 1024)).toFixed(1)} MB/s`;
}

function formatETA(seconds) {
    if (seconds < 60) return `${seconds}秒`;
    if (seconds < 3600) return `${Math.floor(seconds / 60)}分${seconds % 60}秒`;
    return `${Math.floor(seconds / 3600)}时${Math.floor((seconds % 3600) / 60)}分`;
}

function isScrolledToBottom(el) {
    return el.scrollHeight - el.clientHeight <= el.scrollTop + 5;
}

function scrollToBottom(el) {
    el.scrollTop = el.scrollHeight;
}

function getFileIcon(fileType) {
    if (!fileType) return '📎';
    if (fileType.startsWith('image/')) return '🖼️';
    if (fileType.startsWith('video/')) return '🎥';
    if (fileType.startsWith('audio/')) return '🎵';
    if (fileType.includes('pdf')) return '📄';
    if (fileType.includes('zip') || fileType.includes('rar')) return '📦';
    if (fileType.includes('doc') || fileType.includes('txt')) return '📝';
    return '📎';
}

function escapeHtml(str) {
    const div = document.createElement('div');
    div.textContent = str;
    return div.innerHTML;
}

// =================================
// Unread Count Management
// =================================
function getUnreadCount(chatId) {
    const lastRead = parseInt(localStorage.getItem(`lastRead_${chatId}`) || '0', 10);
    return AppState.allMessages.filter(msg => {
        const ts = new Date(msg.timestamp).getTime();
        if (ts <= lastRead) return false;
        if (msg.isOwn) return false;
        if (chatId === 'all') return !msg.isPrivate;
        return msg.isPrivate &&
            (msg.sender === chatId || msg.recipient === chatId);
    }).length;
}

function markChatAsRead(chatId) {
    localStorage.setItem(`lastRead_${chatId}`, Date.now().toString());
    updateTitleBadge();
}

function getTotalUnread() {
    let total = getUnreadCount('all');
    AppState.onlineUsers.forEach(u => {
        total += getUnreadCount(u);
    });
    // Also check users that have messages but may be offline
    const chatUsers = new Set();
    AppState.allMessages.forEach(msg => {
        if (msg.isPrivate && !msg.isOwn && msg.sender) chatUsers.add(msg.sender);
    });
    chatUsers.forEach(u => {
        if (!AppState.onlineUsers.includes(u)) {
            total += getUnreadCount(u);
        }
    });
    return total;
}

function updateTitleBadge() {
    if (!AppState.settings.badgeCount) {
        document.title = AppState.originalTitle;
    } else {
        const total = getTotalUnread();
        document.title = total > 0 ? `(${total}) ${AppState.originalTitle}` : AppState.originalTitle;
    }
    updateBackBadge();
}

function updateBackBadge() {
    const badge = document.getElementById('backBadge');
    if (!badge) return;
    // Count unread in all chats OTHER than the currently open one
    let otherUnread = 0;
    const currentChat = AppState.currentChatId;
    if (currentChat === 'all') {
        // Viewing public chat - count private unread
        const chatUsers = new Set();
        AppState.onlineUsers.forEach(u => chatUsers.add(u));
        AppState.allMessages.forEach(msg => {
            if (msg.isPrivate && !msg.isOwn && msg.sender) chatUsers.add(msg.sender);
        });
        chatUsers.forEach(u => { otherUnread += getUnreadCount(u); });
    } else if (currentChat) {
        // Viewing a private chat - count public + other private unread
        otherUnread = getUnreadCount('all');
        const chatUsers = new Set();
        AppState.onlineUsers.forEach(u => chatUsers.add(u));
        AppState.allMessages.forEach(msg => {
            if (msg.isPrivate && !msg.isOwn && msg.sender) chatUsers.add(msg.sender);
        });
        chatUsers.forEach(u => {
            if (u !== currentChat) otherUnread += getUnreadCount(u);
        });
    }
    if (otherUnread > 0) {
        badge.textContent = otherUnread > 99 ? '99+' : otherUnread;
        badge.classList.add('visible');
    } else {
        badge.classList.remove('visible');
    }
}

// =================================
// Chat List Model
// =================================
function buildChatList() {
    const chats = [];

    // 1. Public chat
    const publicMessages = AppState.allMessages.filter(m => !m.isPrivate);
    const lastPublic = publicMessages.length > 0 ? publicMessages[publicMessages.length - 1] : null;
    // For sort order: only my own messages in public chat affect the position
    const myLastPublic = publicMessages.filter(m => m.isOwn).pop();
    chats.push({
        id: 'all',
        name: '公共聊天',
        type: 'public',
        avatarColor: getAccentColor(),
        avatarIcon: AppState.settings.skin === 'wisetalk' ? '💬' : '🌐',
        lastMessage: lastPublic ? getMessagePreview(lastPublic) : '',
        lastSender: lastPublic && !lastPublic.isOwn ? lastPublic.sender : (lastPublic && lastPublic.isOwn ? '我' : ''),
        lastTimestamp: myLastPublic ? new Date(myLastPublic.timestamp) : new Date(0),
        unreadCount: getUnreadCount('all'),
        isOnline: true,
    });

    // 2. Collect all private chat partners (from messages + online users + DB history)
    const chatPartners = new Set();
    AppState.allMessages.forEach(msg => {
        if (msg.isPrivate) {
            if (msg.isOwn && msg.recipient) chatPartners.add(msg.recipient);
            else if (!msg.isOwn && msg.sender) chatPartners.add(msg.sender);
        }
    });
    AppState.onlineUsers.forEach(u => chatPartners.add(u));
    AppState.knownPartners.forEach(u => chatPartners.add(u));

    chatPartners.forEach(partner => {
        if (partner === AppState.localUsername || partner === 'all') return;
        const pmsgs = AppState.allMessages.filter(m =>
            m.isPrivate &&
            ((m.sender === partner && m.recipient === AppState.localUsername) ||
             (m.isOwn && m.recipient === partner))
        );
        const lastMsg = pmsgs.length > 0 ? pmsgs[pmsgs.length - 1] : null;
        const isOnline = AppState.onlineUsers.includes(partner);
        const isBlocked = AppState.blockedUsers.has(partner);

        chats.push({
            id: partner,
            name: partner,
            type: 'private',
            avatarColor: getAvatarColor(partner),
            avatarLetter: getAvatarLetter(partner),
            lastMessage: lastMsg ? getMessagePreview(lastMsg) : (isOnline ? '在线' : ''),
            lastSender: lastMsg && lastMsg.isOwn ? '我' : '',
            lastTimestamp: lastMsg ? new Date(lastMsg.timestamp) : new Date(0),
            unreadCount: getUnreadCount(partner),
            isOnline,
            isBlocked,
        });
    });

    // Sort by lastTimestamp descending (most recent conversation first)
    chats.sort((a, b) => b.lastTimestamp - a.lastTimestamp);

    return chats;
}

function getMessagePreview(msg) {
    if (!msg) return '';
    if (msg.content && msg.content.startsWith('emoji:')) return '[表情]';
    if (msg.messageType === 'image') return '📷 图片';
    if (msg.messageType === 'file') return `📎 ${msg.fileName || '文件'}`;
    if (msg.messageType === 'reply') return msg.content || '';
    return msg.content || '';
}

// =================================
// Chat List Rendering
// =================================
function renderChatList() {
    const chatList = document.getElementById('chatList');
    const chats = buildChatList();
    const query = AppState.searchQuery.toLowerCase();

    // Filter by search
    const filtered = query
        ? chats.filter(c => c.name.toLowerCase().includes(query))
        : chats;

    chatList.innerHTML = '';

    filtered.forEach(chat => {
        const item = document.createElement('div');
        item.className = 'tg-chat-item';
        if (chat.id === AppState.currentChatId) item.classList.add('active');
        if (chat.isBlocked) item.classList.add('blocked');
        item.dataset.chatId = chat.id;

        // Avatar wrapper (for online dot positioning)
        const avatarWrap = document.createElement('div');
        avatarWrap.className = 'tg-avatar-wrap';

        const avatar = document.createElement('div');
        avatar.className = 'tg-avatar';
        if (chat.type === 'public') {
            avatar.classList.add('public');
            avatar.textContent = chat.avatarIcon;
        } else {
            avatar.style.background = chat.avatarColor;
            avatar.textContent = chat.avatarLetter;
        }
        avatarWrap.appendChild(avatar);

        // Online indicator dot
        if (chat.isOnline && chat.type === 'private') {
            const onlineDot = document.createElement('span');
            onlineDot.className = 'tg-avatar-online-dot';
            avatarWrap.appendChild(onlineDot);
        }

        // Body
        const body = document.createElement('div');
        body.className = 'tg-chat-body';

        // Top row (name + time)
        const top = document.createElement('div');
        top.className = 'tg-chat-top';

        const nameEl = document.createElement('div');
        nameEl.className = 'tg-chat-name';
        nameEl.textContent = chat.name;

        const timeEl = document.createElement('div');
        timeEl.className = 'tg-chat-time';
        if (chat.unreadCount > 0) timeEl.classList.add('has-unread');
        if (chat.lastTimestamp.getTime() > 0) {
            timeEl.textContent = formatChatTime(chat.lastTimestamp);
        }

        top.appendChild(nameEl);
        top.appendChild(timeEl);

        // Bottom row (preview + badge)
        const bottom = document.createElement('div');
        bottom.className = 'tg-chat-bottom';

        const preview = document.createElement('div');
        preview.className = 'tg-chat-preview';
        if (chat.lastSender && chat.lastMessage) {
            preview.innerHTML = `<span class="preview-sender">${escapeHtml(chat.lastSender)}: </span>${escapeHtml(chat.lastMessage)}`;
        } else if (chat.lastMessage) {
            preview.textContent = chat.lastMessage;
        } else if (chat.type === 'private') {
            preview.textContent = chat.isOnline ? '在线' : '离线';
            if (!chat.isOnline) preview.classList.add('offline');
        }

        bottom.appendChild(preview);

        if (chat.unreadCount > 0) {
            const badge = document.createElement('div');
            badge.className = 'tg-badge';
            badge.textContent = chat.unreadCount > 99 ? '99+' : chat.unreadCount;
            bottom.appendChild(badge);
        }

        body.appendChild(top);
        body.appendChild(bottom);

        item.appendChild(avatarWrap);
        item.appendChild(body);

        item.addEventListener('click', () => selectChat(chat.id));

        // Right-click context menu
        item.addEventListener('contextmenu', (e) => {
            e.preventDefault();
            showChatContextMenu(e, chat);
        });

        chatList.appendChild(item);
    });
}

// =================================
// Chat Context Menu
// =================================
function showChatContextMenu(e, chat) {
    // Remove any existing context menu
    const old = document.querySelector('.tg-context-menu');
    if (old) old.remove();

    const menu = document.createElement('div');
    menu.className = 'tg-context-menu';

    const deleteBtn = document.createElement('div');
    deleteBtn.className = 'tg-context-menu-item danger';
    deleteBtn.textContent = '删除聊天记录';
    deleteBtn.onclick = () => {
        menu.remove();
        deleteChatHistory(chat.id, chat.name);
    };
    menu.appendChild(deleteBtn);

    // Position the menu
    menu.style.left = e.clientX + 'px';
    menu.style.top = e.clientY + 'px';
    document.body.appendChild(menu);

    // Adjust if off-screen
    const rect = menu.getBoundingClientRect();
    if (rect.right > window.innerWidth) {
        menu.style.left = (e.clientX - rect.width) + 'px';
    }
    if (rect.bottom > window.innerHeight) {
        menu.style.top = (e.clientY - rect.height) + 'px';
    }

    // Close on click outside
    const closeMenu = (ev) => {
        if (!menu.contains(ev.target)) {
            menu.remove();
            document.removeEventListener('click', closeMenu);
        }
    };
    setTimeout(() => document.addEventListener('click', closeMenu), 0);
}

async function deleteChatHistory(chatId, chatName) {
    const label = chatId === 'all' ? '公共聊天' : chatName;
    const ok = await showConfirm(`确定要删除与「${label}」的所有聊天记录吗？`);
    if (!ok) return;

    fetch('/delete-chat-history', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ chatId })
    })
    .then(r => {
        if (r.ok) {
            // Remove from in-memory messages too
            if (chatId === 'all') {
                AppState.allMessages = AppState.allMessages.filter(m => m.isPrivate);
            } else {
                AppState.allMessages = AppState.allMessages.filter(m =>
                    !(m.isPrivate && (m.sender === chatId || m.recipient === chatId))
                );
            }
            renderChatList();
            if (AppState.currentChatId === chatId) {
                displayMessages();
            }
            showToast(`已删除与「${label}」的聊天记录`, 'success');
        } else {
            throw new Error();
        }
    })
    .catch(() => showToast('删除失败', 'error'));
}

// =================================
// Chat Selection
// =================================
function selectChat(chatId) {
    AppState.currentChatId = chatId;
    AppState.showConversation = true;
    AppState.historyOffset = 0;
    cancelReply();

    // Mark as read
    markChatAsRead(chatId);

    // Update conversation header
    updateConversationHeader();

    // Show conversation panel
    document.getElementById('mainEmpty').style.display = 'none';
    document.getElementById('conversation').style.display = 'flex';

    // Mobile: hide sidebar
    if (AppState.isMobile) {
        document.querySelector('.tg-sidebar').classList.add('hidden');
    }

    // Update input placeholder
    const input = document.getElementById('messageInput');
    input.value = '';
    input.placeholder = chatId === 'all' ? '输入公共消息...' : `给 ${chatId} 发消息...`;
    input.focus();

    // Re-render
    renderChatList();
    displayMessages();

    // Load history for this chat, then force scroll to bottom
    loadHistory();

    // Defer scroll to ensure DOM layout is complete (container just became visible)
    setTimeout(() => scrollToBottom(document.getElementById('messages')), 50);
}

function updateConversationHeader() {
    const chatId = AppState.currentChatId;
    if (!chatId) return;

    const avatar = document.getElementById('convAvatar');
    const nameEl = document.getElementById('convName');
    const statusEl = document.getElementById('convStatus');
    const blockBtn = document.getElementById('blockToggleBtn');

    let peerOffline = false;
    if (chatId === 'all') {
        avatar.style.background = getAccentColor();
        avatar.textContent = AppState.settings.skin === 'wisetalk' ? '💬' : '🌐';
        nameEl.textContent = '公共聊天';
        const count = AppState.onlineUsers.length;
        statusEl.textContent = `${count} 位在线成员`;
        statusEl.className = 'tg-conv-status';
        blockBtn.style.display = 'none';
    } else {
        const color = getAvatarColor(chatId);
        avatar.style.background = color;
        avatar.textContent = getAvatarLetter(chatId);
        nameEl.textContent = chatId;
        const isOnline = AppState.onlineUsers.includes(chatId);
        peerOffline = !isOnline;
        statusEl.textContent = isOnline ? '在线' : '离线';
        statusEl.className = 'tg-conv-status' + (isOnline ? ' online' : '');
        blockBtn.style.display = '';
        const isBlocked = AppState.blockedUsers.has(chatId);
        blockBtn.textContent = isBlocked ? '🔓' : '🚫';
        blockBtn.title = isBlocked ? '解除屏蔽' : '屏蔽用户';
    }

    // Disable input area when viewing an offline private chat
    const inputArea = document.querySelector('.tg-input-area');
    if (inputArea) {
        if (peerOffline) {
            inputArea.classList.add('disabled');
            document.getElementById('messageInput').disabled = true;
            document.getElementById('messageInput').placeholder = `${chatId} 已离线，无法发送消息`;
        } else {
            inputArea.classList.remove('disabled');
            document.getElementById('messageInput').disabled = false;
            document.getElementById('messageInput').placeholder = chatId === 'all' ? '输入公共消息...' : `给 ${chatId} 发消息...`;
        }
    }
}

// =================================
// Chat Switching & Navigation
// =================================
function initChatSwitching() {
    // Back button for mobile
    document.getElementById('backBtn').addEventListener('click', () => {
        AppState.showConversation = false;
        AppState.currentChatId = null;
        document.getElementById('conversation').style.display = 'none';
        document.getElementById('mainEmpty').style.display = 'flex';
        if (AppState.isMobile) {
            document.querySelector('.tg-sidebar').classList.remove('hidden');
        }
        renderChatList();
    });

    // Block toggle button
    document.getElementById('blockToggleBtn').addEventListener('click', () => {
        if (AppState.currentChatId && AppState.currentChatId !== 'all') {
            blockUser(AppState.currentChatId);
        }
    });

    // File transfers panel removed — all transfers shown inline in conversation
}

// =================================
// Input Handlers
// =================================
function autoResizeInput() {
    const input = document.getElementById('messageInput');
    input.style.height = 'auto';
    input.style.height = input.scrollHeight + 'px';
}

function initInputHandlers() {
    const input = document.getElementById('messageInput');
    input.addEventListener('input', autoResizeInput);
    input.addEventListener('keypress', (e) => {
        if (e.key === 'Enter') {
            if (AppState.mentionActive && !e.shiftKey) {
                e.preventDefault();
                const items = document.querySelectorAll('.tg-mention-item');
                if (items.length > 0) insertMention(items[AppState.mentionIndex].dataset.name);
                return;
            }
            var mode = AppState.settings.sendMode || 'enter';
            if (mode === 'enter' && !e.shiftKey) {
                e.preventDefault();
                sendMessage();
            } else if (mode === 'ctrlenter' && e.ctrlKey) {
                e.preventDefault();
                sendMessage();
            }
        }
    });

    input.addEventListener('input', handleMentionInput);
    input.addEventListener('keydown', handleMentionKeydown);
    input.addEventListener('blur', () => setTimeout(closeMentionDropdown, 200));

    // Send button removed — Enter key is the only send trigger

    // Reply close
    document.getElementById('replyCloseBtn').addEventListener('click', cancelReply);
}

// =================================
// @ Mention Dropdown
// =================================
function handleMentionInput() {
    const input = document.getElementById('messageInput');
    const cursorPos = input.selectionStart;
    const text = input.value.substring(0, cursorPos);

    // Only in public chat
    if (AppState.currentChatId !== 'all') { closeMentionDropdown(); return; }

    // Find last '@' before cursor that isn't preceded by a word char
    const match = text.match(/@([\w\u4e00-\u9fff]*)$/);
    if (match) {
        AppState.mentionStartPos = cursorPos - match[0].length;
        showMentionDropdown(match[1]);
    } else {
        closeMentionDropdown();
    }
}

function handleMentionKeydown(e) {
    if (!AppState.mentionActive) return;
    const items = document.querySelectorAll('.tg-mention-item');
    if (items.length === 0) return;

    if (e.key === 'ArrowDown') {
        e.preventDefault();
        items[AppState.mentionIndex]?.classList.remove('active');
        AppState.mentionIndex = (AppState.mentionIndex + 1) % items.length;
        items[AppState.mentionIndex]?.classList.add('active');
        items[AppState.mentionIndex]?.scrollIntoView({ block: 'nearest' });
    } else if (e.key === 'ArrowUp') {
        e.preventDefault();
        items[AppState.mentionIndex]?.classList.remove('active');
        AppState.mentionIndex = (AppState.mentionIndex - 1 + items.length) % items.length;
        items[AppState.mentionIndex]?.classList.add('active');
        items[AppState.mentionIndex]?.scrollIntoView({ block: 'nearest' });
    } else if (e.key === 'Escape') {
        e.preventDefault();
        closeMentionDropdown();
    }
}

function showMentionDropdown(query) {
    const dropdown = document.getElementById('mentionDropdown');
    const lowerQ = (query || '').toLowerCase();
    // Filter online users (exclude self)
    const matches = AppState.onlineUsers
        .filter(u => u !== AppState.localUsername && u.toLowerCase().includes(lowerQ));

    if (matches.length === 0) { closeMentionDropdown(); return; }

    AppState.mentionActive = true;
    AppState.mentionIndex = 0;

    dropdown.innerHTML = matches.map((name, i) => {
        const color = getAvatarColor(name);
        const letter = getAvatarLetter(name);
        return `<div class="tg-mention-item${i === 0 ? ' active' : ''}" data-name="${escapeHtml(name)}">
            <div class="tg-mention-item-avatar" style="background:${color}">${letter}</div>
            <div class="tg-mention-item-name">${escapeHtml(name)}</div>
        </div>`;
    }).join('');
    dropdown.style.display = 'block';

    // Click handlers
    dropdown.querySelectorAll('.tg-mention-item').forEach(item => {
        item.addEventListener('mousedown', (e) => {
            e.preventDefault(); // prevent blur
            insertMention(item.dataset.name);
        });
    });
}

function closeMentionDropdown() {
    const dropdown = document.getElementById('mentionDropdown');
    dropdown.style.display = 'none';
    dropdown.innerHTML = '';
    AppState.mentionActive = false;
    AppState.mentionStartPos = -1;
    AppState.mentionIndex = 0;
}

function insertMention(username) {
    const input = document.getElementById('messageInput');
    const before = input.value.substring(0, AppState.mentionStartPos);
    const after = input.value.substring(input.selectionStart);
    input.value = before + '@' + username + ' ' + after;
    const newPos = AppState.mentionStartPos + username.length + 2; // @name + space
    input.setSelectionRange(newPos, newPos);
    input.focus();
    closeMentionDropdown();
}

function renderMentions(escapedHtml) {
    // Build set of known names for validation
    const knownNames = new Set([
        AppState.localUsername,
        ...AppState.onlineUsers,
        ...AppState.knownPartners
    ]);
    return escapedHtml.replace(/@([\w\u4e00-\u9fff]+)/g, (match, name) => {
        if (knownNames.has(name)) {
            const cls = name === AppState.localUsername ? 'tg-mention self' : 'tg-mention';
            return `<span class="${cls}">${match}</span>`;
        }
        return match;
    });
}

// Wrap emoji characters in <span class="emoji"> for larger rendering
function wrapEmoji(html) {
    // Match emoji sequences: emoji presentation, keycap, flags, ZWJ sequences, modifiers
    const emojiRegex = /(?:\p{Emoji_Presentation}|\p{Emoji}\uFE0F)(?:\u200D(?:\p{Emoji_Presentation}|\p{Emoji}\uFE0F))*[\u{1F3FB}-\u{1F3FF}]?/gu;
    return html.replace(emojiRegex, m => `<span class="emoji">${m}</span>`);
}

// =================================
// Messages
// =================================
function sendMessage() {
    const input = document.getElementById('messageInput');
    let message = input.value.trim();

    if (message === '') {
        input.style.animation = 'shake 0.3s ease-in-out';
        setTimeout(() => { input.style.animation = ''; }, 300);
        return;
    }

    if (!AppState.currentChatId) {
        showToast('请先选择一个聊天', 'warning');
        return;
    }

    // If replying
    if (AppState.replyingTo) {
        sendReplyMessage(message);
        return;
    }

    // Private chat: prepend /to command
    if (AppState.currentChatId !== 'all') {
        if (AppState.blockedUsers.has(AppState.currentChatId)) {
            showToast(`请先解除对 ${AppState.currentChatId} 的屏蔽`, 'warning');
            return;
        }
        message = `/to ${AppState.currentChatId} ${message}`;
    }

    fetch('/send', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ message })
    })
    .then(response => {
        if (response.ok) {
            input.value = '';
            input.style.height = 'auto';
            cancelReply();
            input.focus();
            loadMessages(); // Immediately refresh to show sent message
        } else {
            throw new Error('发送失败');
        }
    })
    .catch(() => showToast('发送消息失败', 'error'));
}

function loadMessages() {
    fetch('/messages')
        .then(r => r.json())
        .then(data => {
            const msgs = data.messages || [];
            // Robust change detection: compare both length and last messageId
            // (length alone fails when buffer is at capacity — add+trim keeps count equal)
            const newLastId = msgs.length > 0 ? msgs[msgs.length - 1].messageId : '';
            const oldLastId = AppState.allMessages.length > 0 ? AppState.allMessages[AppState.allMessages.length - 1].messageId : '';
            if (msgs.length !== AppState.allMessages.length || newLastId !== oldLastId) {
                const oldLen = AppState.allMessages.length;
                AppState.allMessages = msgs;
                displayMessages();
                renderChatList();
                updateTitleBadge();

                // Notify for new messages
                if (oldLen > 0 && msgs.length > oldLen) {
                    const newMsgs = msgs.slice(oldLen);
                    newMsgs.forEach(msg => {
                        if (!msg.isOwn) {
                            notifyNewMessage(msg);
                        }
                    });
                }
            }
        })
        .catch(e => console.error('加载消息失败:', e));
}

function loadHistory() {
    if (!AppState.currentChatId) return;

    const url = new URL('/loadhistory', window.location.origin);
    url.searchParams.append('chatId', AppState.currentChatId);
    url.searchParams.append('limit', HISTORY_LIMIT);
    url.searchParams.append('offset', AppState.historyOffset);

    fetch(url)
        .then(r => r.json())
        .then(data => {
            if (data.messages && data.messages.length > 0) {
                const isInitialLoad = AppState.historyOffset === 0;
                // Deduplicate: only prepend history messages not already in allMessages
                const existingIds = new Set(
                    AppState.allMessages.map(m => m.messageId).filter(Boolean)
                );
                const newMsgs = data.messages.filter(
                    m => !m.messageId || !existingIds.has(m.messageId)
                );
                if (newMsgs.length > 0) {
                    AppState.allMessages = newMsgs.concat(AppState.allMessages);
                    displayMessages();
                    renderChatList();
                    if (isInitialLoad) {
                        // Defer to ensure DOM is fully rendered after history prepend
                        setTimeout(() => scrollToBottom(document.getElementById('messages')), 50);
                    }
                }
                AppState.historyOffset += data.messages.length;
            }
        })
        .catch(e => console.error('加载历史失败:', e));
}

function displayMessages() {
    const container = document.getElementById('messages');
    if (!container || !AppState.currentChatId) return;

    const shouldScroll = isScrolledToBottom(container);

    const filtered = AppState.allMessages.filter(msg => {
        if (AppState.currentChatId === 'all') return !msg.isPrivate;
        return msg.isPrivate &&
            ((msg.sender === AppState.currentChatId && msg.recipient === AppState.localUsername) ||
             (msg.isOwn && msg.recipient === AppState.currentChatId));
    });

    container.innerHTML = '';

    if (filtered.length === 0) {
        const placeholder = document.createElement('div');
        placeholder.className = 'tg-msg-placeholder';
        placeholder.textContent = AppState.currentChatId === 'all'
            ? '开始在公共聊天中发言吧！'
            : `开始与 ${AppState.currentChatId} 对话吧！`;
        container.appendChild(placeholder);
    } else {
        let lastDate = '';
        filtered.forEach(msg => {
            const msgDate = new Date(msg.timestamp);
            const dateStr = msgDate.toDateString();
            if (dateStr !== lastDate) {
                lastDate = dateStr;
                const sep = document.createElement('div');
                sep.className = 'tg-date-separator';
                sep.innerHTML = `<span class="tg-date-label">${formatDate(msgDate)}</span>`;
                container.appendChild(sep);
            }
            container.appendChild(createMessageElement(msg));
        });
    }

    // Re-insert file transfer cards for this chat
    renderFileTransferCards();

    if (shouldScroll) {
        scrollToBottom(container);
    }

    // Mark as read if this chat is active
    if (AppState.currentChatId) {
        markChatAsRead(AppState.currentChatId);
        renderChatList();
    }
}

function createMessageElement(msg) {
    const row = document.createElement('div');
    row.className = `tg-msg-row ${msg.isOwn ? 'own' : 'other'}`;
    row.dataset.messageId = msg.messageId || '';

    const isWisetalk = AppState.settings.skin === 'wisetalk';

    // WiseTalk: inline avatar next to bubble
    const inlineAvatar = document.createElement('div');
    inlineAvatar.className = 'tg-msg-avatar';
    const senderName = msg.isOwn ? AppState.localUsername : (msg.sender || '?');
    inlineAvatar.style.background = isWisetalk ? '#0089ff' : getAvatarColor(senderName);
    inlineAvatar.textContent = getAvatarLetter(senderName);

    // WiseTalk: name + time header above bubble
    const msgHeader = document.createElement('div');
    msgHeader.className = 'tg-msg-header';
    const ts = new Date(msg.timestamp);
    const headerName = msg.isOwn ? AppState.localUsername : (msg.sender || '');
    const headerTime = `${ts.getMonth()+1}/${ts.getDate()} ${formatTime(ts)}`;
    if (msg.isOwn) {
        msgHeader.innerHTML = `<span class="tg-msg-header-time">${headerTime}</span> <span class="tg-msg-header-name">${escapeHtml(headerName)}</span>`;
    } else {
        msgHeader.innerHTML = `<span class="tg-msg-header-name">${escapeHtml(headerName)}</span> <span class="tg-msg-header-time">${headerTime}</span>`;
    }

    // Bubble wrapper (header + bubble grouped together)
    const bubbleGroup = document.createElement('div');
    bubbleGroup.className = 'tg-bubble-group';
    bubbleGroup.appendChild(msgHeader);

    const bubble = document.createElement('div');
    const isEmojiOnly = msg.content && msg.content.startsWith('emoji:') && msg.messageType !== 'image' && msg.messageType !== 'file';
    bubble.className = 'tg-bubble' + (isEmojiOnly ? ' emoji-only' : '');

    // Sender name (only in public chat, other's messages) - Telegram style (inside bubble)
    if (!msg.isOwn && AppState.currentChatId === 'all' && msg.sender) {
        const senderEl = document.createElement('div');
        senderEl.className = 'tg-msg-sender';
        senderEl.style.color = getAvatarColor(msg.sender);
        senderEl.textContent = msg.sender;
        bubble.appendChild(senderEl);
    }

    // Reply quote
    if (msg.messageType === 'reply' && msg.replyToSender && msg.replyToContent) {
        const quote = document.createElement('div');
        quote.className = 'tg-reply-quote';
        quote.innerHTML = `
            <div class="tg-reply-quote-name">${escapeHtml(msg.replyToSender)}</div>
            <div class="tg-reply-quote-text">${escapeHtml(msg.replyToContent.substring(0, 80))}${msg.replyToContent.length > 80 ? '...' : ''}</div>
        `;
        bubble.appendChild(quote);
    }

    // Content
    if (msg.messageType === 'image' && (msg.fileUrl || msg.fileName)) {
        const imageUrl = msg.fileUrl || `/images/${msg.fileName}`;
        const img = document.createElement('img');
        img.className = 'tg-msg-image';
        img.src = imageUrl;
        img.alt = msg.fileName || '图片';
        img.loading = 'lazy';
        img.onclick = () => openImageModal(imageUrl);
        bubble.appendChild(img);
        if (msg.content && !msg.content.startsWith('发送了图片')) {
            const cap = document.createElement('div');
            cap.className = 'tg-msg-image-caption';
            cap.textContent = msg.content;
            bubble.appendChild(cap);
        }
    } else if (msg.messageType === 'file' && msg.fileName) {
        const fileDiv = document.createElement('div');
        fileDiv.className = 'tg-msg-file';
        fileDiv.innerHTML = `
            <div class="tg-msg-file-icon">${getFileIcon(msg.fileType || '')}</div>
            <div class="tg-msg-file-info">
                <div class="tg-msg-file-name">${escapeHtml(msg.fileName)}</div>
                <div class="tg-msg-file-size">${formatBytes(msg.fileSize || 0)}</div>
            </div>
        `;
        bubble.appendChild(fileDiv);
        // Inline file transfer actions (if fileId is available)
        if (msg.fileId) {
            const actionsDiv = document.createElement('div');
            actionsDiv.className = 'tg-msg-file-actions';
            actionsDiv.dataset.fileId = msg.fileId;
            const transfer = AppState.fileTransfers.find(t => t.fileId === msg.fileId);
            if (msg.isOwn) {
                renderSenderFileActions(actionsDiv, transfer, msg.fileId);
            } else {
                renderReceiverFileActions(actionsDiv, transfer, msg.fileId);
            }
            bubble.appendChild(actionsDiv);
        }
    } else if (msg.content && msg.content.startsWith('emoji:')) {
        // Legacy emoji:xxx format - show as [表情] text
        const text = document.createElement('div');
        text.className = 'tg-msg-text';
        text.textContent = '[表情]';
        bubble.appendChild(text);
    } else {
        const text = document.createElement('div');
        text.className = 'tg-msg-text';
        const cleanContent = (msg.content || '').replace(/[\r\n\s]+$/, '');
        text.innerHTML = wrapEmoji(renderMentions(escapeHtml(cleanContent)));
        // Time span appended inside the block div — naturally inline, no display hacks
        const metaEl = document.createElement('span');
        metaEl.className = 'tg-msg-meta';
        metaEl.innerHTML = `<span class="tg-msg-time">${formatTime(new Date(msg.timestamp))}</span>`;
        text.appendChild(metaEl);
        bubble.appendChild(text);
    }

    // Add time for non-text messages
    if (msg.messageType === 'image' || msg.messageType === 'file' || isEmojiOnly) {
        const timeEl = document.createElement('div');
        timeEl.style.cssText = 'text-align:right;margin-top:2px;';
        timeEl.innerHTML = `<span class="tg-msg-time">${formatTime(new Date(msg.timestamp))}</span>`;
        bubble.appendChild(timeEl);
    }

    bubbleGroup.appendChild(bubble);

    // Assemble row: [avatar] [bubbleGroup] or [bubbleGroup] [avatar]
    if (msg.isOwn) {
        row.appendChild(bubbleGroup);
        row.appendChild(inlineAvatar);
    } else {
        row.appendChild(inlineAvatar);
        row.appendChild(bubbleGroup);
    }

    // Reply button (on hover)
    if (!msg.isOwn && msg.messageId) {
        const replyBtn = document.createElement('button');
        replyBtn.className = 'tg-msg-reply-btn';
        replyBtn.textContent = '↩';
        replyBtn.title = '回复';
        replyBtn.onclick = (e) => {
            e.stopPropagation();
            replyToMessage(msg);
        };
        row.appendChild(replyBtn);
    }

    return row;
}

// =================================
// Reply
// =================================
function replyToMessage(msg) {
    AppState.replyingTo = {
        id: msg.messageId,
        sender: msg.sender || '未知',
        content: msg.content || ''
    };

    const indicator = document.getElementById('replyIndicator');
    document.getElementById('replyName').textContent = AppState.replyingTo.sender;
    document.getElementById('replyText').textContent = AppState.replyingTo.content.substring(0, 60) + (AppState.replyingTo.content.length > 60 ? '...' : '');
    indicator.style.display = 'flex';

    document.getElementById('messageInput').focus();
}

function cancelReply() {
    AppState.replyingTo = null;
    document.getElementById('replyIndicator').style.display = 'none';
}

function sendReplyMessage(replyContent) {
    if (!AppState.replyingTo) return;

    const targetName = AppState.currentChatId === 'all' ? 'all' : AppState.currentChatId;

    fetch('/sendreply', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
            targetName,
            replyContent,
            originalMsgId: AppState.replyingTo.id,
            originalSender: AppState.replyingTo.sender,
            originalContent: AppState.replyingTo.content
        })
    })
    .then(r => {
        if (r.ok) {
            document.getElementById('messageInput').value = '';
            cancelReply();
        } else {
            throw new Error('发送失败');
        }
    })
    .catch(() => showToast('回复发送失败', 'error'));
}

// =================================
// Users
// =================================
function loadUsers() {
    fetch('/users')
        .then(r => r.json())
        .then(data => {
            const users = data.users || [];
            const selfUser = users.find(u => u.includes('(自己)'));
            if (selfUser) {
                AppState.localUsername = selfUser.replace(' (自己)', '').trim();
            }

            const onlineNames = users
                .filter(u => !u.includes('(自己)'))
                .map(u => u.split(' ')[0]);

            // Detect online/offline changes (browser mode only; Wails uses events)
            if (!AppState.isWails && !AppState.isFirstUserLoad) {
                const { justOnline, justOffline } = detectOnlineChanges(onlineNames, AppState.previousOnlineUsers);

                justOnline.forEach(name => {
                    insertSystemMessage(`${name} 已上线`);
                    if (AppState.settings.onlineNotify) {
                        showToast(`${name} 已上线`, 'info');
                    }
                });
                justOffline.forEach(name => {
                    insertSystemMessage(`${name} 已离线`);
                });
            }
            AppState.isFirstUserLoad = false;

            AppState.previousOnlineUsers = [...AppState.onlineUsers];
            AppState.onlineUsers = onlineNames;

            // Update conversation header if needed
            if (AppState.currentChatId) {
                updateConversationHeader();
            }

            renderChatList();
            updateUserSelect();
        })
        .catch(e => console.error('加载用户失败:', e));
}

function loadChatPartners() {
    fetch('/chatpartners')
        .then(r => r.json())
        .then(data => {
            AppState.knownPartners = data.partners || [];
            renderChatList();
        })
        .catch(e => console.error('加载聊天伙伴失败:', e));
}

function detectOnlineChanges(newUsers, oldUsers) {
    // Skip on first load (previousOnlineUsers is empty)
    if (oldUsers.length === 0 && AppState.allMessages.length === 0) {
        return { justOnline: [], justOffline: [] };
    }
    const newSet = new Set(newUsers);
    const oldSet = new Set(oldUsers);
    return {
        justOnline: newUsers.filter(u => !oldSet.has(u) && u !== AppState.localUsername),
        justOffline: oldUsers.filter(u => !newSet.has(u) && u !== AppState.localUsername),
    };
}

function insertSystemMessage(text, targetChatId) {
    // Default: public chat; if targetChatId provided, show in that chat
    const chatId = targetChatId || 'all';
    if (AppState.currentChatId === chatId) {
        const container = document.getElementById('messages');
        if (container) {
            const sysMsg = document.createElement('div');
            sysMsg.className = 'tg-system-msg';
            sysMsg.innerHTML = `<span>${escapeHtml(text).replace(/\n/g, '<br>')}</span>`;
            container.appendChild(sysMsg);
            scrollToBottom(container);
        }
    }
}

// =================================
// Blocked Users
// =================================
async function loadBlockedUsers() {
    try {
        const r = await fetch('/acl');
        const data = await r.json();
        AppState.blockedUsers = new Set(data.blocked || []);
    } catch {
        AppState.blockedUsers = new Set();
    }
}

function blockUser(username) {
    const isBlocked = AppState.blockedUsers.has(username);
    const command = isBlocked ? `/unblock ${username}` : `/block ${username}`;
    const action = isBlocked ? '解除屏蔽' : '屏蔽';

    fetch('/send', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ message: command })
    })
    .then(r => {
        if (r.ok) {
            loadBlockedUsers().then(() => {
                loadUsers();
                updateConversationHeader();
                showToast(`${action} ${username} 成功`, 'success');
            });
        } else {
            throw new Error(`${action}失败`);
        }
    })
    .catch(() => showToast(`${action}失败`, 'error'));
}

// =================================
// Notifications
// =================================
function notifyNewMessage(msg) {
    if (!AppState.settings.msgNotify) return;

    const chatId = msg.isPrivate ? msg.sender : 'all';

    // Don't notify for current active chat
    if (chatId === AppState.currentChatId && document.hasFocus()) return;

    const senderName = msg.sender || '未知';
    const preview = getMessagePreview(msg).substring(0, 60);

    // Browser notification
    if ('Notification' in window && Notification.permission === 'granted') {
        try {
            new Notification(`LS Messager - ${senderName}`, {
                body: preview,
                icon: 'data:image/svg+xml,<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><text y=".9em" font-size="90">💬</text></svg>',
                tag: `lanshare-${chatId}`,
            });
        } catch (e) {
            // Notification not supported in some environments
        }
    }

    // Title flash
    startTitleFlash();
}

function startTitleFlash() {
    if (AppState.titleFlashInterval) return;
    let show = true;
    AppState.titleFlashInterval = setInterval(() => {
        const total = getTotalUnread();
        if (total === 0 || document.hasFocus()) {
            stopTitleFlash();
            return;
        }
        document.title = show ? `💬 新消息 - LS Messager` : `(${total}) ${AppState.originalTitle}`;
        show = !show;
    }, 1000);
}

function stopTitleFlash() {
    if (AppState.titleFlashInterval) {
        clearInterval(AppState.titleFlashInterval);
        AppState.titleFlashInterval = null;
        updateTitleBadge();
    }
}

// Stop flash when window gains focus
window.addEventListener('focus', () => {
    stopTitleFlash();
    if (AppState.currentChatId) {
        markChatAsRead(AppState.currentChatId);
        renderChatList();
    }
});

// =================================
// Toast Notifications
// =================================
function showToast(message, type = 'info', duration = 3000) {
    const container = document.getElementById('toastContainer');
    const toast = document.createElement('div');
    toast.className = `tg-toast ${type}`;
    toast.innerText = message;
    container.appendChild(toast);

    setTimeout(() => {
        toast.classList.add('removing');
        setTimeout(() => toast.remove(), 300);
    }, duration);
}

// Legacy compatibility: showNotification -> showToast
function showNotification(message, type) {
    showToast(message, type);
}

// =================================
// Banner Notifications (top of screen, pushes content down)
// =================================
/**
 * Show a banner at the top of the interface.
 * @param {string} message - Text to display
 * @param {string} type - 'info' | 'success' | 'warning' | 'error'
 * @param {Object} [options]
 * @param {Array<{label:string, class:string, onClick:Function}>} [options.actions] - Buttons
 * @param {boolean} [options.closable=true] - Show close button
 * @param {number} [options.duration=0] - Auto-dismiss ms (0 = manual close only)
 * @param {string} [options.id] - Unique ID to prevent duplicates
 * @returns {HTMLElement} The banner element
 */
function showBanner(message, type = 'info', options = {}) {
    const container = document.getElementById('bannerContainer');
    const { actions, closable = true, duration = 0, id } = options;

    // Prevent duplicate banners with same id
    if (id) {
        const existing = container.querySelector(`[data-banner-id="${id}"]`);
        if (existing) {
            existing.querySelector('.tg-banner-text').textContent = message;
            return existing;
        }
    }

    const banner = document.createElement('div');
    banner.className = `tg-banner ${type}`;
    if (id) banner.dataset.bannerId = id;

    const textEl = document.createElement('span');
    textEl.className = 'tg-banner-text';
    textEl.textContent = message;
    banner.appendChild(textEl);

    const removeBanner = () => {
        banner.classList.add('removing');
        setTimeout(() => banner.remove(), 250);
    };

    if (actions && actions.length > 0) {
        const actionsEl = document.createElement('div');
        actionsEl.className = 'tg-banner-actions';
        actions.forEach(act => {
            const btn = document.createElement('button');
            btn.className = `tg-banner-btn ${act.class || 'secondary'}`;
            btn.textContent = act.label;
            btn.addEventListener('click', () => {
                if (act.onClick) act.onClick(banner, removeBanner);
            });
            actionsEl.appendChild(btn);
        });
        banner.appendChild(actionsEl);
    }

    if (closable) {
        const closeBtn = document.createElement('button');
        closeBtn.className = 'tg-banner-close';
        closeBtn.textContent = '✕';
        closeBtn.addEventListener('click', removeBanner);
        banner.appendChild(closeBtn);
    }

    container.appendChild(banner);

    if (duration > 0) {
        setTimeout(removeBanner, duration);
    }

    return banner;
}

function removeBannerById(id) {
    const container = document.getElementById('bannerContainer');
    const existing = container.querySelector(`[data-banner-id="${id}"]`);
    if (existing) {
        existing.classList.add('removing');
        setTimeout(() => existing.remove(), 250);
    }
}

// =================================
// Emoji Picker
// =================================
function initEmojiPicker() {
    const btn = document.getElementById('emojiButton');
    const picker = document.getElementById('emojiPicker');

    btn.addEventListener('click', (e) => {
        e.stopPropagation();
        const isVisible = picker.style.display === 'flex';
        picker.style.display = isVisible ? 'none' : 'flex';
    });

    document.addEventListener('click', (e) => {
        if (!picker.contains(e.target) && !btn.contains(e.target)) {
            picker.style.display = 'none';
        }
    });
}

// Comprehensive UTF-8 emoji data organized by category
const EMOJI_CATEGORIES = [
    { icon: '😀', name: '表情', emojis: [
        '😀','😃','😄','😁','😆','😅','🤣','😂','🙂','🙃','🫠','😉','😊','😇','🥰','😍',
        '🤩','😘','😗','😚','😙','🥲','😋','😛','😜','🤪','😝','🤑','🤗','🤭','🫢','🫣',
        '🤫','🤔','🫡','🤐','🤨','😐','😑','😶','🫥','😏','😒','🙄','😬','🤥','🫨','😶‍🌫️',
        '😌','😔','😪','🤤','😴','😷','🤒','🤕','🤢','🤮','🥵','🥶','🥴','😵','😵‍💫','🤯',
        '🤠','🥳','🥸','😎','🤓','🧐','😕','🫤','😟','🙁','☹️','😮','😯','😲','😳','🥺','🥹',
        '😦','😧','😨','😰','😥','😢','😭','😱','😖','😣','😞','😓','😩','😫','🥱',
        '😤','😡','😠','🤬','😈','👿','💀','☠️','💩','🤡','👹','👺','👻','👽','👾','🤖',
        '😺','😸','😹','😻','😼','😽','🙀','😿','😾','🙈','🙉','🙊',
        '💋','👋','🤚','🖐️','✋','🖖','👌','✌️','🤞','🤟','🤘','🤙','👍','👎','✊','👊','👏','🙌','👐','🤝','🙏',
    ]},
    { icon: '👋', name: '手势', emojis: [
        '👋','🤚','🖐️','✋','🖖','🫱','🫲','🫳','🫴','🫷','🫸','👌','🤌','🤏','✌️','🤞',
        '🫰','🤟','🤘','🤙','👈','👉','👆','🖕','👇','☝️','🫵','👍','👎','✊','👊','🤛',
        '🤜','👏','🙌','🫶','👐','🤲','🤝','🙏','✍️','💅','🤳','💪','🦾','🦿','🦵','🦶',
        '👂','🦻','👃','👀','👁️','👅','👄','🫦','🧠','🦷','🦴','💋','🫀','🫁',
    ]},
    { icon: '👤', name: '人物', emojis: [
        '👤','👥','🗣️','👶','🧒','👦','👧','🧑','👱','👨','🧔','👩','🧓','👴','👵',
        '🙍','🙎','🙅','🙆','💁','🙋','🧏','🙇','🤦','🤷',
        '👮','🕵️','💂','🥷','👷','🫅','🤴','👸','👳','👲','🧕','🤵','👰','🤰','🫃','🫄','🤱',
        '👼','🎅','🤶','🦸','🦹','🧙','🧚','🧛','🧜','🧝','🧞','🧟','🧌','💆','💇',
        '🚶','🧍','🧎','🏃','💃','🕺','👯','🧖','🧗','🤸','⛹️','🏋️','🚴','🚵','🤼','🤽','🤾','🤺',
        '🏇','⛷️','🏂','🏌️','🏄','🚣','🏊','🤿',
        '👫','👬','👭','💏','💑','👪','👨‍👩‍👦','👨‍👩‍👧','👨‍👩‍👧‍👦','👨‍👩‍👦‍👦','👨‍👩‍👧‍👧',
    ]},
    { icon: '❤️', name: '心/符号', emojis: [
        '❤️','🧡','💛','💚','💙','💜','🖤','🤍','🤎','❤️‍🔥','❤️‍🩹','💔','❣️','💕','💞','💓',
        '💗','💖','💘','💝','💟','♥️','🩷','🩵','🩶',
        '✨','🔥','💥','💫','💦','💤','⚡','🌟','⭐','🌀','💯','✅','❌',
        '❓','❗','‼️','⁉️','❔','❕','⭕','❎','➕','➖','➗','✖️',
        '☮️','✝️','☪️','🕉️','☸️','✡️','🔯','🕎','☯️','☦️','🛐',
        '⛎','♈','♉','♊','♋','♌','♍','♎','♏','♐','♑','♒','♓',
        '🆔','⚛️','🔀','🔁','🔂','▶️','⏩','⏭️','⏯️','◀️','⏪','⏮️','🔼','⏫','🔽','⏬',
        '⏸️','⏹️','⏺️','⏏️','🎦','🔅','🔆','📶','🔰','♻️','🔱','📛',
        '☑️','✔️','💲','💱','©️','®️','™️','〰️',
        '♠️','♣️','♥️','♦️','🃏','🀄','🎴',
        '🔴','🟠','🟡','🟢','🔵','🟣','⚫','⚪','🟤','🔶','🔷','🔸','🔹','🔺','🔻',
        '💠','🔘','🔳','🔲','⬛','⬜','🟥','🟧','🟨','🟩','🟦','🟪','🟫',
    ]},
    { icon: '🐶', name: '动物', emojis: [
        '🐶','🐕','🦮','🐕‍🦺','🐩','🐺','🦊','🦝','🐱','🐈','🐈‍⬛','🦁','🐯','🐅','🐆','🐴',
        '🫎','🫏','🐎','🦄','🦓','🦌','🦬','🐮','🐂','🐃','🐄','🐷','🐖','🐗','🐽','🐏',
        '🐑','🐐','🐪','🐫','🦙','🦒','🐘','🦣','🦏','🦛','🐭','🐁','🐀','🐹','🐰','🐇',
        '🐿️','🦫','🦔','🦇','🐻','🐻‍❄️','🐨','🐼','🦥','🦦','🦨','🦘','🦡',
        '🐾','🦃','🐔','🐓','🐣','🐤','🐥','🐦','🐧','🕊️','🦅','🦆','🦢','🦉','🦤',
        '🪶','🦩','🦚','🦜','🪽','🪿','🐦‍⬛','🐸','🐊','🐢','🦎','🐍','🐲','🐉','🦕','🦖',
        '🐳','🐋','🐬','🦭','🐟','🐠','🐡','🦈','🐙','🐚','🪸','🪼','🐌','🦋','🐛','🐜',
        '🐝','🪲','🐞','🦗','🪳','🕷️','🕸️','🦂','🦟','🪰','🪱','🦠',
    ]},
    { icon: '🌸', name: '自然', emojis: [
        '💐','🌸','💮','🪷','🏵️','🌹','🥀','🌺','🌻','🌼','🌷','🪻','🌱','🪴','🌲','🌳',
        '🌴','🌵','🌾','🌿','☘️','🍀','🍁','🍂','🍃','🪹','🪺','🍄',
        '🌍','🌎','🌏','🌐','🗺️','🌑','🌒','🌓','🌔','🌕','🌖','🌗','🌘','🌙','🌚','🌛',
        '🌜','☀️','🌝','🌞','⭐','🌟','🌠','☁️','⛅','⛈️','🌤️','🌥️','🌦️','🌧️','🌨️','🌩️',
        '🌪️','🌫️','🌬️','🌈','☂️','☔','⚡','❄️','☃️','⛄','☄️','🔥','💧','🌊',
    ]},
    { icon: '🍔', name: '食物', emojis: [
        '🍇','🍈','🍉','🍊','🍋','🍋‍🟩','🍌','🍍','🥭','🍎','🍏','🍐','🍑','🍒','🍓','🫐','🥝',
        '🍅','🫒','🥥','🥑','🍆','🥔','🥕','🌽','🌶️','🫑','🥒','🥬','🥦','🧄','🧅','🥜',
        '🫘','🌰','🫚','🫛','🍞','🥐','🥖','🫓','🥨','🥯','🥞','🧇','🧀','🍖','🍗','🥩',
        '🥓','🍔','🍟','🍕','🌭','🥪','🌮','🌯','🫔','🥙','🧆','🥚','🍳','🥘','🍲','🫕',
        '🥣','🥗','🍿','🧈','🧂','🥫','🍱','🍘','🍙','🍚','🍛','🍜','🍝','🍠','🍢','🍣',
        '🍤','🍥','🥮','🍡','🥟','🥠','🥡','🦀','🦞','🦐','🦑','🦪',
        '🍦','🍧','🍨','🍩','🍪','🎂','🍰','🧁','🥧','🍫','🍬','🍭','🍮','🍯',
        '🍼','🥛','☕','🫖','🍵','🍶','🍾','🍷','🍸','🍹','🍺','🍻','🥂','🥃','🫗',
        '🥤','🧋','🧃','🧉','🧊',
    ]},
    { icon: '⚽', name: '运动', emojis: [
        '⚽','🏀','🏈','⚾','🥎','🎾','🏐','🏉','🥏','🎱','🪀','🏓','🏸','🏒','🏑','🥍',
        '🏏','🪃','🥅','⛳','🪁','🏹','🎣','🤿','🥊','🥋','🎽','🛹','🛼','🛷','⛸️','🥌',
        '🎿','⛷️','🏂','🪂','🏋️','🤼','🤸','🤺','⛹️','🤾','🏌️','🏇','🧘','🏄','🏊','🤽',
        '🚣','🧗','🚵','🚴','🏆','🥇','🥈','🥉','🏅','🎖️','🏵️','🎗️','🎫','🎟️','🎪',
        '🎭','🎨','🎬','🎤','🎧','🎼','🎹','🥁','🪘','🎷','🎺','🪗','🎸','🪕','🎻','🪈',
        '🎲','♟️','🎯','🎳','🎮','🕹️','🎰',
    ]},
    { icon: '🚗', name: '交通', emojis: [
        '🚗','🚕','🚙','🚌','🚎','🏎️','🚓','🚑','🚒','🚐','🛻','🚚','🚛','🚜','🏍️','🛵',
        '🦽','🦼','🛺','🚲','🛴','🛹','🛼','🚏','🛣️','🛤️','🛞','⛽','🚨','🚥','🚦',
        '🛑','🚧','⚓','🛟','⛵','🛶','🚤','🛳️','⛴️','🛥️','🚢','✈️','🛩️','🛫','🛬','🪂',
        '💺','🚁','🚟','🚠','🚡','🛰️','🚀','🛸','🎆','🎇','🎑','🗼','🗽','🗿','🏰','🏯',
        '🏟️','🎡','🎢','🎠','⛲','⛱️','🏖️','🏝️','🏜️','🌋','⛰️','🏔️','🗻','🏕️','⛺','🛖',
        '🏠','🏡','🏘️','🏚️','🏗️','🏢','🏬','🏣','🏤','🏥','🏦','🏨','🏪','🏫','🏩','💒',
        '🏛️','⛪','🕌','🕍','🛕','🕋','⛩️',
    ]},
    { icon: '💡', name: '物品', emojis: [
        '⌚','📱','📲','💻','⌨️','🖥️','🖨️','🖱️','🖲️','🕹️','🗜️','💽','💾','💿','📀','📼',
        '📷','📸','📹','🎥','📽️','🎞️','📞','☎️','📟','📠','📺','📻','🎙️','🎚️','🎛️','🧭',
        '⏱️','⏲️','⏰','🕰️','⌛','⏳','📡','🔋','🪫','🔌','💡','🔦','🕯️','🪔',
        '🧯','🗑️','🛢️','💸','💵','💴','💶','💷','🪙','💰','💳','🪪','💎','⚖️','🪜','🧰',
        '🪛','🔧','🔨','⚒️','🛠️','⛏️','🪚','🔩','⚙️','🪤','🧲','🔫','💣','🧨','🪓','🔪',
        '🗡️','⚔️','🛡️','🚬','⚰️','🪦','⚱️','🏺','🔮','📿','🧿','🪬','💈','⚗️','🔭','🔬',
        '🕳️','🩹','🩺','🩻','🩼','💊','💉','🩸','🧬','🦠','🧫','🧪','🌡️','🧹','🪠','🧺',
        '🧻','🚽','🚰','🚿','🛁','🛀','🧼','🪥','🪒','🧽','🪣','🧴','🛎️','🔑','🗝️','🚪',
        '🪑','🛋️','🛏️','🛌','🧸','🪆','🖼️','🪞','🪟','🛍️','🛒','🎁','🎈','🎏','🎀','🪄',
        '🪅','🎊','🎉','🎎','🏮','🎐','🧧','✉️','📩','📨','📧','💌','📥','📤','📦','🏷️',
        '🪧','📪','📫','📬','📭','📮','📯','📜','📃','📄','📑','🧾','📊','📈','📉','🗒️',
        '🗓️','📆','📅','🗑️','📇','🗃️','🗳️','🗄️','📋','📁','📂','🗂️','🗞️','📰','📓','📔',
        '📒','📕','📗','📘','📙','📚','📖','🔖','🧷','🔗','📎','🖇️','📐','📏','🧮','📌',
        '📍','✂️','🖊️','🖋️','✒️','🖌️','🖍️','📝','✏️','🔍','🔎','🔏','🔐','🔒','🔓',
    ]},
];

function loadGifEmojis() {
    // Flatten categories into gifEmojis for backward compat
    AppState.gifEmojis = [];
    EMOJI_CATEGORIES.forEach(cat => {
        cat.emojis.forEach(ch => {
            AppState.gifEmojis.push({ char: ch, name: cat.name, type: 'native' });
        });
    });
    return Promise.resolve();
}

function createEmojiGrid() {
    const picker = document.getElementById('emojiPicker');
    picker.innerHTML = '';

    // Category tabs
    const tabs = document.createElement('div');
    tabs.className = 'tg-emoji-tabs';
    EMOJI_CATEGORIES.forEach((cat, idx) => {
        const tab = document.createElement('button');
        tab.className = 'tg-emoji-tab' + (idx === 0 ? ' active' : '');
        tab.textContent = cat.icon;
        tab.title = cat.name;
        tab.addEventListener('click', () => {
            tabs.querySelectorAll('.tg-emoji-tab').forEach(t => t.classList.remove('active'));
            tab.classList.add('active');
            showEmojiCategory(idx);
        });
        tabs.appendChild(tab);
    });
    picker.appendChild(tabs);

    // Grid container
    const grid = document.createElement('div');
    grid.className = 'tg-emoji-grid';
    grid.id = 'emojiGrid';
    picker.appendChild(grid);

    showEmojiCategory(0);
}

function showEmojiCategory(catIdx) {
    const grid = document.getElementById('emojiGrid');
    if (!grid) return;
    grid.innerHTML = '';
    const cat = EMOJI_CATEGORIES[catIdx];
    if (!cat) return;
    cat.emojis.forEach(ch => {
        const item = document.createElement('div');
        item.className = 'tg-emoji-item';
        item.textContent = ch;
        item.addEventListener('click', () => {
            const input = document.getElementById('messageInput');
            const start = input.selectionStart;
            const end = input.selectionEnd;
            const text = input.value;
            input.value = text.substring(0, start) + ch + text.substring(end);
            input.selectionStart = input.selectionEnd = start + ch.length;
            input.focus();
            document.getElementById('emojiPicker').style.display = 'none';
        });
        grid.appendChild(item);
    });
}

// sendEmojiMessage is no longer used - native emoji chars are inserted directly into the input field

function showEmojiAlert(message) {
    const dialog = document.getElementById('emoji-alert-dialog');
    document.getElementById('alert-message').textContent = message;
    dialog.style.display = 'flex';
    setTimeout(() => dialog.classList.add('visible'), 10);

    const hide = () => {
        dialog.classList.remove('visible');
        setTimeout(() => dialog.style.display = 'none', 200);
    };

    document.getElementById('alert-ok-btn').onclick = hide;
    dialog.onclick = (e) => { if (e.target === dialog) hide(); };
}

// In-app confirm dialog (replaces browser confirm())
function showConfirm(message) {
    return new Promise((resolve) => {
        const dialog = document.getElementById('confirmDialog');
        document.getElementById('confirmMessage').textContent = message;
        dialog.style.display = 'flex';
        setTimeout(() => dialog.classList.add('visible'), 10);

        const hide = (result) => {
            dialog.classList.remove('visible');
            setTimeout(() => dialog.style.display = 'none', 200);
            resolve(result);
        };

        document.getElementById('confirmOkBtn').onclick = () => hide(true);
        document.getElementById('confirmCancelBtn').onclick = () => hide(false);
        dialog.onclick = (e) => { if (e.target === dialog) hide(false); };
    });
}

// =================================
// File Transfer
// =================================
function initFileTransfer() {
    const fileInput = document.getElementById('fileInput');
    const imageInput = document.getElementById('imageInput');

    fileInput.addEventListener('change', (e) => {
        if (e.target.files.length > 0) {
            const file = e.target.files[0];
            // Auto-send if we're in a private chat
            if (AppState.currentChatId && AppState.currentChatId !== 'all') {
                sendDroppedFile(file); // handles Wails path vs browser base64
                fileInput.value = '';
            } else {
                AppState.selectedFile = file;
                document.getElementById('fileNameDisplay').textContent = file.name;
                document.getElementById('file-transfer-controls').style.display = 'flex';
                updateSendFileButton();
            }
        }
    });

    imageInput.addEventListener('change', (e) => {
        if (e.target.files.length > 0) {
            sendImage(e.target.files[0]);
        }
    });

    document.getElementById('fileTargetUser').addEventListener('change', updateSendFileButton);
}

function initAttachMenu() {
    const attachBtn = document.getElementById('attachBtn');
    const menu = document.getElementById('attachMenu');

    attachBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        menu.style.display = menu.style.display === 'none' ? 'block' : 'none';
    });

    document.getElementById('attachImageBtn').addEventListener('click', () => {
        document.getElementById('imageInput').click();
        menu.style.display = 'none';
    });

    document.getElementById('attachFileBtn').addEventListener('click', () => {
        menu.style.display = 'none';
        if (!AppState.currentChatId || AppState.currentChatId === 'all') {
            showToast('文件传输需要在私聊中使用', 'warning');
            return;
        }
        if (!AppState.onlineUsers.includes(AppState.currentChatId)) {
            showToast('对方不在线，无法发送文件', 'warning');
            return;
        }
        // Wails mode: use Go binding for file dialog + direct disk transfer (no base64)
        if (AppState.isWails && window.go && window.go.main && window.go.main.DesktopApp) {
            window.go.main.DesktopApp.SendFile(AppState.currentChatId)
                .then(result => {
                    if (result && result.fileId) {
                        postFileMsgAfterSend(AppState.currentChatId, result.fileName,
                            parseInt(result.fileSize) || 0, '', result.fileId);
                    }
                })
                .catch(err => {
                    if (err && !err.toString().includes('cancelled')) {
                        showToast('文件发送失败: ' + err, 'error');
                    }
                });
        } else {
            document.getElementById('fileInput').click();
        }
    });

    document.addEventListener('click', (e) => {
        if (!menu.contains(e.target) && !attachBtn.contains(e.target)) {
            menu.style.display = 'none';
        }
    });
}

function updateUserSelect() {
    const select = document.getElementById('fileTargetUser');
    const current = select.value;
    while (select.options.length > 1) select.remove(1);
    AppState.onlineUsers.forEach(u => {
        const opt = document.createElement('option');
        opt.value = u;
        opt.textContent = u;
        select.appendChild(opt);
    });
    select.value = current;
}

function updateSendFileButton() {
    document.getElementById('sendFileBtn').disabled =
        !AppState.selectedFile || !document.getElementById('fileTargetUser').value;
}

function sendFile() {
    // Use currentChatId if available, otherwise fallback to dropdown
    const target = (AppState.currentChatId && AppState.currentChatId !== 'all')
        ? AppState.currentChatId
        : document.getElementById('fileTargetUser').value;

    if (!AppState.selectedFile || !target) {
        showToast('请选择文件和目标用户', 'error');
        return;
    }

    const fileRef = AppState.selectedFile;
    uploadFileAsBase64(fileRef, target)
        .then(data => {
            postFileMsgAfterSend(target, fileRef.name,
                fileRef.size, fileRef.type || '', data.fileId || '');
            cancelFileSelection();
        })
        .catch(() => showToast('文件发送失败', 'error'));
}

// Post /sendfilemsg to create the chat message after a file transfer is initiated
function postFileMsgAfterSend(targetName, fileName, fileSize, fileType, fileId) {
    fetch('/sendfilemsg', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ targetName, fileName, fileSize, fileType, fileId })
    });
}

// Read file as base64 and POST as JSON (browser fallback only)
// Resolves with { fileId } on success
function uploadFileAsBase64(file, targetName) {
    return new Promise((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => {
            const base64 = reader.result.split(',')[1];
            fetch('/sendfile', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    file: base64,
                    fileName: file.name,
                    targetName: targetName
                })
            })
                .then(r => {
                    if (!r.ok) { reject(); return; }
                    return r.json();
                })
                .then(data => { if (data) resolve(data); })
                .catch(reject);
        };
        reader.onerror = reject;
        reader.readAsDataURL(file);
    });
}

function cancelFileSelection() {
    AppState.selectedFile = null;
    document.getElementById('fileInput').value = '';
    document.getElementById('file-transfer-controls').style.display = 'none';
    document.getElementById('fileNameDisplay').textContent = '';
    updateSendFileButton();
}

let _fileTransferPollTimer = null;

function startFileTransferPolling() {
    // Adaptive polling: 500ms during active transfers, 3s otherwise
    const hasActive = AppState.fileTransfers.some(t =>
        t.status === 'transferring' || t.status === 'pending');
    const interval = hasActive ? 500 : 3000;

    clearTimeout(_fileTransferPollTimer);
    _fileTransferPollTimer = setTimeout(() => {
        loadFileTransfers();
    }, interval);
}

function loadFileTransfers() {
    fetch('/filetransfers')
        .then(r => r.json())
        .then(data => {
            const transfers = data.transfers || [];
            AppState.fileTransfers = transfers;

            // Pending receive → inline card in sender's chat
            transfers.filter(t => t.direction === 'receive' && t.status === 'pending').forEach(pending => {
                if (!AppState.shownPendingTransfers.has(pending.fileId)) {
                    AppState.shownPendingTransfers.add(pending.fileId);
                    insertFileRequestCard(pending);
                }
            });

            // Failed notifications (inline UI shows status, system message for visibility)
            transfers.filter(t => t.status === 'failed').forEach(t => {
                if (!AppState.shownFailedTransfers.has(t.fileId)) {
                    insertSystemMessage(`文件传输失败: ${t.fileName}`, t.peerName);
                    AppState.shownFailedTransfers.add(t.fileId);
                }
            });

            // Completed → tracked for inline button updates
            transfers.filter(t => t.status === 'completed').forEach(t => {
                if (!AppState.shownCompletedTransfers.has(t.fileId)) {
                    AppState.shownCompletedTransfers.add(t.fileId);
                }
            });

            // Update existing pending cards that have been accepted/rejected
            renderFileCardUpdates(transfers);

            // Update inline file actions in message bubbles
            updateInlineFileActions();

            // Schedule next poll (adaptive interval)
            startFileTransferPolling();
        })
        .catch(e => {
            console.error('加载传输失败:', e);
            startFileTransferPolling();
        });
}

// Re-insert file transfer cards for the current chat after displayMessages() clears the DOM
function renderFileTransferCards() {
    const container = document.getElementById('messages');
    if (!container || !AppState.currentChatId) return;

    const chatPeer = AppState.currentChatId;
    const transfers = AppState.fileTransfers.filter(t => t.peerName === chatPeer);

    transfers.forEach(t => {
        // Skip if card already in DOM
        if (container.querySelector(`[data-file-id="${t.fileId}"]`)) return;

        if (t.direction === 'receive') {
            if (t.status === 'pending') {
                insertFileRequestCard(t);
            } else if (t.status === 'transferring') {
                // Transferring card (accepted, in progress)
                const pct = t.fileSize > 0 ? (t.progress / t.fileSize * 100) : 0;
                const progressText = `${pct.toFixed(0)}% · ${formatBytes(t.progress)}/${formatBytes(t.fileSize)}${t.speed > 0 ? ' · ' + formatSpeed(t.speed) : ''}`;
                const card = document.createElement('div');
                card.className = 'tg-file-card';
                card.dataset.fileId = t.fileId;
                card.innerHTML = `
                    <div class="tg-file-card-header">
                        <span class="tg-file-card-icon">📥</span>
                        <span class="tg-file-card-title">文件接收中</span>
                    </div>
                    <div class="tg-file-card-body">
                        <div class="tg-file-card-name">${escapeHtml(t.fileName)}</div>
                        <div class="tg-file-card-size">${formatBytes(t.fileSize)}</div>
                    </div>
                    <div class="tg-file-card-actions">
                        <span class="tg-file-card-status transferring">${progressText}</span>
                    </div>
                `;
                container.appendChild(card);
            } else if (t.status === 'failed') {
                const card = document.createElement('div');
                card.className = 'tg-file-card failed';
                card.dataset.fileId = t.fileId;
                card.innerHTML = `
                    <div class="tg-file-card-header">
                        <span class="tg-file-card-icon">❌</span>
                        <span class="tg-file-card-title">文件传输失败</span>
                    </div>
                    <div class="tg-file-card-body">
                        <div class="tg-file-card-name">${escapeHtml(t.fileName)}</div>
                        <div class="tg-file-card-size">${formatBytes(t.fileSize)}</div>
                    </div>
                    <div class="tg-file-card-actions">
                        <span class="tg-file-card-status failed">传输失败</span>
                    </div>
                `;
                container.appendChild(card);
            }
        } else if (t.direction === 'send') {
            // Show sent file transfer status
            const card = document.createElement('div');
            card.className = 'tg-file-card' + (t.status === 'completed' ? ' completed' : t.status === 'failed' ? ' failed' : '');
            card.dataset.fileId = t.fileId;
            const icon = t.status === 'completed' ? '✅' : t.status === 'failed' ? '❌' : '📤';
            const title = t.status === 'completed' ? '文件发送完成' : t.status === 'failed' ? '文件发送失败' : '文件发送中';
            let statusHtml = '';
            if (t.status === 'transferring') {
                const pct = t.fileSize > 0 ? (t.progress / t.fileSize * 100) : 0;
                statusHtml = `<span class="tg-file-card-status transferring">${pct.toFixed(0)}% · ${formatBytes(t.progress)}/${formatBytes(t.fileSize)}${t.speed > 0 ? ' · ' + formatSpeed(t.speed) : ''}</span>`;
            } else if (t.status === 'pending') {
                statusHtml = '<span class="tg-file-card-status">等待对方接受...</span>';
            } else if (t.status === 'completed') {
                statusHtml = '<span class="tg-file-card-status completed">已完成</span>';
            } else if (t.status === 'failed') {
                statusHtml = '<span class="tg-file-card-status failed">发送失败</span>';
            }
            card.innerHTML = `
                <div class="tg-file-card-header">
                    <span class="tg-file-card-icon">${icon}</span>
                    <span class="tg-file-card-title">${title}</span>
                </div>
                <div class="tg-file-card-body">
                    <div class="tg-file-card-name">${escapeHtml(t.fileName)}</div>
                    <div class="tg-file-card-size">${formatBytes(t.fileSize)}</div>
                </div>
                <div class="tg-file-card-actions">${statusHtml}</div>
            `;
            container.appendChild(card);
        }
    });
}

// Insert a file request card into the sender's chat conversation
function insertFileRequestCard(transfer) {
    if (AppState.currentChatId !== transfer.peerName) return;
    const container = document.getElementById('messages');
    if (!container) return;

    // Skip if inline element already exists in a message bubble
    if (document.querySelector(`.tg-msg-file-actions[data-file-id="${transfer.fileId}"]`)) return;

    // Avoid duplicate cards
    if (container.querySelector(`[data-file-id="${transfer.fileId}"]`)) return;

    const card = document.createElement('div');
    card.className = 'tg-file-card';
    card.dataset.fileId = transfer.fileId;

    card.innerHTML = `
        <div class="tg-file-card-header">
            <span class="tg-file-card-icon">📥</span>
            <span class="tg-file-card-title">文件接收请求</span>
        </div>
        <div class="tg-file-card-body">
            <div class="tg-file-card-name">${escapeHtml(transfer.fileName)}</div>
            <div class="tg-file-card-size">${formatBytes(transfer.fileSize)}</div>
        </div>
        <div class="tg-file-card-actions" data-file-id="${transfer.fileId}">
            <button class="tg-file-card-btn reject">拒绝</button>
            <button class="tg-file-card-btn accept">接受</button>
        </div>
    `;

    // Bind buttons
    card.querySelector('.tg-file-card-btn.accept').onclick = () => respondToFileCard(transfer.fileId, true);
    card.querySelector('.tg-file-card-btn.reject').onclick = () => respondToFileCard(transfer.fileId, false);

    container.appendChild(card);
    scrollToBottom(container);
}

// Insert a file completed card with open file/folder buttons
function insertFileCompletedCard(transfer) {
    if (AppState.currentChatId !== transfer.peerName) return;
    const container = document.getElementById('messages');
    if (!container) return;

    const card = document.createElement('div');
    card.className = 'tg-file-card completed';
    card.dataset.fileId = transfer.fileId;

    card.innerHTML = `
        <div class="tg-file-card-header">
            <span class="tg-file-card-icon">✅</span>
            <span class="tg-file-card-title">文件接收完成</span>
        </div>
        <div class="tg-file-card-body">
            <div class="tg-file-card-name">${escapeHtml(transfer.fileName)}</div>
            <div class="tg-file-card-size">${formatBytes(transfer.fileSize)}</div>
            <div class="tg-file-card-path">${escapeHtml(transfer.savePath)}</div>
        </div>
        <div class="tg-file-card-actions">
            <button class="tg-file-card-btn open-file">打开文件</button>
            <button class="tg-file-card-btn open-folder">打开文件夹</button>
        </div>
    `;

    card.querySelector('.tg-file-card-btn.open-file').onclick = () => openFilePath(transfer.savePath);
    card.querySelector('.tg-file-card-btn.open-folder').onclick = () => openFolderPath(transfer.savePath);

    container.appendChild(card);
    scrollToBottom(container);
}

// Update existing pending cards when their status changes (e.g. accepted → transferring)
function renderFileCardUpdates(transfers) {
    const container = document.getElementById('messages');
    if (!container) return;

    container.querySelectorAll('.tg-file-card[data-file-id]').forEach(card => {
        const fileId = card.dataset.fileId;
        const transfer = transfers.find(t => t.fileId === fileId);
        if (!transfer) return;

        const actions = card.querySelector('.tg-file-card-actions');
        if (!actions) return;

        // Update transfer progress inline
        if (transfer.status === 'transferring') {
            const pct = transfer.fileSize > 0 ? (transfer.progress / transfer.fileSize * 100) : 0;
            const progressText = `${pct.toFixed(0)}% · ${formatBytes(transfer.progress)}/${formatBytes(transfer.fileSize)}${transfer.speed > 0 ? ' · ' + formatSpeed(transfer.speed) : ''}`;
            actions.innerHTML = `<span class="tg-file-card-status transferring">${progressText}</span>`;
        } else if (transfer.status === 'completed' && !card.classList.contains('completed')) {
            card.classList.add('completed');
            card.querySelector('.tg-file-card-icon').textContent = '✅';
            card.querySelector('.tg-file-card-title').textContent = '文件接收完成';
            if (transfer.savePath) {
                const body = card.querySelector('.tg-file-card-body');
                if (!body.querySelector('.tg-file-card-path')) {
                    const pathEl = document.createElement('div');
                    pathEl.className = 'tg-file-card-path';
                    pathEl.textContent = transfer.savePath;
                    body.appendChild(pathEl);
                }
                actions.innerHTML = `
                    <button class="tg-file-card-btn open-file">打开文件</button>
                    <button class="tg-file-card-btn open-folder">打开文件夹</button>
                `;
                actions.querySelector('.open-file').onclick = () => openFilePath(transfer.savePath);
                actions.querySelector('.open-folder').onclick = () => openFolderPath(transfer.savePath);
            } else {
                actions.innerHTML = '<span class="tg-file-card-status completed">已完成</span>';
            }
        } else if (transfer.status === 'failed' && !card.classList.contains('failed')) {
            card.classList.add('failed');
            card.querySelector('.tg-file-card-icon').textContent = '❌';
            card.querySelector('.tg-file-card-title').textContent = '文件传输失败';
            actions.innerHTML = '<span class="tg-file-card-status failed">传输失败</span>';
        }
    });
}

function respondToFileCard(fileId, accepted) {
    const card = document.querySelector(`.tg-file-card[data-file-id="${fileId}"]`);
    if (card) {
        const actions = card.querySelector('.tg-file-card-actions');
        actions.innerHTML = `<span class="tg-file-card-status ${accepted ? 'transferring' : 'rejected'}">${accepted ? '已接受，等待传输...' : '已拒绝'}</span>`;
    }

    fetch('/fileresponse', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ fileId, accepted })
    })
    .then(r => {
        if (!r.ok) throw new Error();
    })
    .catch(() => showToast('发送响应失败', 'error'));
}

// =================================
// Inline File Transfer Actions
// =================================
function renderSenderFileActions(container, transfer, fileId) {
    if (!transfer || transfer.status === 'pending') {
        container.innerHTML = `
            <span class="tg-msg-file-status">等待对方接受...</span>
            <button class="tg-msg-file-btn cancel">取消</button>
        `;
        container.querySelector('.cancel').onclick = () => inlineCancelFileTransfer(fileId);
    } else if (transfer.status === 'transferring') {
        const pct = transfer.fileSize > 0 ? (transfer.progress / transfer.fileSize * 100) : 0;
        container.innerHTML = `
            <span class="tg-msg-file-status transferring">${pct.toFixed(0)}% · ${formatBytes(transfer.progress)}/${formatBytes(transfer.fileSize)}${transfer.speed > 0 ? ' · ' + formatSpeed(transfer.speed) : ''}</span>
            <button class="tg-msg-file-btn cancel">取消</button>
        `;
        container.querySelector('.cancel').onclick = () => inlineCancelFileTransfer(fileId);
    } else if (transfer.status === 'completed') {
        container.innerHTML = `<span class="tg-msg-file-status completed">已发送</span>`;
    } else if (transfer.status === 'cancelled') {
        container.innerHTML = `<span class="tg-msg-file-status cancelled">已取消</span>`;
    } else if (transfer.status === 'failed') {
        container.innerHTML = `<span class="tg-msg-file-status failed">发送失败</span>`;
    }
}

function renderReceiverFileActions(container, transfer, fileId) {
    if (!transfer || transfer.status === 'pending') {
        container.innerHTML = `
            <button class="tg-msg-file-btn accept">接受</button>
            <button class="tg-msg-file-btn reject">拒绝</button>
        `;
        container.querySelector('.accept').onclick = () => inlineRespondToFileTransfer(fileId, true);
        container.querySelector('.reject').onclick = () => inlineRespondToFileTransfer(fileId, false);
    } else if (transfer.status === 'transferring') {
        const pct = transfer.fileSize > 0 ? (transfer.progress / transfer.fileSize * 100) : 0;
        container.innerHTML = `<span class="tg-msg-file-status transferring">${pct.toFixed(0)}% · ${formatBytes(transfer.progress)}/${formatBytes(transfer.fileSize)}${transfer.speed > 0 ? ' · ' + formatSpeed(transfer.speed) : ''}</span>`;
    } else if (transfer.status === 'completed') {
        if (transfer.savePath) {
            container.innerHTML = `
                <div class="tg-msg-file-path">${escapeHtml(transfer.savePath)}</div>
                <div class="tg-msg-file-actions-row">
                    <span class="tg-msg-file-status completed">已接收</span>
                    <button class="tg-msg-file-btn open-file">打开</button>
                    <button class="tg-msg-file-btn open-folder">文件夹</button>
                </div>
            `;
            container.querySelector('.open-file').onclick = () => openFilePath(transfer.savePath);
            container.querySelector('.open-folder').onclick = () => openFolderPath(transfer.savePath);
        } else {
            container.innerHTML = `<span class="tg-msg-file-status completed">已接收</span>`;
        }
    } else if (transfer.status === 'cancelled') {
        container.innerHTML = `<span class="tg-msg-file-status cancelled">对方已取消</span>`;
    } else if (transfer.status === 'failed') {
        container.innerHTML = `<span class="tg-msg-file-status failed">传输失败</span>`;
    }
}

function inlineCancelFileTransfer(fileId) {
    fetch('/filecancel', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ fileId })
    })
    .then(r => {
        if (!r.ok) throw new Error();
        // Immediately update UI
        const el = document.querySelector(`.tg-msg-file-actions[data-file-id="${fileId}"]`);
        if (el) el.innerHTML = `<span class="tg-msg-file-status cancelled">已取消</span>`;
        loadFileTransfers();
    })
    .catch(() => showToast('取消失败', 'error'));
}

function inlineRespondToFileTransfer(fileId, accepted) {
    // Immediately update inline UI
    const el = document.querySelector(`.tg-msg-file-actions[data-file-id="${fileId}"]`);
    if (el) {
        el.innerHTML = `<span class="tg-msg-file-status ${accepted ? 'transferring' : 'cancelled'}">${accepted ? '已接受，等待传输...' : '已拒绝'}</span>`;
    }

    // Also update standalone card if present
    const card = document.querySelector(`.tg-file-card[data-file-id="${fileId}"]`);
    if (card) {
        const actions = card.querySelector('.tg-file-card-actions');
        if (actions) {
            actions.innerHTML = `<span class="tg-file-card-status ${accepted ? 'transferring' : 'rejected'}">${accepted ? '已接受，等待传输...' : '已拒绝'}</span>`;
        }
    }

    fetch('/fileresponse', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ fileId, accepted })
    })
    .then(r => {
        if (!r.ok) throw new Error();
        loadFileTransfers();
    })
    .catch(() => showToast('发送响应失败', 'error'));
}

function updateInlineFileActions() {
    document.querySelectorAll('.tg-msg-file-actions[data-file-id]').forEach(el => {
        const fileId = el.dataset.fileId;
        const transfer = AppState.fileTransfers.find(t => t.fileId === fileId);
        const row = el.closest('.tg-msg-row');
        const isOwn = row && row.classList.contains('own');
        if (isOwn) {
            renderSenderFileActions(el, transfer, fileId);
        } else {
            renderReceiverFileActions(el, transfer, fileId);
        }
    });
}

function openFilePath(filePath) {
    if (AppState.isWails) {
        window.go.main.DesktopApp.OpenFile(filePath).catch(() => {
            showToast('无法打开文件', 'error');
        });
    } else {
        fetch('/open-file', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ path: filePath })
        }).catch(() => showToast('无法打开文件', 'error'));
    }
}

function openFolderPath(filePath) {
    if (AppState.isWails) {
        window.go.main.DesktopApp.RevealInExplorer(filePath).catch(() => {
            showToast('无法打开文件夹', 'error');
        });
    } else {
        fetch('/open-folder', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ path: filePath })
        }).catch(() => showToast('无法打开文件夹', 'error'));
    }
}

// =================================
// Image
// =================================
function sendImage(imageFile) {
    if (!imageFile) return;
    if (!AppState.currentChatId) {
        showToast('请先选择一个聊天', 'warning');
        return;
    }

    const targetName = AppState.currentChatId === 'all' ? 'all' : AppState.currentChatId;

    const reader = new FileReader();
    reader.onload = () => {
        const base64 = reader.result.split(',')[1];
        if (AppState.isWails) {
            // Wails mode: Go binding (same path as paperclip)
            window.go.main.DesktopApp.SendImageBase64(base64, imageFile.name, imageFile.type || 'image/png', targetName)
                .then(r => { if (r && r.status === 'success') { showToast('图片发送成功', 'success'); loadMessages(); } })
                .catch(err => showToast('图片发送失败: ' + err, 'error'));
        } else {
            // Browser mode: HTTP POST
            fetch('/sendimage', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    image: base64,
                    fileName: imageFile.name,
                    fileType: imageFile.type || 'image/png',
                    fileSize: imageFile.size,
                    targetName: targetName
                })
            })
                .then(r => r.json())
                .then(data => { if (data.status === 'success') showToast('图片发送成功', 'success'); else throw new Error(); })
                .catch(() => showToast('图片发送失败', 'error'));
        }
    };
    reader.onerror = () => showToast('读取图片文件失败', 'error');
    reader.readAsDataURL(imageFile);
}

function openImageModal(src) {
    const modal = document.createElement('div');
    modal.className = 'tg-image-modal';
    modal.innerHTML = `
        <img src="${src}">
        <button class="tg-image-modal-close">✕</button>
    `;
    modal.onclick = (e) => {
        if (e.target === modal || e.target.classList.contains('tg-image-modal-close')) {
            modal.remove();
        }
    };
    document.body.appendChild(modal);
}

// =================================
// Drag & Drop
// =================================
function initDragAndDrop() {
    const conversation = document.getElementById('conversation');
    const overlay = document.getElementById('dropOverlay');
    let dragCounter = 0;

    // Wails mode: file drop handled by Wails' built-in DragAndDrop (runtime.OnFileDrop).
    // It gives file paths directly — same speed as paperclip button.
    // The overlay is managed via CSS: --wails-drop-target / wails-drop-target-active class.
    if (AppState.isWails) {
        return; // no JS drag handlers needed — Wails handles everything natively
    }

    // Browser mode: drag-drop with overlay
    conversation.addEventListener('dragenter', (e) => {
        e.preventDefault();
        dragCounter++;
        if (dragCounter === 1) {
            overlay.style.display = 'flex';
        }
    });

    conversation.addEventListener('dragover', (e) => {
        e.preventDefault();
        e.dataTransfer.dropEffect = 'copy';
    });

    conversation.addEventListener('dragleave', (e) => {
        e.preventDefault();
        dragCounter--;
        if (dragCounter <= 0) {
            dragCounter = 0;
            overlay.style.display = 'none';
        }
    });

    conversation.addEventListener('drop', (e) => {
        e.preventDefault();
        dragCounter = 0;
        overlay.style.display = 'none';
        handleDrop(e);
    });
}

function handleDrop(e) {
    if (!AppState.currentChatId) {
        showToast('请先选择一个聊天', 'warning');
        return;
    }

    const items = e.dataTransfer.items;
    const files = [];

    // Browser mode: try webkitGetAsEntry for directory support
    if (items && items.length > 0 && items[0].webkitGetAsEntry) {
        const entries = [];
        for (let i = 0; i < items.length; i++) {
            const entry = items[i].webkitGetAsEntry();
            if (entry) entries.push(entry);
        }
        collectFilesFromEntries(entries).then(collected => {
            if (collected.length > 0) processDroppedFiles(collected);
        });
    } else {
        // Fallback to dataTransfer.files
        for (let i = 0; i < e.dataTransfer.files.length; i++) {
            files.push(e.dataTransfer.files[i]);
        }
        if (files.length > 0) processDroppedFiles(files);
    }
}

function collectFilesFromEntries(entries) {
    const files = [];
    const promises = [];

    for (const entry of entries) {
        if (entry.isFile) {
            promises.push(new Promise(resolve => {
                entry.file(f => { files.push(f); resolve(); });
            }));
        } else if (entry.isDirectory) {
            promises.push(readDirectoryEntries(entry).then(dirFiles => {
                files.push(...dirFiles);
            }));
        }
    }

    return Promise.all(promises).then(() => files);
}

function readDirectoryEntries(dirEntry) {
    return new Promise(resolve => {
        const reader = dirEntry.createReader();
        const allEntries = [];

        function readBatch() {
            reader.readEntries(batch => {
                if (batch.length === 0) {
                    // All entries read, recurse into them
                    collectFilesFromEntries(allEntries).then(resolve);
                } else {
                    allEntries.push(...batch);
                    readBatch(); // Keep reading (readEntries returns max ~100 at a time)
                }
            });
        }

        readBatch();
    });
}

async function processDroppedFiles(files) {
    const imageFiles = [];
    const otherFiles = [];

    for (const file of files) {
        if (isImageFile(file)) {
            imageFiles.push(file);
        } else {
            otherFiles.push(file);
        }
    }

    // Send non-image files directly (requires private chat)
    for (const file of otherFiles) {
        sendDroppedFile(file);
    }

    // Process images sequentially with choice dialog
    for (const file of imageFiles) {
        await showDropChoiceDialog(file);
    }
}

function isImageFile(file) {
    if (file.type && file.type.startsWith('image/')) return true;
    const ext = (file.name || '').split('.').pop().toLowerCase();
    return ['jpg', 'jpeg', 'png', 'gif', 'bmp', 'webp'].includes(ext);
}

function showDropChoiceDialog(imageFile) {
    return new Promise(resolve => {
        const overlay = document.createElement('div');
        overlay.className = 'tg-drop-choice-overlay';

        const box = document.createElement('div');
        box.className = 'tg-drop-choice-box';

        const title = document.createElement('h4');
        title.textContent = imageFile.name;
        box.appendChild(title);

        // Image preview
        const preview = document.createElement('div');
        preview.className = 'tg-drop-preview';
        const img = document.createElement('img');
        const reader = new FileReader();
        reader.onload = (e) => { img.src = e.target.result; };
        reader.readAsDataURL(imageFile);
        preview.appendChild(img);
        box.appendChild(preview);

        const buttons = document.createElement('div');
        buttons.className = 'tg-drop-choice-buttons';

        const sendAsImage = document.createElement('button');
        sendAsImage.className = 'tg-drop-choice-btn image';
        sendAsImage.textContent = '📷 发送为图片';
        sendAsImage.onclick = () => {
            overlay.remove();
            sendImage(imageFile);
            resolve();
        };
        buttons.appendChild(sendAsImage);

        const sendAsFile = document.createElement('button');
        sendAsFile.className = 'tg-drop-choice-btn file';
        sendAsFile.textContent = '📁 发送为文件';
        // Disable file send in public chat
        if (!AppState.currentChatId || AppState.currentChatId === 'all') {
            sendAsFile.disabled = true;
            sendAsFile.title = '文件传输需要在私聊中使用';
        }
        sendAsFile.onclick = () => {
            overlay.remove();
            sendDroppedFile(imageFile);
            resolve();
        };
        buttons.appendChild(sendAsFile);

        const cancel = document.createElement('button');
        cancel.className = 'tg-drop-choice-cancel';
        cancel.textContent = '取消';
        cancel.onclick = () => {
            overlay.remove();
            resolve();
        };
        buttons.appendChild(cancel);

        box.appendChild(buttons);
        overlay.appendChild(box);

        // Click overlay background to cancel
        overlay.addEventListener('click', (e) => {
            if (e.target === overlay) {
                overlay.remove();
                resolve();
            }
        });

        document.body.appendChild(overlay);
    });
}

// Wails-mode drop choice dialog: uses Go bindings (SendImagePath/SendFilePath) instead of HTTP.
// Takes a file path string (not a File object).
function showWailsDropChoiceDialog(filePath) {
    return new Promise(resolve => {
        const fileName = filePath.split(/[/\\]/).pop();
        const target = AppState.currentChatId;

        const overlay = document.createElement('div');
        overlay.className = 'tg-drop-choice-overlay';

        const box = document.createElement('div');
        box.className = 'tg-drop-choice-box';

        const title = document.createElement('h4');
        title.textContent = fileName;
        box.appendChild(title);

        // Image preview using file:/// URL
        const preview = document.createElement('div');
        preview.className = 'tg-drop-preview';
        const img = document.createElement('img');
        img.src = 'file:///' + filePath.replace(/\\/g, '/');
        img.onerror = () => { preview.innerHTML = '<span style="color:var(--tg-text-secondary);font-size:13px">(预览不可用)</span>'; };
        preview.appendChild(img);
        box.appendChild(preview);

        const buttons = document.createElement('div');
        buttons.className = 'tg-drop-choice-buttons';

        const sendAsImage = document.createElement('button');
        sendAsImage.className = 'tg-drop-choice-btn image';
        sendAsImage.textContent = '发送为图片';
        sendAsImage.onclick = () => {
            overlay.remove();
            window.go.main.DesktopApp.SendImagePath(filePath, target)
                .then(result => {
                    if (result && result.status === 'success') {
                        showToast('图片发送成功', 'success');
                        loadMessages();
                    }
                })
                .catch(err => showToast('图片发送失败: ' + err, 'error'));
            resolve();
        };
        buttons.appendChild(sendAsImage);

        const sendAsFile = document.createElement('button');
        sendAsFile.className = 'tg-drop-choice-btn file';
        sendAsFile.textContent = '发送为文件';
        // File transfer requires private chat with online user
        const canSendFile = target && target !== 'all' && AppState.onlineUsers.includes(target);
        if (!canSendFile) {
            sendAsFile.disabled = true;
            sendAsFile.title = target === 'all' ? '文件传输需要在私聊中使用' : '对方不在线';
        }
        sendAsFile.onclick = () => {
            overlay.remove();
            window.go.main.DesktopApp.SendFilePath(filePath, target)
                .then(result => {
                    if (result && result.fileId) {
                        postFileMsgAfterSend(target, result.fileName,
                            parseInt(result.fileSize) || 0, '', result.fileId);
                    }
                })
                .catch(err => showToast('文件发送失败: ' + err, 'error'));
            resolve();
        };
        buttons.appendChild(sendAsFile);

        const cancel = document.createElement('button');
        cancel.className = 'tg-drop-choice-cancel';
        cancel.textContent = '取消';
        cancel.onclick = () => { overlay.remove(); resolve(); };
        buttons.appendChild(cancel);

        box.appendChild(buttons);
        overlay.appendChild(box);

        overlay.addEventListener('click', (e) => {
            if (e.target === overlay) { overlay.remove(); resolve(); }
        });

        document.body.appendChild(overlay);
    });
}

function sendDroppedFile(file) {
    if (!AppState.currentChatId || AppState.currentChatId === 'all') {
        showToast('文件传输需要在私聊中使用', 'warning');
        return;
    }
    if (!AppState.onlineUsers.includes(AppState.currentChatId)) {
        showToast('对方不在线，无法发送文件', 'warning');
        return;
    }

    if (AppState.isWails) {
        // Wails mode: read blob → Go binding saves temp + initiates P2P transfer
        const reader = new FileReader();
        reader.onload = () => {
            const base64 = reader.result.split(',')[1];
            window.go.main.DesktopApp.SendFileFromBase64(base64, file.name, AppState.currentChatId)
                .then(r => { if (r && r.fileId) postFileMsgAfterSend(AppState.currentChatId, r.fileName, parseInt(r.fileSize) || 0, '', r.fileId); })
                .catch(err => showToast(`文件发送失败: ${err}`, 'error'));
        };
        reader.onerror = () => showToast(`读取文件失败: ${file.name}`, 'error');
        reader.readAsDataURL(file);
        return;
    }

    // Browser mode: upload via base64
    uploadFileAsBase64(file, AppState.currentChatId)
        .then(data => {
            postFileMsgAfterSend(AppState.currentChatId, file.name,
                file.size, file.type || '', data.fileId || '');
        })
        .catch(() => showToast(`文件发送失败: ${file.name}`, 'error'));
}

// =================================
// Connection Status
// =================================
function checkConnection() {
    fetch('/ping')
        .then(r => {
            if (!r.ok) throw new Error();
            setConnectionStatus(true);
        })
        .catch(() => setConnectionStatus(false));
}

function setConnectionStatus(online) {
    const dot = document.querySelector('.tg-status-dot');
    const text = document.querySelector('.tg-status-text');
    if (online) {
        dot.classList.add('online');
        text.textContent = '已连接';
    } else {
        dot.classList.remove('online');
        text.textContent = '未连接';
    }
}

// =================================
// Search
// =================================
function initSearchFilter() {
    const input = document.getElementById('searchInput');
    input.addEventListener('input', () => {
        AppState.searchQuery = input.value.trim();
        renderChatList();
    });
}

// =================================
// Responsive
// =================================
function initResponsive() {
    const checkMobile = () => {
        AppState.isMobile = window.innerWidth <= 768;
        if (!AppState.isMobile) {
            document.querySelector('.tg-sidebar').classList.remove('hidden');
        }
    };

    window.addEventListener('resize', checkMobile);
    checkMobile();

    // Initial render: show chat list, hide conversation
    renderChatList();
}

// =================================
// Settings
// =================================
function loadSettings() {
    try {
        const saved = localStorage.getItem('lanshare_settings');
        if (saved) {
            const parsed = JSON.parse(saved);
            Object.assign(AppState.settings, parsed);
        }
    } catch { /* ignore */ }
    applyFontSize(AppState.settings.fontSize);
    applySkin(AppState.settings.skin);
}

function saveSettings() {
    localStorage.setItem('lanshare_settings', JSON.stringify(AppState.settings));
}

function applyFontSize(size) {
    AppState.settings.fontSize = size;
    document.documentElement.style.setProperty('--tg-font-size', size + 'px');
    document.documentElement.style.setProperty('--tg-emoji-size', Math.round(size * 1.5) + 'px');
}

function applySkin(skinId) {
    skinId = skinId || 'telegram';
    AppState.settings.skin = skinId;
    // Swap theme CSS file
    var themeLink = document.getElementById('themeCSS');
    if (themeLink) {
        document.documentElement.classList.add('theme-transitioning');
        themeLink.href = '/static/theme-' + skinId + '.css';
        setTimeout(function() { document.documentElement.classList.remove('theme-transitioning'); }, 350);
    }
    // Update skin selector UI
    var options = document.querySelectorAll('#skinOptions .tg-skin-option');
    options.forEach(function(opt) {
        opt.classList.toggle('active', opt.dataset.skin === skinId);
    });
    // Update meta theme-color + native title bar
    var meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.content = skinId === 'wisetalk' ? '#0089ff' : '#17212b';
    if (typeof window.go !== 'undefined' && window.go.main && window.go.main.DesktopApp) {
        window.go.main.DesktopApp.SetWindowTheme(skinId === 'wisetalk' ? 'light' : 'dark');
        window.go.main.DesktopApp.SetWindowIcon(skinId);
        // WiseTalk: hide title bar text; Telegram: restore
        window.runtime.WindowSetTitle(skinId === 'wisetalk' ? '' : 'LS Messager');
        // Update notification app name per theme
        window.go.main.DesktopApp.SetNotificationAppName(skinId === 'wisetalk' ? '即时通' : 'LS Messager');
    }
    // Swap favicon per theme
    var favicon = document.querySelector('link[rel="icon"]');
    if (favicon) {
        if (skinId === 'wisetalk') {
            favicon.href = '/static/wisetalk-icon.png';
        } else {
            favicon.href = 'data:image/svg+xml,<svg xmlns=%22http://www.w3.org/2000/svg%22 viewBox=%220 0 100 100%22><text y=%22.9em%22 font-size=%2290%22>💬</text></svg>';
        }
    }
    // Update empty state and input layout per theme
    updateEmptyState(skinId);
    updateInputLayout(skinId);
    // Refresh UI to update theme-dependent elements (public chat avatar etc.)
    renderChatList();
    if (AppState.currentChatId) updateConversationHeader();
}

function updateEmptyState(skinId) {
    var container = document.getElementById('mainEmpty');
    if (!container) return;
    if (skinId === 'wisetalk') {
        container.innerHTML =
            '<img class="tg-empty-illustration" src="/static/wisetalk-empty.png" alt="">' +
            '<div class="tg-empty-text">开始聊天</div>' +
            '<div class="tg-empty-subtitle">选择一个联系人，立即开始对话</div>';
    } else {
        container.innerHTML =
            '<div class="tg-empty-icon">💬</div>' +
            '<div class="tg-empty-text">选择一个聊天开始对话</div>';
    }
}

function updateInputLayout(skinId) {
    var existing = document.querySelector('.tg-send-btn-wt');
    if (skinId === 'wisetalk') {
        if (!existing) {
            var inputArea = document.querySelector('.tg-input-area');
            if (!inputArea) return;
            var sendWrap = document.createElement('div');
            sendWrap.className = 'tg-send-btn-wt';
            var mode = AppState.settings.sendMode || 'enter';
            sendWrap.innerHTML =
                '<button id="wtSendBtn">发送</button>' +
                '<span class="tg-send-dropdown" id="wtSendDropdown">∨</span>' +
                '<div class="tg-send-mode-menu" id="wtSendModeMenu">' +
                    '<div class="tg-send-mode-option" data-mode="enter">' +
                        '<span class="tg-check">' + (mode === 'enter' ? '✓' : '') + '</span>' +
                        '<span>按Enter发送</span>' +
                    '</div>' +
                    '<div class="tg-send-mode-option" data-mode="ctrlenter">' +
                        '<span class="tg-check">' + (mode === 'ctrlenter' ? '✓' : '') + '</span>' +
                        '<span>按Ctrl+Enter发送</span>' +
                    '</div>' +
                '</div>';
            var inputRow = inputArea.querySelector('.tg-input-row');
            if (inputRow) inputRow.appendChild(sendWrap);
            // Send button click
            document.getElementById('wtSendBtn').addEventListener('click', function() {
                var input = document.getElementById('messageInput');
                if (input && input.value.trim()) sendMessage();
            });
            // Dropdown toggle
            document.getElementById('wtSendDropdown').addEventListener('click', function(e) {
                e.stopPropagation();
                var menu = document.getElementById('wtSendModeMenu');
                menu.classList.toggle('show');
            });
            // Send mode selection
            document.querySelectorAll('#wtSendModeMenu .tg-send-mode-option').forEach(function(opt) {
                opt.addEventListener('click', function(e) {
                    e.stopPropagation();
                    var newMode = this.dataset.mode;
                    AppState.settings.sendMode = newMode;
                    saveSettings();
                    // Update checkmarks
                    document.querySelectorAll('#wtSendModeMenu .tg-check').forEach(function(c) { c.textContent = ''; });
                    this.querySelector('.tg-check').textContent = '✓';
                    document.getElementById('wtSendModeMenu').classList.remove('show');
                });
            });
            // Close menu on outside click
            document.addEventListener('click', function wtMenuClose() {
                var menu = document.getElementById('wtSendModeMenu');
                if (menu) menu.classList.remove('show');
            });
        }
    } else {
        if (existing) existing.remove();
    }
}

function getAccentColor() {
    return getComputedStyle(document.documentElement).getPropertyValue('--tg-accent').trim() || '#2ca5e0';
}

function initSettings() {
    const sidebarMain = document.getElementById('sidebarMain');
    const sidebarSettings = document.getElementById('sidebarSettings');
    const openBtn = document.getElementById('settingsBtn');
    const backBtn = document.getElementById('settingsBackBtn');
    const usernameInput = document.getElementById('settingUsername');
    const fontDecBtn = document.getElementById('fontDecBtn');
    const fontIncBtn = document.getElementById('fontIncBtn');
    const fontDisplay = document.getElementById('fontSizeDisplay');
    const msgNotify = document.getElementById('settingMsgNotify');
    const onlineNotify = document.getElementById('settingOnlineNotify');
    const badgeCount = document.getElementById('settingBadgeCount');
    const saveHistoryToggle = document.getElementById('settingSaveHistory');
    const logLevelSelect = document.getElementById('settingLogLevel');
    const openLogDirBtn = document.getElementById('openLogDirBtn');
    const versionEl = document.getElementById('settingsVersion');

    function openSettings() {
        // Populate current values
        usernameInput.value = AppState.localUsername;
        fontDisplay.textContent = AppState.settings.fontSize;
        msgNotify.checked = AppState.settings.msgNotify;
        onlineNotify.checked = AppState.settings.onlineNotify;
        badgeCount.checked = AppState.settings.badgeCount;

        // Switch sidebar view
        sidebarMain.style.display = 'none';
        sidebarSettings.style.display = 'flex';

        // Load version and log level
        loadSettingsInfo();
    }

    function closeSettings() {
        sidebarSettings.style.display = 'none';
        sidebarMain.style.display = 'flex';
    }

    function loadSettingsInfo() {
        const isWails = typeof window.go !== 'undefined';
        if (isWails) {
            window.go.main.DesktopApp.GetAppInfo().then(info => {
                const channelLabel = info.channel === 'stable' ? '稳定版' : '测试版';
                versionEl.textContent = `LANShare Messager v${info.version} [${channelLabel}]`;
            }).catch(() => {});
        }
        fetch('/version')
            .then(r => r.json())
            .then(data => {
                if (!isWails) {
                    const channelLabel = data.channel === 'stable' ? '稳定版' : '测试版';
                    versionEl.textContent = `LANShare Messager v${data.version} [${channelLabel}]`;
                }
                if (data.channel) {
                    _localChannel = data.channel;
                }
                if (data.logLevel) {
                    logLevelSelect.value = data.logLevel;
                }
                if (data.saveHistory !== undefined) {
                    saveHistoryToggle.checked = data.saveHistory;
                }
            })
            .catch(() => {});
    }

    openBtn.addEventListener('click', openSettings);
    backBtn.addEventListener('click', closeSettings);

    // Skin selector
    const skinOptions = document.getElementById('skinOptions');
    if (skinOptions) {
        skinOptions.addEventListener('click', (e) => {
            const option = e.target.closest('.tg-skin-option');
            if (!option) return;
            const skin = option.dataset.skin;
            applySkin(skin);
            saveSettings();
        });
    }

    // Username change
    let usernameTimeout = null;
    usernameInput.addEventListener('input', () => {
        clearTimeout(usernameTimeout);
        usernameTimeout = setTimeout(() => {
            const newName = usernameInput.value.trim();
            if (newName && newName !== AppState.localUsername) {
                fetch('/send', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ message: `/name ${newName}` })
                }).then(r => {
                    if (r.ok) {
                        AppState.localUsername = newName;
                        document.querySelector('.tg-user-info').textContent = newName + ' · ' + APP_DATA.localIP;
                        showToast('称呼已更改为 ' + newName, 'success');
                    }
                }).catch(() => {});
            }
        }, 800);
    });

    // Font size
    fontDecBtn.addEventListener('click', () => {
        const size = Math.max(12, AppState.settings.fontSize - 1);
        applyFontSize(size);
        fontDisplay.textContent = size;
        saveSettings();
    });

    fontIncBtn.addEventListener('click', () => {
        const size = Math.min(22, AppState.settings.fontSize + 1);
        applyFontSize(size);
        fontDisplay.textContent = size;
        saveSettings();
    });

    // Toggle switches
    msgNotify.addEventListener('change', () => {
        AppState.settings.msgNotify = msgNotify.checked;
        saveSettings();
    });

    onlineNotify.addEventListener('change', () => {
        AppState.settings.onlineNotify = onlineNotify.checked;
        saveSettings();
    });

    badgeCount.addEventListener('change', () => {
        AppState.settings.badgeCount = badgeCount.checked;
        saveSettings();
        updateTitleBadge();
    });

    // Save history toggle
    saveHistoryToggle.addEventListener('change', () => {
        const enabled = saveHistoryToggle.checked;
        fetch('/save-history', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ saveHistory: enabled })
        })
        .then(r => {
            if (r.ok) {
                showToast(enabled ? '聊天记录将会保存' : '关闭程序时将清空聊天记录', enabled ? 'success' : 'warning');
            } else {
                throw new Error();
            }
        })
        .catch(() => showToast('设置失败', 'error'));
    });

    // Log level change
    logLevelSelect.addEventListener('change', () => {
        const level = logLevelSelect.value;
        fetch('/loglevel', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ level })
        })
        .then(r => {
            if (r.ok) {
                showToast(`日志级别已设为 ${level}`, 'success');
            } else {
                throw new Error();
            }
        })
        .catch(() => showToast('修改日志级别失败', 'error'));
    });

    // Open log directory
    openLogDirBtn.addEventListener('click', () => {
        const isWails = typeof window.go !== 'undefined';
        if (isWails) {
            window.go.main.DesktopApp.OpenLogDir().catch(() => {
                showToast('无法打开日志目录', 'error');
            });
        } else {
            fetch('/open-logs', { method: 'POST' })
                .then(r => r.json())
                .then(data => {
                    if (data.path) {
                        showToast(`日志目录: ${data.path}`, 'info');
                    }
                })
                .catch(() => showToast('无法打开日志目录', 'error'));
        }
    });
}

// =================================
// Auto-Update
// =================================
let _lastUpdateVersion = null;
let _lastUpdateCrossChannel = false;
let _localChannel = 'stable'; // set by /version on init
function channelLabel(ch) {
    if (ch === 'test') return '测试版';
    return '正式版';
}
function checkForUpdate() {
    fetch('/check-update')
        .then(r => r.json())
        .then(data => {
            const banner = document.getElementById('updateBanner');
            const text = document.getElementById('updateBannerText');
            const btn = document.getElementById('updateBannerBtn');
            if (data.available) {
                const label = channelLabel(data.channel);
                text.textContent = `新版本 v${data.version} [${label}] (来自 ${data.source})`;
                banner.style.display = 'flex';
                _lastUpdateCrossChannel = !!data.crossChannel;
                // Reset button if a newer version appeared
                if (_lastUpdateVersion !== data.version) {
                    _lastUpdateVersion = data.version;
                    btn.disabled = false;
                    btn.textContent = '更新';
                }
            } else {
                banner.style.display = 'none';
            }
        })
        .catch(() => {});
}

function doPerformUpdate() {
    const btn = document.getElementById('updateBannerBtn');
    btn.disabled = true;
    btn.textContent = '下载中...';
    fetch('/perform-update', { method: 'POST' })
        .then(r => r.json())
        .then(data => {
            if (data.status === 'updating') {
                pollUpdateStatus();
            } else {
                showToast(data.message || '更新失败', 'error');
                btn.disabled = false;
                btn.textContent = '更新';
            }
        })
        .catch(() => {
            showToast('更新请求失败', 'error');
            btn.disabled = false;
            btn.textContent = '更新';
        });
}

function initUpdateBanner() {
    const btn = document.getElementById('updateBannerBtn');
    btn.addEventListener('click', () => {
        if (_lastUpdateCrossChannel) {
            showBanner('当前为正式版，确认要更新到测试版吗？测试版可能不稳定。', 'warning', {
                id: 'cross-channel-confirm',
                closable: true,
                actions: [
                    { label: '取消', class: 'secondary', onClick: (b, rm) => rm() },
                    { label: '确认更新', class: 'primary', onClick: (b, rm) => { rm(); doPerformUpdate(); } }
                ]
            });
        } else {
            doPerformUpdate();
        }
    });
}

function pollUpdateStatus() {
    const btn = document.getElementById('updateBannerBtn');
    showBanner('正在下载更新...', 'info', { id: 'update-progress', closable: false });
    const interval = setInterval(() => {
        fetch('/update-status')
            .then(r => r.json())
            .then(data => {
                if (data.status === 'completed') {
                    clearInterval(interval);
                    removeBannerById('update-progress');
                    btn.textContent = '已更新';
                    btn.disabled = true;
                    showRestartConfirm();
                } else if (data.status === 'failed') {
                    clearInterval(interval);
                    removeBannerById('update-progress');
                    showBanner(`更新失败: ${data.error || '未知错误'}`, 'error', { id: 'update-failed', duration: 5000 });
                    btn.disabled = false;
                    btn.textContent = '更新';
                }
                // 'downloading' — keep polling
            })
            .catch(() => {});
    }, 1000);
}

function showRestartConfirm() {
    showBanner('新版本已下载完成，是否立即重启？', 'success', {
        id: 'update-restart',
        closable: true,
        actions: [
            {
                label: '稍后',
                class: 'secondary',
                onClick: (banner, remove) => {
                    remove();
                }
            },
            {
                label: '立即重启',
                class: 'primary',
                onClick: (banner, remove) => {
                    const btns = banner.querySelectorAll('.tg-banner-btn');
                    btns.forEach(b => b.disabled = true);
                    banner.querySelector('.tg-banner-text').textContent = '正在重启，请稍候...';
                    fetch('/restart', { method: 'POST' })
                        .catch(() => {
                            remove();
                            showToast('重启失败，请手动重启程序', 'error');
                        });
                }
            }
        ]
    });
}
