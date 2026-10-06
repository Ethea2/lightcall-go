package rooms

import (
	"sync"

	"github.com/pion/webrtc/v4"
	"golang.org/x/net/websocket"
)

type Rooms struct {
	Rooms []*Room
}

type Room struct {
	RoomID          string
	listLock        sync.RWMutex
	peerConnections []peerConnectionState
}

type peerConnectionState struct {
	peerConnection *webrtc.PeerConnection
	websocket      *threadSafeWriter
}

type threadSafeWriter struct {
	*websocket.Conn
	sync.Mutex
}
