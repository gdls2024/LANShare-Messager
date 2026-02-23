package main

// Event name constants for Wails runtime events
const (
	EventNewMessage      = "new-message"
	EventUserOnline      = "user-online"
	EventUserOffline     = "user-offline"
	EventUpdateAvailable = "update-available"
	EventUpdateCleared   = "update-cleared"
	EventFocusChat       = "focus-chat"
)

// Safe event emission helpers - check for nil before calling.
// Callbacks run in goroutines to avoid blocking the caller.
func (node *P2PNode) emitNewMessage(msg ChatMessage) {
	if node.OnNewMessage != nil {
		go node.OnNewMessage(msg)
	}
}

func (node *P2PNode) emitUserOnline(name string) {
	if node.OnUserOnline != nil {
		go node.OnUserOnline(name)
	}
}

func (node *P2PNode) emitUserOffline(name string) {
	if node.OnUserOffline != nil {
		go node.OnUserOffline(name)
	}
}

func (node *P2PNode) emitUpdateAvailable(source updateSource) {
	if node.OnUpdateAvailable != nil {
		go node.OnUpdateAvailable(source)
	}
}

// emitUpdateCleared notifies the frontend that the update source went offline.
// Re-uses OnUpdateAvailable with a zero-value source (empty version) as the "cleared" signal.
func (node *P2PNode) emitUpdateCleared() {
	if node.OnUpdateAvailable != nil {
		go node.OnUpdateAvailable(updateSource{})
	}
}
