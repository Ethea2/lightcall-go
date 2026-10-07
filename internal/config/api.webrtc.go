package config

import (
	"fmt"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/webrtc/v4"
)

type WebrtcAPIConfig struct {
	Api      *webrtc.API
	PcConfig webrtc.Configuration
}

func NewWebrtcAPIConfig(stunServer string) *WebrtcAPIConfig {
	peerConnectionConfig := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{stunServer},
			},
		},
	}

	mediaEngine := &webrtc.MediaEngine{}

	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		fmt.Println("Failed to register default codecs")
	}

	interceptorRegistry := &interceptor.Registry{}

	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		fmt.Println("Failed to register default interceptors")
	}

	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		fmt.Println("Failed to create interval PLI interceptor")
	}

	interceptorRegistry.Add(intervalPliFactory)

	var ingestSettings webrtc.SettingEngine

	webrtcAPI := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
		webrtc.WithSettingEngine(ingestSettings),
	)

	return &WebrtcAPIConfig{
		Api:      webrtcAPI,
		PcConfig: peerConnectionConfig,
	}
}
