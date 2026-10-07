package services

import (
	"context"

	"github.com/Ethea2/lightcall-go/internal/rooms"
	"github.com/Ethea2/lightcall-go/internal/utils"
)

type RoomsService struct {
	rooms *rooms.Rooms
}

func NewRoomsService(r *rooms.Rooms) *RoomsService {
	return &RoomsService{
		rooms: r,
	}
}

func (r *RoomsService) CreateRoom(ctx context.Context) *rooms.Room {
	roomID := utils.GenerateRoomID()
	newRoom := r.rooms.CreateRoom(roomID)

	return newRoom
}

func (r *RoomsService) CheckAvailableRooms(ctx context.Context) []string {
	return r.rooms.CheckAvailableRooms()
}

func (r *RoomsService) JoinRoom(ctx context.Context) {

}

func (r *RoomsService) LeaveRoom(ctx context.Context) {

}
