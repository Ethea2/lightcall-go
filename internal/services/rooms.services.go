package services

import (
	"context"
)

type RoomsService struct{}

func NewRoomsService() *RoomsService {
	return &RoomsService{}
}

func (r *RoomsService) CreateRoom(ctx context.Context) {

}

func (r *RoomsService) JoinRoom(ctx context.Context) {

}

func (r *RoomsService) LeaveRoom(ctx context.Context) {

}
