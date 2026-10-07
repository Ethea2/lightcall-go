package dto

type CreateRoomRequest struct {
	Username string `json:"username"`
}

type CheckAvailableRoomsResponse struct {
	Rooms []string `json:"rooms,omitempty"`
}
