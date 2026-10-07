package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/Ethea2/lightcall-go/internal/config"
	"github.com/Ethea2/lightcall-go/internal/rooms"
	"github.com/Ethea2/lightcall-go/internal/routes"
	"github.com/Ethea2/lightcall-go/internal/services"
)

func main() {
	configs := config.NewConfig()

	if configs.Port == "" {
		configs.Port = "3000"
	}

	if configs.StunServer == "" {
		configs.StunServer = "stun:stun.l.google.com:19302"
	}

	webrtcAPIConfigs := config.NewWebrtcAPIConfig(configs.StunServer)

	rooms := rooms.NewRooms(webrtcAPIConfigs.Api, webrtcAPIConfigs.PcConfig)

	roomsService := services.NewRoomsService(rooms)

	r := routes.SetupRoutes(roomsService)
	fmt.Printf("Server running on port %s\n", configs.Port)
	log.Fatal(http.ListenAndServe(":"+configs.Port, r))
}
