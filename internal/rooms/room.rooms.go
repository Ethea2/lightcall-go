package rooms

import (
	"sync"

	"github.com/pion/webrtc/v4"
	"golang.org/x/net/websocket"
)

type Rooms struct {
	mu    sync.RWMutex
	Rooms map[string]*Room

	api      *webrtc.API
	pcConfig webrtc.Configuration
}

type Room struct {
	RoomID          string
	listLock        sync.RWMutex
	peerConnections []peerConnectionState
	trackLocals     map[string]*webrtc.TrackLocalStaticRTP

	api      *webrtc.API
	pcConfig webrtc.Configuration
}

type peerConnectionState struct {
	peerConnection *webrtc.PeerConnection
	websocket      *threadSafeWriter
}

type threadSafeWriter struct {
	*websocket.Conn
	sync.Mutex
}

type websocketMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

func NewRooms(api *webrtc.API, pcConfig webrtc.Configuration) *Rooms {
	return &Rooms{
		Rooms:    map[string]*Room{},
		api:      api,
		pcConfig: pcConfig,
	}
}

func (rs *Rooms) CreateRoom(generatedID string) *Room {
	room := &Room{
		RoomID:      generatedID,
		trackLocals: map[string]*webrtc.TrackLocalStaticRTP{},
		api:         rs.api,
		pcConfig:    rs.pcConfig,
	}

	rs.mu.Lock()
	rs.Rooms[generatedID] = room
	rs.mu.Unlock()
	return room
}
