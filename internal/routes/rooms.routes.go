package routes

import (
	"github.com/Ethea2/lightcall-go/internal/handlers"
	"github.com/go-chi/chi/v5"
)

func SetupRoomsRoutes(roomsHandler *handlers.RoomsHandler) *chi.Mux {
	router := chi.NewRouter()

	router.Get("/", roomsHandler.GetAvailableRooms)
	router.Post("/", roomsHandler.CreateRoom)

	return router
}
