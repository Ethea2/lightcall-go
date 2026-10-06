package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/Ethea2/lightcall-go/internal/config"
	"github.com/Ethea2/lightcall-go/internal/routes"
)

func main() {
	config.NewConfig()

	if config.ConfigInstance.Port == "" {
		config.ConfigInstance.Port = "3000"
	}

	r := routes.SetupRoutes()
	fmt.Printf("Server running on port %s", config.ConfigInstance.Port)
	log.Fatal(http.ListenAndServe(":"+config.ConfigInstance.Port, r))
}
