package dto

type GeneralResponse[T any] struct {
	Data    T      `json:"data,omitempty"`
	Message string `json:"message"`
}
