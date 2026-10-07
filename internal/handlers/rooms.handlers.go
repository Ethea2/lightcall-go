package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/Ethea2/lightcall-go/internal/dto"
	"github.com/Ethea2/lightcall-go/internal/rooms"
	"github.com/Ethea2/lightcall-go/internal/services"
)

type RoomsHandler struct {
	roomsService *services.RoomsService
}

func NewRoomsHandler(r *services.RoomsService) *RoomsHandler {
	return &RoomsHandler{
		roomsService: r,
	}
}

func (rh *RoomsHandler) CreateRoom(w http.ResponseWriter, r *http.Request) {
	createRoomReq := &dto.CreateRoomRequest{}

	err := json.NewDecoder(r.Body).Decode(&createRoomReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	room := rh.roomsService.CreateRoom(r.Context())

	json.NewEncoder(w).Encode(dto.GeneralResponse[*rooms.Room]{
		Data:    room,
		Message: "Created room successfully!",
	})
}

func (rh *RoomsHandler) GetAvailableRooms(w http.ResponseWriter, r *http.Request) {
	rooms := rh.roomsService.CheckAvailableRooms(r.Context())
	json.NewEncoder(w).Encode(dto.GeneralResponse[dto.CheckAvailableRoomsResponse]{
		Data: dto.CheckAvailableRoomsResponse{
			Rooms: rooms,
		},
		Message: "Rooms fetched successfully!",
	})
}
