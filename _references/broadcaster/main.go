// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js

// broadcast is a one-to-many SFU with the forwarding path left in the open.
//
// This file is next to sfu-ws because the SFU mechanism lives here. sfu-ws
// proves a call works, then buries that mechanism under signaling. If you
// only study sfu-ws, signalPeerConnections looks like the product, and the
// packet path stays hidden. signalPeerConnections never reads or writes RTP.
// It only decides which PeerConnection should send which track, then
// exchanges a new SDP offer so the browsers agree. The product is the copy
// in OnTrack below: Read one decrypted RTP packet from the ingest
// PeerConnection and Write it into a single TrackLocalStaticRTP. Every
// viewer PeerConnection is already bound to that track via AddTrack, so one
// Write is the fan-out.
//
// The broadcaster uploads one video. This process terminates that
// PeerConnection (ICE, DTLS, SRTP), copies the decrypted RTP into that
// TrackLocalStaticRTP, and each viewer PeerConnection sends the same packets
// under its own SRTP keys. Nothing is decoded or mixed.
//
// Signaling here is one SDP paste per peer (vanilla ICE, browser offers,
// server answers, no renegotiation). It exists so the copy loop has a
// session to read and sessions to write. It is not the mechanism.
package main

import (
	// encoding/base64 makes a SessionDescription safe to copy through a
	// terminal. The signaling channel in this example is a human paste.
	"encoding/base64"
	// encoding/json serializes the JSEP SessionDescription (type + SDP).
	"encoding/json"
	// errors identifies io.ErrClosedPipe, the "no subscriber is ready" case
	// on the forward path.
	"errors"
	// flag selects the TCP port of the signaling HTTP mailbox.
	"flag"
	// fmt prints the base64 answer the human pastes back into the browser.
	"fmt"
	// io reads the posted SDP and names ErrClosedPipe.
	"io"
	// log writes diagnosis to stderr. The paste token stays on stdout.
	"log"
	// net/http is the signaling mailbox. Media does not travel on it.
	"net/http"
	// strconv formats the listen port.
	"strconv"
	// strings inspects SDP and rejected signaling bodies.
	"strings"
	// time paces the forward-path and RTCP summaries.
	"time"

	// pion/interceptor is the RTP/RTCP pipeline that sits between the
	// application and SRTP. NACK, receiver reports, and PLI are interceptors.
	"github.com/pion/interceptor"
	// intervalpli is a receiver interceptor that writes a Picture Loss
	// Indication on a timer, asking the broadcaster for a keyframe.
	"github.com/pion/interceptor/pkg/intervalpli"
	// pion/logging is the ICE, DTLS, and SRTP logger. Warn is on by default
	// so library failures show up next to the session logs.
	"github.com/pion/logging"
	// pion/rtcp parses viewer feedback (PLI, NACK, receiver reports).
	"github.com/pion/rtcp"
	// pion/webrtc/v4 is the server-side stack: PeerConnection, ICE, DTLS-SRTP,
	// transceivers, and tracks.
	"github.com/pion/webrtc/v4"
)

// How often a healthy path reports. Failures log immediately.
const (
	forwardReportEvery = 5 * time.Second
	rtcpReportEvery    = 5 * time.Second
)

// nolint:gocognit, cyclop
func main() {
	// Signaling listen port. RTP uses ephemeral UDP ports chosen by ICE.
	port := flag.Int("port", 8080, "http server port")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// Pion's own ICE/DTLS logs. Warn is enough to name a failure; set
	// PION_LOG_DEBUG=all or PION_LOG_TRACE=ice when the state logs are not.
	loggerFactory := logging.NewDefaultLoggerFactory()
	if loggerFactory.DefaultLogLevel < logging.LogLevelWarn {
		loggerFactory.DefaultLogLevel = logging.LogLevelWarn
	}
	log.Printf("[log] pion internal level %s", loggerFactory.DefaultLogLevel)

	// Start the HTTP mailbox and take the channel it posts SDP offers on.
	// WebRTC does not define signaling. This channel is the entire signaling
	// protocol: one base64 SessionDescription per POST.
	sdpChan := httpSDPServer(*port)

	// Empty offer, filled by the broadcaster's pasted SDP.
	// WebRTC: a SessionDescription is a type (offer/answer) plus an SDP body.
	// The browser is the offerer for the ingest session.
	offer := webrtc.SessionDescription{}
	// Block until the broadcaster's offer arrives, then JSON-decode it.
	// The SFU cannot build the ingest PeerConnection's remote description
	// until this SDP exists: it carries the codec list, ICE ufrag/pwd, DTLS
	// fingerprint, and the video m-line the browser will send.
	log.Printf("[ingest] waiting for broadcaster offer")
	decode("ingest", <-sdpChan, &offer)
	// Blank line so the answer printed later is visually separate.
	fmt.Println("")

	// ICE configuration shared by the ingest PeerConnection and every viewer.
	// WebRTC: ICEServers lists STUN (and, in production, TURN). STUN asks a
	// public server "what address do you see me from?" and the answer becomes
	// a server-reflexive candidate, so a browser behind NAT can still reach
	// this process. Host candidates are gathered either way.
	peerConnectionConfig := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	// MediaEngine is the codec and RTP-header-extension catalogue that this
	// PeerConnection will put in SDP. Built by hand because the ingest
	// PeerConnection also needs a custom interceptor (intervalpli).
	// webrtc.NewPeerConnection hides this and installs the defaults.
	mediaEngine := &webrtc.MediaEngine{}
	// Register Opus, VP8, H264, and the other default codecs, plus the
	// extensions the default interceptors require. The negotiated codec is
	// whatever the broadcaster and this list have in common. The SFU forwards
	// that codec as-is; it does not transcode.
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		fail("ingest", "register default codecs", err)
	}

	// InterceptorRegistry is the user-built RTP/RTCP pipeline for one
	// PeerConnection. A registry cannot be shared: each PeerConnection calls
	// the factories once and binds the resulting interceptors to its own
	// streams. NewPeerConnection builds a private registry internally, which
	// is why adding intervalpli means constructing the API ourselves.
	interceptorRegistry := &interceptor.Registry{}

	// Default pipeline, in registration order: NACK (retransmit lost RTP),
	// RTCP sender/receiver reports, simulcast extension headers, stream
	// stats, and transport-wide congestion control (TWCC). These are what
	// NewPeerConnection would have installed. They are part of the media
	// path, not of signaling.
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		fail("ingest", "register default interceptors", err)
	}

	// intervalpli sends a PLI every 3 seconds, and one immediately when a
	// video SSRC is bound.
	// WebRTC: a keyframe (VP8 keyframe, H.264 IDR) is the only frame a
	// decoder can start from. Viewers join mid-stream, and the SFU does not
	// decode, so it cannot invent that frame. PLI (RFC 4585) asks the
	// broadcaster's encoder for one. The keyframe then rides the same copy
	// loop to every viewer.
	// A production SFU would also forward PLI and NACK that arrive from
	// viewers. This timer is the stand-in that keeps the example seekable.
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		fail("ingest", "create interval PLI interceptor", err)
	}
	log.Printf("[ingest] interval PLI interceptor installed")
	// Attach the factory to this registry only. The viewer API built later
	// does not get it. PLI goes to the publisher, so it belongs on the
	// ingest PeerConnection.
	interceptorRegistry.Add(intervalPliFactory)

	// Ingest PeerConnection: the SFU's side of the broadcaster's session.
	// WebRTC: this endpoint gathers ICE candidates, handshakes DTLS, and
	// keys SRTP. OnTrack then sees plaintext RTP. The API carries the codec
	// catalogue and the pipeline built above, including intervalpli.
	var ingestSettings webrtc.SettingEngine
	ingestSettings.LoggerFactory = loggerFactory
	peerConnection, err := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
		webrtc.WithSettingEngine(ingestSettings),
	).NewPeerConnection(peerConnectionConfig)
	if err != nil {
		fail("ingest", "create peer connection", err)
	}
	watchPeer("ingest", peerConnection)
	// Close the ingest session if main ever returns. The viewer loop below
	// does not return; each viewer PeerConnection is a separate value.
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			log.Printf("[ingest] close peer connection: %v", cErr)
		}
	}()

	// Give the ingest PeerConnection one video transceiver before the offer
	// is applied.
	// WebRTC: a transceiver pairs a sender and a receiver on one m-line.
	// No init is passed, so the direction is sendrecv. The broadcaster's
	// offer selects the negotiated direction; this transceiver is what gives
	// the answerer a video receiver so OnTrack can fire for that m-line.
	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		fail("ingest", "add video transceiver", err)
	}

	// Handshake from the ingest goroutine to the viewer-accept loop.
	// OnTrack runs only after the broadcaster's RTP arrives, which is after
	// the answer is pasted and ICE/DTLS complete. main waits on this channel
	// so no viewer PeerConnection is created until the local track exists.
	// Unbuffered: OnTrack blocks in the send until main receives it, then
	// starts the copy loop.
	localTrackChan := make(chan *webrtc.TrackLocalStaticRTP)
	// Ingest callback. remoteTrack is decrypted RTP from the broadcaster.
	// receiver is the RTPReceiver for that m-line; intervalpli, not this
	// callback, owns PLI, so receiver is unused.
	//
	// This function is the SFU. The Read/Write loop under it is the packet
	// path that sfu-ws hides. signalPeerConnections in sfu-ws is the
	// subscription and SDP step that this example does once, up front, by
	// waiting on localTrackChan and calling AddTrack before CreateAnswer.
	// After that, packets move here with no further signaling. Everything
	// after this callback in main is either "finish the ingest negotiation"
	// or "bind another viewer to localTrack".
	peerConnection.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) { //nolint: revive
		// One outbound track, shared by every viewer.
		// RTPCodecCapability is the codec the broadcaster negotiated (mime
		// type, clock rate, fmtp). Copying it is the "no transcode" decision.
		// "video" is the MediaStreamTrack id and "pion" is the MediaStream id;
		// viewers see a single stream with those msid values. One publisher,
		// so there is nothing to copy from remoteTrack.ID()/StreamID().
		codec := remoteTrack.Codec()
		log.Printf("[ingest] OnTrack kind=%s codec=%s clock=%d pt=%d ssrc=%d id=%s stream=%s rid=%q",
			remoteTrack.Kind(), codec.MimeType, codec.ClockRate, remoteTrack.PayloadType(),
			remoteTrack.SSRC(), remoteTrack.ID(), remoteTrack.StreamID(), remoteTrack.RID())

		localTrack, newTrackErr := webrtc.NewTrackLocalStaticRTP(codec.RTPCodecCapability, "video", "pion")
		if newTrackErr != nil {
			fail("ingest", "create local track", newTrackErr)
		}
		// Publish the track to main. The copy loop starts after this send
		// returns, which is after main is inside the viewer loop.
		// A second OnTrack blocks here: the session loop only receives once.
		log.Printf("[ingest] publishing local track to the session loop")
		localTrackChan <- localTrack
		log.Printf("[ingest] session loop accepted the local track; forwarding RTP")

		// One decrypted RTP packet per Read. 1400 bytes sits under a 1500-byte
		// Ethernet MTU. DTLS and SRTP have already been stripped by the
		// ingest PeerConnection.
		rtpBuf := make([]byte, 1400)
		var (
			packets          uint64
			totalBytes       uint64
			closedPipeWrites uint64
			lastPackets      uint64
			lastClosedPipe   uint64
			lastReport       = time.Now()
		)
		for {
			// Block for the next inbound RTP packet.
			// Media path: browser encoder, RTP, SRTP, DTLS, ICE/UDP, decrypt,
			// then Read.
			i, _, readErr := remoteTrack.Read(rtpBuf)
			if readErr != nil {
				log.Printf("[forward] RTP read stopped after packets=%d bytes=%d: %v", packets, totalBytes, readErr)
				panic(readErr)
			}
			packets++
			totalBytes += uint64(i)
			if packets == 1 {
				log.Printf("[forward] first RTP packet bytes=%d", i)
			}

			// Fan-out. Write unmarshals the RTP and delivers it to every
			// binding, meaning every viewer PeerConnection that AddTrack'd
			// this localTrack. Each binding rewrites SSRC and payload type to
			// the values in that viewer's SDP, then SRTP-protects with that
			// viewer's keys.
			// io.ErrClosedPipe means a binding has no open SRTP writer yet
			// (no viewer has finished DTLS, or a sender was closed). The
			// broadcaster keeps uploading; the loop stays up until a viewer
			// exists. Any other error aborts the forward.
			// One closed binding fails the call even when other viewers were
			// written, so the counter is "a binding was not ready", not
			// "nobody received this packet".
			if _, writeErr := localTrack.Write(rtpBuf[:i]); writeErr != nil {
				if !errors.Is(writeErr, io.ErrClosedPipe) {
					log.Printf("[forward] RTP write failed after packets=%d bytes=%d: %v", packets, totalBytes, writeErr)
					panic(writeErr)
				}
				closedPipeWrites++
				if closedPipeWrites == 1 {
					log.Printf("[forward] write closed pipe; a viewer binding has no SRTP writer yet")
				}
			}

			if time.Since(lastReport) >= forwardReportEvery {
				log.Printf("[forward] packets=%d (+%d) bytes=%d closedPipeWrites=%d (+%d)",
					packets, packets-lastPackets, totalBytes, closedPipeWrites, closedPipeWrites-lastClosedPipe)
				lastPackets = packets
				lastClosedPipe = closedPipeWrites
				lastReport = time.Now()
			}
		}
	})

	// Apply the broadcaster's offer as the remote description.
	// WebRTC: the ingest PeerConnection is the answerer. Signaling state
	// becomes have-remote-offer. Codecs, ICE credentials, and the DTLS
	// fingerprint are now known, which is what CreateAnswer needs.
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		fail("ingest", "set remote description", err)
	}

	// Build the SDP answer from the negotiated transceivers.
	// nil options means no ICE restart and no other JSEP overrides.
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		fail("ingest", "create answer", err)
	}

	// Channel that closes when ICE gathering finishes.
	// Registered before SetLocalDescription so a fast gather cannot be missed.
	// WebRTC: gathering is the ICE agent producing host and server-reflexive
	// candidates. Waiting for it is vanilla ICE: every candidate is embedded
	// in this one SDP. Trickle ICE would send the SDP immediately and forward
	// candidates as they appear (that is what sfu-ws does).
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Commit the answer. Signaling state returns toward stable, and gathering
	// starts. UDP sockets for the host candidates open here.
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		fail("ingest", "set local description", err)
	}

	// Block until gathering is complete. The local description now contains
	// the full candidate set, so a single paste is a complete answer. This
	// example has no second signaling message to carry a late candidate.
	<-gatherComplete

	// Print the answer for the human to paste into the broadcaster page.
	// encode is JSON, then base64. After the browser calls
	// setRemoteDescription, ICE checks run, DTLS handshakes, and OnTrack fires.
	// The paste token is the next stdout line. Diagnosis stays on stderr.
	local := peerConnection.LocalDescription()
	log.Printf("[ingest] answer ready %s", summarizeSDP(local.SDP))
	fmt.Println(encode(local))
	log.Printf("[ingest] waiting for broadcaster RTP")

	// Block until the copy loop has created the local track.
	// Viewers added before this would have nothing to AddTrack.
	localTrack := <-localTrackChan
	log.Printf("[ingest] forward track ready; accepting viewer offers")

	// Viewers get the default pipeline, not the ingest registry, so they do
	// not run intervalpli. The setting engine only adds the pion logger.
	viewerMedia := &webrtc.MediaEngine{}
	if err = viewerMedia.RegisterDefaultCodecs(); err != nil {
		fail("viewer", "register default codecs", err)
	}
	viewerRegistry := &interceptor.Registry{}
	if err = webrtc.RegisterDefaultInterceptors(viewerMedia, viewerRegistry); err != nil {
		fail("viewer", "register default interceptors", err)
	}
	var viewerSettings webrtc.SettingEngine
	viewerSettings.LoggerFactory = loggerFactory
	viewerAPI := webrtc.NewAPI(
		webrtc.WithMediaEngine(viewerMedia),
		webrtc.WithInterceptorRegistry(viewerRegistry),
		webrtc.WithSettingEngine(viewerSettings),
	)

	for viewerID := 1; ; viewerID++ {
		role := fmt.Sprintf("viewer %d", viewerID)
		fmt.Println("")
		// Next POST on sdpChan is a viewer's offer, not another broadcaster.
		fmt.Println("Curl an base64 SDP to start sendonly peer connection")
		log.Printf("[%s] waiting for offer", role)

		// The viewer's offer. The browser page offers a recvonly video
		// transceiver: it wants to receive and will not send.
		recvOnlyOffer := webrtc.SessionDescription{}
		decode(role, <-sdpChan, &recvOnlyOffer)

		// A new PeerConnection per viewer. This is the egress leg.
		// WebRTC: SFU topology is a star. The viewer has no PeerConnection to
		// the broadcaster. It has one to this process, which already holds
		// the packets. The viewer API installs the default interceptor
		// pipeline (NACK, reports, TWCC) and the default codecs. It does not
		// use the ingest registry, so it does not run intervalpli.
		// The outer peerConnection (the ingest session) is shadowed and stays
		// alive; closing this inner value would not close the broadcaster.
		peerConnection, err := viewerAPI.NewPeerConnection(peerConnectionConfig)
		if err != nil {
			fail(role, "create peer connection", err)
		}
		watchPeer(role, peerConnection)

		// Bind this viewer to the one shared local track.
		// WebRTC: AddTrack attaches an RTPSender. The next answer grows (or
		// matches) a video m-line whose direction is send from the SFU.
		// Doing it before SetRemoteDescription and CreateAnswer folds the
		// subscription into the viewer's single offer/answer. There is no
		// renegotiation because the track set is already known.
		// The same localTrack is passed for every viewer. That shared binding
		// list is the fan-out the copy loop writes into.
		rtpSender, err := peerConnection.AddTrack(localTrack)
		if err != nil {
			fail(role, "add track", err)
		}
		log.Printf("[%s] bound to the shared local track", role)

		// Drain this sender's RTCP for as long as the viewer is up.
		// WebRTC: RTCP is the feedback channel (receiver reports, NACK, PLI).
		// Interceptors process those packets as they are read. NACK
		// retransmission in particular does not run unless something reads.
		// The bytes are discarded after that. This loop does not forward
		// viewer PLI to the broadcaster; intervalpli on the ingest side is
		// what requests keyframes.
		go readViewerRTCP(viewerID, rtpSender)

		// Apply the viewer's recvonly offer. The RTPSender from AddTrack is
		// matched to the offered video m-line.
		err = peerConnection.SetRemoteDescription(recvOnlyOffer)
		if err != nil {
			fail(role, "set remote description", err)
		}

		// Answer: this PeerConnection sends the broadcast track, the browser
		// receives it. Still the answerer, still one round trip.
		answer, err := peerConnection.CreateAnswer(nil)
		if err != nil {
			fail(role, "create answer", err)
		}

		// Same vanilla-ICE gate as the ingest session, on this viewer's agent.
		gatherComplete = webrtc.GatheringCompletePromise(peerConnection)

		// Commit the answer and start this viewer's candidate gathering.
		err = peerConnection.SetLocalDescription(answer)
		if err != nil {
			fail(role, "set local description", err)
		}

		// Wait until this viewer's SDP contains every candidate.
		<-gatherComplete

		// Paste target for this viewer. After setRemoteDescription, ICE and
		// DTLS complete, the RTPSender binds SSRC and payload type, and the
		// copy loop's Write starts delivering packets to this viewer only,
		// encrypted for this PeerConnection.
		local := peerConnection.LocalDescription()
		log.Printf("[%s] answer ready %s", role, summarizeSDP(local.SDP))
		fmt.Println(encode(local))
	}
}

// encode turns a SessionDescription into the paste token printed on stdout.
// JSON is the JSEP encoding; base64 keeps the token on one line.
func encode(obj *webrtc.SessionDescription) string {
	b, err := json.Marshal(obj)
	if err != nil {
		fail("sdp", "encode answer", err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

// decode reverses encode. `in` is the body the browser posted, which is the
// base64 of a JSON SessionDescription. `obj` is filled in place so callers
// can pass offer or recvOnlyOffer.
func decode(role, in string, obj *webrtc.SessionDescription) {
	trimmed := strings.TrimSpace(in)
	if trimmed == "" {
		log.Printf("[%s] SDP body is empty", role)
		panic("empty SDP")
	}
	if strings.HasPrefix(trimmed, "{") {
		log.Printf("[%s] SDP body looks like raw JSON (%d bytes); expected base64(JSON)", role, len(in))
	}
	if trimmed != in {
		log.Printf("[%s] SDP body has leading or trailing whitespace (%d bytes)", role, len(in))
	}

	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		log.Printf("[%s] base64 decode failed (%d bytes, starts %q): %v", role, len(in), preview(in), err)
		panic(err)
	}

	if err = json.Unmarshal(b, obj); err != nil {
		log.Printf("[%s] SDP JSON unmarshal failed: %v", role, err)
		panic(err)
	}
	log.Printf("[%s] offer type=%s sdpBytes=%d %s", role, obj.Type, len(obj.SDP), summarizeSDP(obj.SDP))
}

// httpSDPServer is a one-way signaling mailbox.
// Each HTTP body is one offer, delivered on the returned channel. The HTTP
// response is only the text "done". The SDP answer is not written here; main
// prints it. A human carries it back to the browser.
// WebRTC: signaling may be any bidirectional channel. This one is a single
// message each way, which is why the example cannot trickle candidates or
// renegotiate and must use vanilla ICE.
func httpSDPServer(port int) chan string {
	sdpChan := make(chan string)
	http.HandleFunc("/", func(res http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			log.Printf("[http] read body from %s %s %s: %v", req.RemoteAddr, req.Method, req.URL.Path, err)
			http.Error(res, "read body", http.StatusBadRequest)
			return
		}
		log.Printf("[http] %s %s from %s bytes=%d", req.Method, req.URL.Path, req.RemoteAddr, len(body))
		if _, err = fmt.Fprint(res, "done"); err != nil {
			log.Printf("[http] write response to %s: %v", req.RemoteAddr, err)
		}
		// Blocks while the session loop is busy (gathering, or waiting on
		// the broadcaster track). The next line is the proof it was consumed.
		sdpChan <- string(body)
		log.Printf("[http] delivered %d bytes from %s to the session loop", len(body), req.RemoteAddr)
	})

	// Serve in the background so main can block on sdpChan. ListenAndServe
	// only returns on failure; exit there takes the process down because
	// there is then no signaling path left.
	go func() {
		// nolint: gosec
		err := http.ListenAndServe(":"+strconv.Itoa(port), nil)
		log.Fatalf("[http] server exited: %v", err)
	}()
	log.Printf("[http] signaling mailbox on :%d", port)

	return sdpChan
}

// fail logs the step that broke, then panics so the stack is still there.
func fail(role, step string, err error) {
	if err == nil {
		return
	}
	log.Printf("[%s] %s: %v", role, step, err)
	panic(err)
}

// watchPeer logs the transitions that distinguish "SDP was bad", "ICE never
// checked", "DTLS failed", and "the peer went away".
func watchPeer(role string, pc *webrtc.PeerConnection) {
	var candidates int
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			log.Printf("[%s] ICE gathering finished candidates=%d", role, candidates)
			return
		}
		candidates++
		if c.RelatedAddress != "" {
			log.Printf("[%s] ICE candidate %s %s %s:%d related %s:%d",
				role, c.Typ, c.Protocol, c.Address, c.Port, c.RelatedAddress, c.RelatedPort)
			return
		}
		log.Printf("[%s] ICE candidate %s %s %s:%d", role, c.Typ, c.Protocol, c.Address, c.Port)
	})
	pc.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		log.Printf("[%s] ICE gathering state: %s", role, state)
	})
	pc.OnSignalingStateChange(func(state webrtc.SignalingState) {
		log.Printf("[%s] signaling state: %s", role, state)
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("[%s] ICE connection state: %s", role, state)
		switch state {
		case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
			// Stats collection takes the peer-connection lock. Run it off
			// the ICE callback so a lock the agent still holds cannot deadlock.
			go logICEPairs(role, pc, false)
		case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateDisconnected:
			go logICEPairs(role, pc, true)
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		ice := pc.ICEConnectionState()
		log.Printf("[%s] peer connection state: %s (ice=%s signaling=%s)", role, state, ice, pc.SignalingState())
		if state == webrtc.PeerConnectionStateFailed && ice != webrtc.ICEConnectionStateFailed {
			log.Printf("[%s] peer failed while ICE is %s; check DTLS and the media path", role, ice)
			go logICEPairs(role, pc, true)
		}
	})
}

// logICEPairs names the candidate pair in use, or every pair when the
// connection failed and there is no selected pair to point at.
func logICEPairs(role string, pc *webrtc.PeerConnection, includeAll bool) {
	candidates := map[string]webrtc.ICECandidateStats{}
	var pairs []webrtc.ICECandidatePairStats
	for _, stat := range pc.GetStats() {
		switch s := stat.(type) {
		case webrtc.ICECandidateStats:
			candidates[s.ID] = s
		case webrtc.ICECandidatePairStats:
			pairs = append(pairs, s)
		}
	}
	if len(pairs) == 0 {
		log.Printf("[%s] ICE stats: no candidate pairs", role)
		return
	}
	logged := 0
	for _, pair := range pairs {
		if !includeAll && !pair.Nominated {
			continue
		}
		local := candidates[pair.LocalCandidateID]
		remote := candidates[pair.RemoteCandidateID]
		log.Printf("[%s] ICE pair state=%s nominated=%t rtt=%.3fs sent=%d recv=%d local=%s %s:%d remote=%s %s:%d",
			role, pair.State, pair.Nominated, pair.CurrentRoundTripTime, pair.PacketsSent, pair.PacketsReceived,
			local.CandidateType, local.IP, local.Port, remote.CandidateType, remote.IP, remote.Port)
		logged++
	}
	if logged == 0 {
		log.Printf("[%s] ICE stats: %d pairs, none nominated", role, len(pairs))
	}
}

// readViewerRTCP drains RTCP so NACK retransmission runs, and logs the
// feedback that says whether the viewer is losing video.
func readViewerRTCP(id int, sender *webrtc.RTPSender) {
	role := fmt.Sprintf("viewer %d", id)
	buf := make([]byte, 1500)
	var pli, nackMessages, nackPackets int
	var lastPLI, lastNack, lastRR time.Time
	log.Printf("[%s] reading RTCP", role)
	for {
		n, _, err := sender.Read(buf)
		if err != nil {
			log.Printf("[%s] RTCP read ended: %v (pli=%d nackMessages=%d nackPackets=%d)", role, err, pli, nackMessages, nackPackets)
			return
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			log.Printf("[%s] RTCP unmarshal (%d bytes): %v", role, n, err)
			continue
		}
		for _, pkt := range packets {
			switch p := pkt.(type) {
			case *rtcp.PictureLossIndication:
				pli++
				if pli == 1 || time.Since(lastPLI) >= rtcpReportEvery {
					log.Printf("[%s] PLI count=%d mediaSSRC=%d", role, pli, p.MediaSSRC)
					lastPLI = time.Now()
				}
			case *rtcp.FullIntraRequest:
				log.Printf("[%s] FIR senderSSRC=%d entries=%d", role, p.SenderSSRC, len(p.FIR))
			case *rtcp.TransportLayerNack:
				nackMessages++
				lost := 0
				for i := range p.Nacks {
					lost += len(p.Nacks[i].PacketList())
				}
				nackPackets += lost
				if nackMessages == 1 || time.Since(lastNack) >= rtcpReportEvery {
					log.Printf("[%s] NACK messages=%d lostPackets=%d latestMediaSSRC=%d latestLost=%d",
						role, nackMessages, nackPackets, p.MediaSSRC, lost)
					lastNack = time.Now()
				}
			case *rtcp.ReceiverReport:
				if len(p.Reports) == 0 || (lastRR != (time.Time{}) && time.Since(lastRR) < rtcpReportEvery) {
					continue
				}
				lastRR = time.Now()
				for _, report := range p.Reports {
					log.Printf("[%s] RR ssrc=%d fractionLost=%d/256 totalLost=%d jitter=%d",
						role, report.SSRC, report.FractionLost, report.TotalLost, report.Jitter)
				}
			case *rtcp.Goodbye:
				log.Printf("[%s] RTCP BYE sources=%v reason=%q", role, p.Sources, p.Reason)
			}
		}
	}
}

func summarizeSDP(sdp string) string {
	var parts []string
	var media string
	flush := func() {
		if media != "" {
			parts = append(parts, media)
			media = ""
		}
	}
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "m="):
			flush()
			media = line
		case media != "" && (strings.HasPrefix(line, "a=sendrecv") ||
			strings.HasPrefix(line, "a=sendonly") ||
			strings.HasPrefix(line, "a=recvonly") ||
			strings.HasPrefix(line, "a=inactive") ||
			strings.HasPrefix(line, "a=mid:")):
			media += " " + strings.TrimPrefix(line, "a=")
		}
	}
	flush()
	if len(parts) == 0 {
		return "no m-lines"
	}
	return strings.Join(parts, " | ")
}

func preview(s string) string {
	s = strings.ReplaceAll(s, "\n", `\n`)
	const max = 48
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
