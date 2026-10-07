package services

import (
	"context"

	"github.com/Ethea2/lightcall-go/internal/rooms"
)

type RoomsService struct {
	rooms *rooms.Rooms
}

func NewRoomsService(r *rooms.Rooms) *RoomsService {
	return &RoomsService{
		rooms: r,
	}
}

func (r *RoomsService) CreateRoom(ctx context.Context) {

}

func (r *RoomsService) JoinRoom(ctx context.Context) {

}

func (r *RoomsService) LeaveRoom(ctx context.Context) {

}
