// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js

// sfu-ws is a many-to-many websocket based SFU.
//
// Architecture (WebRTC selective forwarding):
//
//	Each browser opens one PeerConnection to this process. The browser is a
//	WebRTC endpoint; this process is the other endpoint for every participant.
//	Media is not meshed between browsers and is not mixed (that would be an MCU).
//	The server terminates ICE, DTLS, and SRTP for each peer, then forwards the
//	decrypted RTP packets to the other peers, re-encrypting per subscriber.
//	That packet copy, without decode or transcode, is the SFU data path.
//
//	WebRTC does not define signaling. Offers, answers, and ICE candidates are
//	carried on a WebSocket that this file also serves. Port 8080 is signaling
//	only. RTP runs on ephemeral UDP ports chosen by ICE.
//
//	The server is always the JSEP offerer. The browser in index.html only
//	creates answers. Renegotiation is server-driven, so the two sides never
//	glare (both offering at once).
package main

import (
	// encoding/json turns JSEP objects (SessionDescription, ICECandidateInit)
	// into the text payloads carried on the signaling channel.
	"encoding/json"
	// flag reads the TCP address the signaling HTTP server binds.
	"flag"
	// net/http serves the demo page and the WebSocket signaling endpoint.
	// The media path does not use this server.
	"net/http"
	// os reads index.html, the browser client that owns the other
	// RTCPeerConnection.
	"os"
	// sync protects the conference tables. Signaling, ICE callbacks, and the
	// RTP read loops share them, and gorilla's websocket.Conn must not be
	// written from two goroutines.
	"sync"
	// text/template injects the signaling URL into the browser page.
	"text/template"
	// time drives periodic RTCP PLI (keyframe requests) and the renegotiation
	// backoff.
	"time"

	// gorilla/websocket is the signaling transport. It is not part of the
	// WebRTC stack; it only carries SDP and ICE.
	"github.com/gorilla/websocket"
	// pion/logging is the SFU's own log, separate from Pion's internal
	// ICE/DTLS/SRTP logs (enable those with PION_LOG_*).
	"github.com/pion/logging"
	// pion/rtcp builds RTCP feedback packets. This SFU sends Picture Loss
	// Indication so publishers emit a keyframe.
	"github.com/pion/rtcp"
	// pion/rtp parses an inbound RTP packet so the SFU can strip header
	// extensions before forwarding it.
	"github.com/pion/rtp"
	// pion/webrtc/v4 is the server-side WebRTC stack: PeerConnection, ICE,
	// DTLS-SRTP, transceivers, and tracks.
	"github.com/pion/webrtc/v4"
)

// nolint
var (
	// addr is the signaling listen address. It is not the RTP port.
	// WebRTC: signaling and media are different transports.
	addr = flag.String("addr", ":8080", "http service address")
	// upgrader turns an HTTP request into the WebSocket used as the
	// signaling channel for one participant.
	upgrader = websocket.Upgrader{
		// CheckOrigin accepts every browser Origin.
		// WebRTC: signaling is application-defined, including who may open it.
		// A production SFU would restrict Origin; this demo does not.
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	// indexTemplate is the parsed browser client. The template slot is the
	// WebSocket URL the page assigns to `new WebSocket(...)`.
	indexTemplate = &template.Template{}

	// listLock guards peerConnections and trackLocals.
	// WebRTC: conference membership (who has a PeerConnection) and the
	// forwarding table (which RTP tracks exist) are shared by the signaling
	// goroutines and the per-track RTP read loops. Renegotiation must see a
	// consistent snapshot of both.
	listLock sync.RWMutex
	// peerConnections is one entry per participant.
	// WebRTC: SFU topology is a star. Each browser has a single
	// PeerConnection to the SFU, not one PeerConnection per other browser
	// (that would be a mesh).
	peerConnections []peerConnectionState
	// trackLocals is the SFU forwarding table.
	// Key: MediaStreamTrack id of a track some peer is publishing.
	// Value: the local RTP track the SFU writes those packets into, which
	// every other peer's RTPSender then encrypts and sends.
	// WebRTC: TrackLocalStaticRTP is an outbound track the server feeds with
	// already-encoded RTP. No decode step sits in between, so VP8 and H264
	// (and any other negotiated codec) can share one conference.
	trackLocals map[string]*webrtc.TrackLocalStaticRTP

	// log is the application logger named "sfu-ws".
	log = logging.NewDefaultLoggerFactory().NewLogger("sfu-ws")
)

// websocketMessage is one signaling envelope.
// WebRTC: the offer/answer and ICE candidate formats are specified (JSEP),
// but the framing that carries them is not. This demo uses {event, data}
// where data is a second JSON document (the SDP or the candidate).
type websocketMessage struct {
	// Event selects the signaling verb: "offer", "answer", or "candidate".
	Event string `json:"event"`
	// Data is the JSON text of a SessionDescription or an ICECandidateInit.
	Data string `json:"data"`
}

// peerConnectionState binds one media session to the signaling socket that
// drives it.
// WebRTC: a PeerConnection does not know how to reach the remote peer's
// signaling stack. The application keeps that association.
type peerConnectionState struct {
	// peerConnection is the SFU's side of one browser's RTCPeerConnection:
	// ICE agent, DTLS transport, SRTP, and the set of transceivers.
	peerConnection *webrtc.PeerConnection
	// websocket is the signaling channel for that same browser.
	websocket *threadSafeWriter
}

func main() {
	// Parse -addr. This only configures the signaling listener.
	flag.Parse()

	// The forwarding table starts empty: nobody is publishing yet.
	// WebRTC: an SFU has no media of its own; tracks appear only when a
	// remote peer's RTP arrives (see OnTrack).
	trackLocals = map[string]*webrtc.TrackLocalStaticRTP{}

	// Load the browser client from disk.
	// WebRTC: index.html owns the other endpoint (getUserMedia,
	// RTCPeerConnection, createAnswer, addIceCandidate).
	indexHTML, err := os.ReadFile("index.html")
	if err != nil {
		panic(err)
	}
	// Compile the page so each request can receive its signaling URL.
	indexTemplate = template.Must(template.New("").Parse(string(indexHTML)))

	// Signaling endpoint. One HTTP upgrade becomes one participant.
	http.HandleFunc("/websocket", websocketHandler)

	// Demo page. The template value is the WebSocket URL the browser opens.
	// WebRTC: the page must learn the signaling URL out of band; it is not
	// inside SDP. "ws://" matches this plain-HTTP server (wss:// would be
	// the secure-page equivalent).
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if err = indexTemplate.Execute(w, "ws://"+r.Host+"/websocket"); err != nil {
			log.Errorf("Failed to parse index template: %v", err)
		}
	})

	// Ask every publisher for a keyframe on a fixed interval.
	// WebRTC: video codecs send one independently decodable frame (VP8
	// keyframe, H.264 IDR) followed by delta frames. A subscriber that
	// joined mid-stream, or that lost a packet, cannot decode until the next
	// keyframe. The SFU does not decode, so it cannot synthesize one; it
	// asks the sender via RTCP. See dispatchKeyFrame.
	go func() {
		for range time.NewTicker(time.Second * 3).C {
			dispatchKeyFrame()
		}
	}()

	// Blocking signaling server. Returning means the process can no longer
	// accept PeerConnections. RTP ports are chosen later by each ICE agent,
	// not by this listener.
	if err = http.ListenAndServe(*addr, nil); err != nil { //nolint: gosec
		log.Errorf("Failed to start http server: %v", err)
	}
}

// addTrack publishes one inbound track into the conference.
//
// WebRTC: when a remote track arrives, the SFU creates an outbound track with
// the same codec and the same MediaStream ids, then renegotiates so every
// other PeerConnection gains an RTPSender for it. That is "selective
// forwarding": the packet is copied, not mixed and not transcoded.
func addTrack(t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP { // nolint
	// Hold the conference lock across the forwarding-table update.
	listLock.Lock()
	// Unlock before renegotiating.
	// signalPeerConnections takes the same lock, and it must observe the
	// track we just inserted. Renegotiation is the WebRTC offer/answer pass
	// that tells other browsers this media now exists.
	defer func() {
		listLock.Unlock()
		signalPeerConnections()
	}()

	// Build the outbound track from the inbound codec and ids.
	// t.Codec().RTPCodecCapability is the negotiated codec (mime type, clock
	// rate, channels, fmtp). Copying it is why one room can carry H264 and
	// VP8 at once: each track keeps the codec it was published with.
	// t.ID() is the MediaStreamTrack id. t.StreamID() is the MediaStream id.
	// Preserving both lets the browser group this publisher's audio and video
	// into one stream (one participant), which is what index.html plays.
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		panic(err)
	}

	// Insert into the forwarding table under the track id. signalPeerConnections
	// walks this map and AddTrack's anything a given peer is not already sending.
	trackLocals[t.ID()] = trackLocal

	// The caller (OnTrack) writes RTP into this track for the life of the
	// inbound stream.
	return trackLocal
}

// removeTrack drops a published track and renegotiates.
//
// WebRTC: the publisher stopped (PeerConnection closed, or the RTP read
// failed). Other peers must be offered an SDP that no longer sends this
// track. Under Unified Plan the m-line is not deleted — m-line order and
// a=mid stay stable for the life of the PeerConnection — the next offer
// stops the corresponding RTPSender.
func removeTrack(t *webrtc.TrackLocalStaticRTP) {
	listLock.Lock()
	defer func() {
		listLock.Unlock()
		signalPeerConnections()
	}()

	// Key matches addTrack, which stored the track under its MediaStreamTrack id.
	delete(trackLocals, t.ID())
}

// signalPeerConnections makes every PeerConnection's outbound tracks match
// trackLocals, then performs an offer/answer renegotiation.
//
// WebRTC: the set of RTPSenders is part of the session description. Adding or
// removing a track does not take effect on the wire until the offerer creates
// a new SDP offer, sets it as the local description, and the answerer applies
// it (the browser in index.html does setRemoteDescription + createAnswer).
// This SFU is always the offerer, including for the first negotiation.
func signalPeerConnections() { // nolint
	listLock.Lock()
	// After the offers are queued, request a keyframe.
	// WebRTC: a browser that just received a new video m-line cannot render
	// until an intra frame arrives. PLI asks each publisher to emit one now
	// rather than waiting for the encoder's normal keyframe interval.
	defer func() {
		listLock.Unlock()
		dispatchKeyFrame()
	}()

	// attemptSync walks every peer once.
	// tryAgain is true when the peer slice was mutated or a negotiation step
	// failed; the caller then restarts from index 0. A false return means one
	// clean pass finished (the named result defaults to false).
	attemptSync := func() (tryAgain bool) {
		for i := range peerConnections {
			// PeerConnectionStateClosed is terminal: ICE and DTLS are gone
			// and this peer will never send or receive again. Drop it from
			// the conference. The slice compaction invalidates i, so the
			// walk restarts.
			if peerConnections[i].peerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed {
				peerConnections = append(peerConnections[:i], peerConnections[i+1:]...)

				return true // We modified the slice, start from the beginning
			}

			// existingSenders is the set of track ids this peer must not be
			// given again: tracks it already sends, plus tracks it publishes
			// itself (those must not be looped back).
			existingSenders := map[string]bool{}

			// GetSenders returns every RTPSender on this PeerConnection,
			// including senders that belong to recvonly transceivers and
			// therefore have no track attached.
			for _, sender := range peerConnections[i].peerConnection.GetSenders() {
				// Recvonly transceivers (the ones created in websocketHandler)
				// have an RTPSender with a nil track: the SFU receives on that
				// m-line and does not send on it. Skip those.
				if sender.Track() == nil {
					continue
				}

				// This track id is already leaving toward the browser.
				existingSenders[sender.Track().ID()] = true

				// The sender is still live, but the publisher left and
				// removeTrack deleted the forwarding entry.
				// WebRTC: RemoveTrack stops that RTPSender. The offer built
				// below tells the browser to stop playing it.
				if _, ok := trackLocals[sender.Track().ID()]; !ok {
					if err := peerConnections[i].peerConnection.RemoveTrack(sender); err != nil {
						return true
					}
				}
			}

			// Do not send a peer its own published media.
			// WebRTC: GetReceivers lists inbound RTP (what this browser is
			// publishing). The browser already renders that locally from
			// getUserMedia; echoing it would waste bandwidth and can loop.
			// The remote track id equals the id addTrack stored, so marking
			// it here suppresses AddTrack for the publisher only.
			for _, receiver := range peerConnections[i].peerConnection.GetReceivers() {
				if receiver.Track() == nil {
					continue
				}

				existingSenders[receiver.Track().ID()] = true
			}

			// Subscribe this peer to every published track it does not
			// already send and did not publish.
			// WebRTC: AddTrack attaches an RTPSender and marks the
			// PeerConnection negotiation-needed. The m-line created for it
			// is sendonly from the SFU's point of view (the browser answers
			// recvonly and plays the stream).
			for trackID := range trackLocals {
				if _, ok := existingSenders[trackID]; !ok {
					if _, err := peerConnections[i].peerConnection.AddTrack(trackLocals[trackID]); err != nil {
						return true
					}
				}
			}

			// CreateOffer builds an SDP offer from the current transceivers:
			// codec lists, directions, a=mid, ICE ufrag/pwd, DTLS
			// fingerprint, and the SSRC/msid of each track we send.
			// nil options means no ICE restart and no other JSEP overrides.
			// WebRTC: this PeerConnection is the offerer (JSEP).
			offer, err := peerConnections[i].peerConnection.CreateOffer(nil)
			if err != nil {
				return true
			}

			// SetLocalDescription commits the offer.
			// WebRTC: signaling state becomes have-local-offer. ICE gathering
			// for any new credentials starts here; candidates found afterward
			// are trickled through OnICECandidate rather than waiting to be
			// pasted into this SDP (trickle ICE, not vanilla ICE).
			if err = peerConnections[i].peerConnection.SetLocalDescription(offer); err != nil {
				return true
			}

			// Serialize the SessionDescription (type "offer" + SDP text) into
			// the signaling envelope's data field.
			offerString, err := json.Marshal(offer)
			if err != nil {
				log.Errorf("Failed to marshal offer to json: %v", err)

				return true
			}

			log.Infof("Send offer to client: %v", offer)

			// Deliver the offer on this peer's signaling socket.
			// The browser applies it with setRemoteDescription, createAnswer,
			// setLocalDescription, and sends the answer back (see the "answer"
			// case in websocketHandler).
			if err = peerConnections[i].websocket.WriteJSON(&websocketMessage{
				Event: "offer",
				Data:  string(offerString),
			}); err != nil {
				return true
			}
		}

		return tryAgain
	}

	// Retry while peers close or negotiation steps fail under us.
	// WebRTC operations are ordered per PeerConnection (the signaling state
	// machine). CreateOffer fails if a previous offer has not been answered
	// yet; a peer can also hit Closed between GetSenders and SetLocalDescription.
	// Both surface as tryAgain.
	for syncAttempt := 0; ; syncAttempt++ {
		if syncAttempt == 25 {
			// Give up the lock and try the whole sync later.
			// Holding listLock any longer would stall addTrack, removeTrack,
			// and dispatchKeyFrame, which need the same lock. The 3s delay is
			// this example's backoff, not a WebRTC timer.
			go func() {
				time.Sleep(time.Second * 3)
				signalPeerConnections()
			}()

			return
		}

		if !attemptSync() {
			break
		}
	}
}

// dispatchKeyFrame asks every publisher to emit an intra frame.
//
// WebRTC: RTCP is the control protocol paired with RTP. Picture Loss
// Indication (RFC 4585, payload-specific feedback) tells a sender "I cannot
// decode; send a keyframe." The SFU writes that RTCP toward the publishing
// PeerConnection. The browser's encoder generates the keyframe, the RTP read
// loop forwards it, and every subscriber can resync. The SFU never generates
// the frame itself because it does not decode.
func dispatchKeyFrame() {
	listLock.Lock()
	defer listLock.Unlock()

	for i := range peerConnections {
		// Each RTPReceiver is one inbound m-line (the audio or video this
		// browser is publishing toward the SFU).
		for _, receiver := range peerConnections[i].peerConnection.GetReceivers() {
			// No RTP bound yet (transceiver added, but no packet has arrived,
			// or the sender is not attached). A PLI needs an SSRC, so skip.
			if receiver.Track() == nil {
				continue
			}

			// WriteRTCP sends an RTCP packet on this PeerConnection toward
			// the browser. MediaSSRC is the inbound RTP synchronization
			// source; the browser matches it to the encoder for that track.
			// The error is ignored: a closed peer just misses this round.
			_ = peerConnections[i].peerConnection.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					MediaSSRC: uint32(receiver.Track().SSRC()),
				},
			})
		}
	}
}

// websocketHandler is the life cycle of one participant.
//
// WebRTC sequence for a peer that reaches this handler:
//  1. Signaling socket opens (this upgrade).
//  2. SFU creates a PeerConnection with recvonly audio and video transceivers.
//  3. signalPeerConnections sends the initial SDP offer.
//  4. Browser answers; both sides trickle ICE candidates.
//  5. ICE connects, DTLS handshakes, SRTP keys are derived.
//  6. Browser RTP arrives, OnTrack fires, the track is forwarded, and a
//     renegotiation offers that track to everyone else.
func websocketHandler(w http.ResponseWriter, r *http.Request) { // nolint
	// Complete the HTTP upgrade. From here the socket carries signaling only.
	unsafeConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Errorf("Failed to upgrade HTTP to Websocket: ", err)

		return
	}

	// Wrap the socket so ICE-candidate writes and offer writes can run on
	// different goroutines. gorilla/websocket allows one concurrent reader
	// and requires writers to be serialized.
	c := &threadSafeWriter{unsafeConn, sync.Mutex{}} // nolint

	// Leaving this function means the participant is gone. Closing the
	// signaling socket drops any future offer/candidate delivery.
	defer c.Close() //nolint

	// New PeerConnection with an empty Configuration.
	// WebRTC: ICEServers is unset, so there is no STUN or TURN server and the
	// ICE agent has only host candidates. That is enough for localhost and
	// same-LAN. A public SFU would set ICEServers (stun: and turn:) so peers
	// behind NAT can still connect. Other fields keep Pion defaults,
	// including Unified Plan (one m-line per transceiver).
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Errorf("Failed to creates a PeerConnection: %v", err)

		return
	}

	// Close releases ICE, DTLS, and SRTP and moves the connection to
	// PeerConnectionStateClosed, which the state handler uses to renegotiate
	// the rest of the room.
	defer peerConnection.Close() //nolint

	// Pre-declare one inbound video m-line and one inbound audio m-line.
	// WebRTC: an RTPTransceiver pairs an RTPSender and an RTPReceiver.
	// Direction recvonly means the offer says a=recvonly: the SFU is willing
	// to receive that media and has nothing to send on this m-line yet.
	// The browser's answer flips its side to sendonly and attaches the
	// getUserMedia tracks (see index.html, addTrack on the client).
	// Without these transceivers the first offer would contain no media
	// section for the browser to send into.
	for _, typ := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := peerConnection.AddTransceiverFromKind(typ, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			log.Errorf("Failed to add transceiver: %v", err)

			return
		}
	}

	// Join the conference. signalPeerConnections, the keyframe loop, and the
	// track add/remove path all iterate this slice.
	listLock.Lock()
	peerConnections = append(peerConnections, peerConnectionState{peerConnection, c})
	listLock.Unlock()

	// Trickle ICE: the local ICE agent reports each candidate as it is
	// discovered, instead of waiting until gathering completes (vanilla ICE)
	// and stuffing every candidate into the SDP.
	// A nil candidate is the end-of-candidates signal. Connectivity does not
	// depend on forwarding that marker, so it is ignored.
	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			return
		}
		// ICECandidateInit is the JSEP object the browser passes to
		// addIceCandidate: the a=candidate line plus sdpMid or sdpMLineIndex
		// so the candidate is bound to the correct m-line.
		// ToJSON produces that object. Marshaling *ICECandidate itself uses
		// different fields and breaks sdpMid, so the browser would reject it.
		candidateString, err := json.Marshal(i.ToJSON())
		if err != nil {
			log.Errorf("Failed to marshal candidate to json: %v", err)

			return
		}

		log.Infof("Send candidate to client: %s", candidateString)

		// Signaling event "candidate". The browser calls addIceCandidate.
		if writeErr := c.WriteJSON(&websocketMessage{
			Event: "candidate",
			Data:  string(candidateString),
		}); writeErr != nil {
			log.Errorf("Failed to write JSON: %v", writeErr)
		}
	})

	// Peer connection state is the combined ICE + DTLS session state
	// (new, connecting, connected, disconnected, failed, closed).
	// It is coarser than the ICE connection state logged below.
	peerConnection.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		log.Infof("Connection state change: %s", p)

		switch p {
		// Failed means ICE or DTLS could not establish (or recover) the
		// session. Close moves it to the terminal Closed state.
		case webrtc.PeerConnectionStateFailed:
			if err := peerConnection.Close(); err != nil {
				log.Errorf("Failed to close PeerConnection: %v", err)
			}
		// Closed: drop this peer's senders from everyone else. The RTP read
		// loop also errors, and its defer runs removeTrack for each track
		// this peer had published.
		case webrtc.PeerConnectionStateClosed:
			signalPeerConnections()
		default:
		}
	})

	// OnTrack fires once per inbound RTP stream, after the answer is applied
	// and SRTP has delivered a packet.
	// t is the remote track (decrypted RTP from the browser).
	// The RTPReceiver is unused; PLI uses GetReceivers later, and the read
	// loop uses the track directly.
	// WebRTC: this callback is the SFU's ingest point. Everything below is
	// the forward path.
	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		// Kind is audio or video. ID is the MediaStreamTrack id.
		// PayloadType is the RTP payload type number negotiated for the codec
		// on this m-line (dynamic, in the 96-127 range for WebRTC codecs).
		log.Infof("Got remote track: Kind=%s, ID=%s, PayloadType=%d", t.Kind(), t.ID(), t.PayloadType())

		// Publish into the conference and renegotiate the other peers.
		// trackLocal is the outbound track those peers' RTPSenders read from.
		trackLocal := addTrack(t)
		// When this read loop ends (publisher hung up, or the PeerConnection
		// closed), unpublish and renegotiate so subscribers stop receiving it.
		defer removeTrack(trackLocal)

		// One RTP packet per Read. 1500 bytes is the Ethernet MTU, which is
		// large enough for a single SRTP payload after the stack has already
		// decrypted it. The application never sees DTLS or SRTP framing;
		// PeerConnection terminates that and hands up RTP.
		buf := make([]byte, 1500)
		rtpPkt := &rtp.Packet{}

		for {
			// Block until the next inbound RTP packet.
			// WebRTC media path: browser encoder -> RTP -> SRTP -> DTLS ->
			// ICE/UDP -> this PeerConnection decrypts -> Read returns RTP.
			i, _, err := t.Read(buf)
			if err != nil {
				return
			}

			// Decode the RTP header and payload. Forwarding a structured
			// packet (instead of the raw bytes) lets us edit the header and
			// lets TrackLocalStaticRTP remap payload type and SSRC per
			// subscriber.
			if err = rtpPkt.Unmarshal(buf[:i]); err != nil {
				log.Errorf("Failed to unmarshal incoming RTP packet: %v", err)

				return
			}

			// Drop RTP header extensions.
			// WebRTC negotiates extensions per PeerConnection (mid, rtp-stream-id,
			// abs-send-time, transport-cc, audio-level, ...). The id numbers and
			// the set of extensions on the publisher's m-line are not the same
			// as on each subscriber's m-line. Forwarding them would attach the
			// publisher's extension ids to a session that negotiated different
			// ones. Clearing both the one-byte and the general extension list
			// sends a bare RTP packet; each outbound sender adds the extensions
			// it actually negotiated.
			rtpPkt.Extension = false
			rtpPkt.Extensions = nil

			// Hand the packet to every RTPSender that AddTrack attached to
			// this local track.
			// WebRTC: this is the selective forward. WriteRTP rewrites SSRC
			// and payload type to the values advertised in that subscriber's
			// SDP, then the PeerConnection SRTP-protects and ICE-sends it.
			// There is still no decode. A write error means a subscriber's
			// sender has gone away; end this forward loop (removeTrack runs
			// via defer).
			if err = trackLocal.WriteRTP(rtpPkt); err != nil {
				return
			}
		}
	})

	// ICE connection state is only the connectivity-check layer
	// (new, checking, connected, completed, disconnected, failed, closed),
	// underneath DTLS. Logged so a failed gathering/check is visible even
	// when the broader PeerConnection state has not moved yet.
	peerConnection.OnICEConnectionStateChange(func(is webrtc.ICEConnectionState) {
		log.Infof("ICE connection state changed: %s", is)
	})

	// Initial offer, and also the offer that subscribes this new peer to
	// tracks already in the room. Same function as later renegotiations.
	signalPeerConnections()

	// Signaling read loop for the life of the socket. The browser sends
	// only answers and ICE candidates; it never offers (the SFU is the
	// offerer).
	message := &websocketMessage{}
	for {
		// One WebSocket text/binary frame. gorilla delivers the payload in raw.
		_, raw, err := c.ReadMessage()
		if err != nil {
			log.Errorf("Failed to read message: %v", err)

			return
		}

		log.Infof("Got message: %s", raw)

		// Outer envelope: which signaling verb, and a JSON string payload.
		if err := json.Unmarshal(raw, &message); err != nil {
			log.Errorf("Failed to unmarshal json to message: %v", err)

			return
		}

		switch message.Event {
		// Remote ICE candidate, trickled by the browser's onicecandidate.
		case "candidate":
			// ICECandidateInit: candidate line, sdpMid, sdpMLineIndex.
			candidate := webrtc.ICECandidateInit{}
			if err := json.Unmarshal([]byte(message.Data), &candidate); err != nil {
				log.Errorf("Failed to unmarshal json to candidate: %v", err)

				return
			}

			log.Infof("Got candidate: %v", candidate)

			// Add the remote candidate to this peer's ICE agent.
			// WebRTC: the agent pairs it with local candidates and runs
			// connectivity checks. If the answer has not been applied yet,
			// the stack queues the candidate until the remote description
			// (which carries ice-ufrag, ice-pwd, and m-line indexes) exists.
			if err := peerConnection.AddICECandidate(candidate); err != nil {
				log.Errorf("Failed to add ICE candidate: %v", err)

				return
			}
		// SDP answer from the browser, in reply to an offer we sent.
		case "answer":
			// SessionDescription with Type "answer" and the SDP body.
			answer := webrtc.SessionDescription{}
			if err := json.Unmarshal([]byte(message.Data), &answer); err != nil {
				log.Errorf("Failed to unmarshal json to answer: %v", err)

				return
			}

			log.Infof("Got answer: %v", answer)

			// SetRemoteDescription completes this offer/answer exchange.
			// WebRTC: signaling state returns to stable. The answer selects
			// codecs, directions, and SSRCs. ICE can now form valid pairs
			// (both sides know ufrag/pwd), DTLS can handshake using the
			// fingerprint in the SDP, and only then does RTP flow.
			if err := peerConnection.SetRemoteDescription(answer); err != nil {
				log.Errorf("Failed to set remote description: %v", err)

				return
			}
		default:
			log.Errorf("unknown message: %+v", message)
		}
	}
}

// threadSafeWriter serializes signaling writes.
// WebRTC: OnICECandidate and signalPeerConnections both write SDP or
// candidates to the same socket, from different goroutines. The media path
// does not use this type; only the signaling channel does.
type threadSafeWriter struct {
	// Conn is the underlying WebSocket. ReadMessage and Close are promoted
	// and used only on the handler goroutine.
	*websocket.Conn
	// Mutex covers WriteJSON. Embedding promotes Lock and Unlock.
	sync.Mutex
}

// WriteJSON sends one signaling envelope with the writers mutually excluded.
func (t *threadSafeWriter) WriteJSON(v any) error {
	t.Lock()
	defer t.Unlock()

	return t.Conn.WriteJSON(v)
}
