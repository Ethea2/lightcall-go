package routes

import (
	"encoding/json"
	"net/http"

	"github.com/Ethea2/lightcall-go/internal/dto"
	"github.com/Ethea2/lightcall-go/internal/handlers"
	"github.com/Ethea2/lightcall-go/internal/services"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

func SetupRoutes(roomsService *services.RoomsService) *chi.Mux {
	r := chi.NewRouter()

	r.Use(middleware.Logger)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	apiRouter := chi.NewRouter()

	apiRouter.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(dto.GeneralResponse[any]{
			Message: "Healthy!",
		})
	})

	roomsHandler := handlers.NewRoomsHandler(roomsService)

	roomsRouter := SetupRoomsRoutes(roomsHandler)

	apiRouter.Mount("/rooms", roomsRouter)

	r.Mount("/api", apiRouter)

	return r
}
