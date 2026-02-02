// Copyright (c) 2023-2025 RapidaAI
// WebRTC Gateway - Bridges WebRTC clients to assistant-api via gRPC
// Audio flows: Browser → WebRTC → this gateway → gRPC → assistant-api → gRPC → this gateway → WebRTC → Browser

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	pw "github.com/pion/webrtc/v4"
	"github.com/rapidaai/protos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Configuration
var (
	assistantAPIAddr = getEnv("ASSISTANT_API_ADDR", "localhost:9007")
	httpPort         = getEnv("HTTP_PORT", "8088")
	assistantID      = getEnvUint64("ASSISTANT_ID", 1)
	apiKey           = getEnv("API_KEY", "")
	projectID        = getEnv("PROJECT_ID", "")
)

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvUint64(key string, defaultVal uint64) uint64 {
	if v := os.Getenv(key); v != "" {
		var val uint64
		fmt.Sscanf(v, "%d", &val)
		return val
	}
	return defaultVal
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// SignalingMessage for WebSocket signaling
type SignalingMessage struct {
	Type      string        `json:"type"`
	SDP       string        `json:"sdp,omitempty"`
	Candidate *ICECandidate `json:"candidate,omitempty"`
	SessionID string        `json:"sessionId,omitempty"`
	Error     string        `json:"error,omitempty"`
	// Configuration from client
	AssistantID uint64            `json:"assistantId,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type ICECandidate struct {
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdpMid"`
	SDPMLineIndex int    `json:"sdpMLineIndex"`
}

// Session represents a WebRTC session bridged to assistant-api
type Session struct {
	id          string
	pc          *pw.PeerConnection
	ws          *websocket.Conn
	grpcStream  protos.TalkService_AssistantTalkClient
	grpcConn    *grpc.ClientConn
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	audioTrack  *pw.TrackLocalStaticRTP
	inputBuffer []byte
	bufferMu    sync.Mutex
	configured  bool
	assistantID uint64
}

func main() {
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/sdk-test", sdkTestHandler)
	http.HandleFunc("/ws", wsHandler)

	log.Printf("WebRTC Gateway starting on http://localhost:%s", httpPort)
	log.Printf("Connecting to assistant-api at %s", assistantAPIAddr)
	log.Printf("Default assistant ID: %d", assistantID)
	log.Fatal(http.ListenAndServe(":"+httpPort, nil))
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	session := &Session{
		id:          fmt.Sprintf("session-%d", time.Now().UnixNano()),
		ws:          ws,
		ctx:         ctx,
		cancel:      cancel,
		assistantID: assistantID,
	}

	log.Printf("[%s] New WebRTC session", session.id)

	defer session.Close()

	// Create WebRTC peer connection
	if err := session.createPeerConnection(); err != nil {
		log.Printf("[%s] Failed to create peer connection: %v", session.id, err)
		session.sendError("Failed to create peer connection")
		return
	}

	// Handle WebSocket messages (signaling)
	session.handleSignaling()
}

func (s *Session) createPeerConnection() error {
	// Create MediaEngine with Opus codec
	m := &pw.MediaEngine{}
	if err := m.RegisterCodec(pw.RTPCodecParameters{
		RTPCodecCapability: pw.RTPCodecCapability{
			MimeType:    pw.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, pw.RTPCodecTypeAudio); err != nil {
		return err
	}

	// Create interceptor registry for RTCP
	i := &interceptor.Registry{}
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		return err
	}
	i.Add(intervalPliFactory)

	// Create API with MediaEngine
	api := pw.NewAPI(pw.WithMediaEngine(m), pw.WithInterceptorRegistry(i))

	// Create peer connection
	pc, err := api.NewPeerConnection(pw.Configuration{
		ICEServers: []pw.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
		},
	})
	if err != nil {
		return err
	}
	s.pc = pc

	// Create output track for sending audio to client
	s.audioTrack, err = pw.NewTrackLocalStaticRTP(
		pw.RTPCodecCapability{MimeType: pw.MimeTypeOpus},
		"audio",
		"rapida-assistant",
	)
	if err != nil {
		return err
	}

	// Add output track to peer connection
	if _, err := pc.AddTrack(s.audioTrack); err != nil {
		return err
	}

	// Handle incoming audio track from client
	pc.OnTrack(func(track *pw.TrackRemote, receiver *pw.RTPReceiver) {
		log.Printf("[%s] Received audio track: %s", s.id, track.Codec().MimeType)
		go s.handleIncomingAudio(track)
	})

	// Handle ICE candidates
	pc.OnICECandidate(func(c *pw.ICECandidate) {
		if c == nil {
			return
		}
		candidateJSON := c.ToJSON()
		s.mu.Lock()
		s.ws.WriteJSON(SignalingMessage{
			Type: "ice_candidate",
			Candidate: &ICECandidate{
				Candidate:     candidateJSON.Candidate,
				SDPMid:        *candidateJSON.SDPMid,
				SDPMLineIndex: int(*candidateJSON.SDPMLineIndex),
			},
		})
		s.mu.Unlock()
	})

	// Handle connection state changes
	pc.OnConnectionStateChange(func(state pw.PeerConnectionState) {
		log.Printf("[%s] Connection state: %s", s.id, state.String())

		if state == pw.PeerConnectionStateConnected {
			log.Printf("[%s] WebRTC connected! Starting assistant conversation...", s.id)
			go s.startAssistantConversation()
		} else if state == pw.PeerConnectionStateFailed || state == pw.PeerConnectionStateDisconnected {
			s.cancel()
		}
	})

	return nil
}

func (s *Session) handleSignaling() {
	for {
		_, data, err := s.ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[%s] WebSocket closed: %v", s.id, err)
			}
			return
		}

		var msg SignalingMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("[%s] Invalid message: %v", s.id, err)
			continue
		}

		switch msg.Type {
		case "offer":
			s.handleOffer(msg)
		case "answer":
			s.handleAnswer(msg)
		case "ice_candidate":
			s.handleICECandidate(msg)
		case "configure":
			s.handleConfigure(msg)
		}
	}
}

func (s *Session) handleOffer(msg SignalingMessage) {
	if msg.SDP == "" {
		return
	}

	// Set assistant ID if provided
	if msg.AssistantID != 0 {
		s.assistantID = msg.AssistantID
	}

	if err := s.pc.SetRemoteDescription(pw.SessionDescription{
		Type: pw.SDPTypeOffer,
		SDP:  msg.SDP,
	}); err != nil {
		log.Printf("[%s] Failed to set offer: %v", s.id, err)
		s.sendError("Failed to set offer")
		return
	}

	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		log.Printf("[%s] Failed to create answer: %v", s.id, err)
		s.sendError("Failed to create answer")
		return
	}

	if err := s.pc.SetLocalDescription(answer); err != nil {
		log.Printf("[%s] Failed to set answer: %v", s.id, err)
		s.sendError("Failed to set answer")
		return
	}

	s.mu.Lock()
	s.ws.WriteJSON(SignalingMessage{
		Type: "answer",
		SDP:  answer.SDP,
	})
	s.mu.Unlock()

	log.Printf("[%s] Sent answer", s.id)
}

func (s *Session) handleAnswer(msg SignalingMessage) {
	if msg.SDP == "" {
		return
	}

	if err := s.pc.SetRemoteDescription(pw.SessionDescription{
		Type: pw.SDPTypeAnswer,
		SDP:  msg.SDP,
	}); err != nil {
		log.Printf("[%s] Failed to set answer: %v", s.id, err)
	}
}

func (s *Session) handleICECandidate(msg SignalingMessage) {
	if msg.Candidate == nil {
		return
	}

	idx := uint16(msg.Candidate.SDPMLineIndex)
	if err := s.pc.AddICECandidate(pw.ICECandidateInit{
		Candidate:     msg.Candidate.Candidate,
		SDPMid:        &msg.Candidate.SDPMid,
		SDPMLineIndex: &idx,
	}); err != nil {
		log.Printf("[%s] Failed to add ICE candidate: %v", s.id, err)
	}
}

func (s *Session) handleConfigure(msg SignalingMessage) {
	if msg.AssistantID != 0 {
		s.assistantID = msg.AssistantID
	}
	log.Printf("[%s] Configured with assistant ID: %d", s.id, s.assistantID)
}

func (s *Session) handleIncomingAudio(track *pw.TrackRemote) {
	buf := make([]byte, 1500)
	packetCount := 0

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			n, _, err := track.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Printf("[%s] Audio read error: %v", s.id, err)
				}
				return
			}

			packetCount++
			if packetCount%100 == 0 {
				log.Printf("[%s] Received %d audio packets from WebRTC", s.id, packetCount)
			}

			// Send audio to assistant-api via gRPC
			s.sendAudioToAssistant(buf[:n])
		}
	}
}

func (s *Session) startAssistantConversation() {
	// Connect to assistant-api
	conn, err := grpc.NewClient(
		assistantAPIAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Printf("[%s] Failed to connect to assistant-api: %v", s.id, err)
		s.sendError("Failed to connect to assistant")
		return
	}
	s.grpcConn = conn

	// Create TalkService client
	client := protos.NewTalkServiceClient(conn)

	// Create metadata for auth
	md := metadata.New(map[string]string{
		"x-rapida-session-id": s.id,
	})
	if apiKey != "" {
		md.Set("x-rapida-key", apiKey)
	}
	if projectID != "" {
		md.Set("x-rapida-project-id", projectID)
	}

	ctx := metadata.NewOutgoingContext(s.ctx, md)

	// Start bidirectional stream
	stream, err := client.AssistantTalk(ctx)
	if err != nil {
		log.Printf("[%s] Failed to start talk stream: %v", s.id, err)
		s.sendError("Failed to start conversation")
		return
	}
	s.grpcStream = stream

	// Send initial configuration
	configMsg := &protos.AssistantTalkInput{
		Request: &protos.AssistantTalkInput_Configuration{
			Configuration: &protos.ConversationConfiguration{
				Assistant: &protos.AssistantDefinition{
					AssistantId: s.assistantID,
				},
				InputConfig: &protos.StreamConfig{
					Audio: &protos.AudioConfig{
						SampleRate:  16000,
						AudioFormat: protos.AudioConfig_LINEAR16,
						Channels:    1,
					},
				},
				OutputConfig: &protos.StreamConfig{
					Audio: &protos.AudioConfig{
						SampleRate:  16000,
						AudioFormat: protos.AudioConfig_LINEAR16,
						Channels:    1,
					},
				},
			},
		},
	}

	if err := stream.Send(configMsg); err != nil {
		log.Printf("[%s] Failed to send config: %v", s.id, err)
		return
	}

	s.configured = true
	log.Printf("[%s] Assistant conversation started (ID: %d)", s.id, s.assistantID)

	// Handle responses from assistant
	s.handleAssistantResponses()
}

func (s *Session) sendAudioToAssistant(audioData []byte) {
	if s.grpcStream == nil || !s.configured {
		return
	}

	// Buffer audio to reduce gRPC calls
	s.bufferMu.Lock()
	s.inputBuffer = append(s.inputBuffer, audioData...)

	// Send when we have enough data (e.g., 20ms of audio at 16kHz mono = 640 bytes)
	if len(s.inputBuffer) >= 640 {
		data := s.inputBuffer
		s.inputBuffer = nil
		s.bufferMu.Unlock()

		msg := &protos.AssistantTalkInput{
			Request: &protos.AssistantTalkInput_Message{
				Message: &protos.ConversationUserMessage{
					Message: &protos.ConversationUserMessage_Audio{
						Audio: data,
					},
				},
			},
		}

		if err := s.grpcStream.Send(msg); err != nil {
			log.Printf("[%s] Failed to send audio: %v", s.id, err)
		}
	} else {
		s.bufferMu.Unlock()
	}
}

func (s *Session) handleAssistantResponses() {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			resp, err := s.grpcStream.Recv()
			if err != nil {
				if err != io.EOF {
					log.Printf("[%s] gRPC recv error: %v", s.id, err)
				}
				return
			}

			s.processAssistantResponse(resp)
		}
	}
}

func (s *Session) processAssistantResponse(resp *protos.AssistantTalkOutput) {
	switch data := resp.GetData().(type) {
	case *protos.AssistantTalkOutput_Configuration:
		log.Printf("[%s] Configuration received: conversation ID %d",
			s.id, data.Configuration.GetAssistantConversationId())

		// Notify client
		s.mu.Lock()
		s.ws.WriteJSON(SignalingMessage{
			Type:      "configured",
			SessionID: s.id,
		})
		s.mu.Unlock()

	case *protos.AssistantTalkOutput_Assistant:
		switch content := data.Assistant.Message.(type) {
		case *protos.ConversationAssistantMessage_Audio:
			// Send audio back through WebRTC
			s.sendAudioToClient(content.Audio)

		case *protos.ConversationAssistantMessage_Text:
			text := content.Text
			log.Printf("[%s] Assistant: %s", s.id, text)

			// Send transcript to client via signaling
			s.mu.Lock()
			s.ws.WriteJSON(map[string]interface{}{
				"type":      "transcript",
				"role":      "assistant",
				"text":      text,
				"completed": data.Assistant.GetCompleted(),
			})
			s.mu.Unlock()
		}

	case *protos.AssistantTalkOutput_User:
		switch content := data.User.Message.(type) {
		case *protos.ConversationUserMessage_Text:
			log.Printf("[%s] User: %s", s.id, content.Text)

			// Send transcript to client
			s.mu.Lock()
			s.ws.WriteJSON(map[string]interface{}{
				"type":      "transcript",
				"role":      "user",
				"text":      content.Text,
				"completed": data.User.GetCompleted(),
			})
			s.mu.Unlock()
		}

	case *protos.AssistantTalkOutput_Interruption:
		log.Printf("[%s] Interruption: %v", s.id, data.Interruption.GetType())

		// Notify client to clear audio
		s.mu.Lock()
		s.ws.WriteJSON(map[string]interface{}{
			"type": "interruption",
		})
		s.mu.Unlock()

	case *protos.AssistantTalkOutput_Directive:
		if data.Directive.GetType() == protos.ConversationDirective_END_CONVERSATION {
			log.Printf("[%s] Conversation ended by assistant", s.id)
			s.cancel()
		}
	}
}

func (s *Session) sendAudioToClient(audioData []byte) {
	// In a full implementation, we'd convert PCM to Opus RTP packets
	// For now, this is a placeholder
	// The actual implementation would use pion's opus encoder

	// TODO: Implement PCM -> Opus encoding and RTP packetization
	// s.audioTrack.WriteRTP(rtpPacket)

	_ = audioData // Placeholder
}

func (s *Session) sendError(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ws.WriteJSON(SignalingMessage{
		Type:  "error",
		Error: msg,
	})
}

func (s *Session) Close() {
	s.cancel()

	if s.grpcStream != nil {
		s.grpcStream.CloseSend()
	}
	if s.grpcConn != nil {
		s.grpcConn.Close()
	}
	if s.pc != nil {
		s.pc.Close()
	}
	if s.ws != nil {
		s.ws.Close()
	}

	log.Printf("[%s] Session closed", s.id)
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, indexHTML)
}

func sdkTestHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "client-sdk-test.html")
}

const indexHTML = `<!DOCTYPE html>
<html>
<head>
    <title>WebRTC Gateway - Assistant API</title>
    <style>
        body { font: 16px sans-serif; max-width: 800px; margin: 40px auto; background: #111; color: #eee; padding: 20px; }
        h2 { color: #0af; }
        .status { padding: 10px; border-radius: 5px; margin: 10px 0; }
        .connected { background: #1a4d1a; }
        .disconnected { background: #4d1a1a; }
        .connecting { background: #4d4d1a; }
        button { background: #0af; border: none; padding: 12px 24px; cursor: pointer; margin: 5px; color: #000; border-radius: 5px; }
        button:disabled { opacity: 0.5; }
        #logbox { background: #000; height: 300px; overflow-y: auto; font: 12px monospace; padding: 10px; border-radius: 5px; }
        #meter { height: 20px; background: #333; border-radius: 3px; margin: 10px 0; }
        #bar { height: 100%; background: #0f0; width: 0; border-radius: 3px; transition: width 0.1s; }
        .transcript { background: #222; padding: 10px; margin: 5px 0; border-radius: 5px; }
        .transcript.user { border-left: 3px solid #0af; }
        .transcript.assistant { border-left: 3px solid #0f0; }
        #transcripts { max-height: 200px; overflow-y: auto; }
        .config { background: #222; padding: 15px; border-radius: 5px; margin: 10px 0; }
        .config label { display: block; margin: 10px 0 5px; }
        .config input { background: #333; border: 1px solid #444; color: #eee; padding: 8px; width: 100%; border-radius: 3px; }
    </style>
</head>
<body>
    <h2>🎙️ WebRTC Gateway → Assistant API</h2>
    <p>Audio flows via WebRTC media tracks to assistant-api via gRPC</p>
    
    <div class="config">
        <label>Assistant ID</label>
        <input type="number" id="assistantId" value="1" />
    </div>
    
    <div id="status" class="status disconnected">Disconnected</div>
    
    <button onclick="connect()" id="connectBtn">Connect</button>
    <button onclick="disconnect()" id="disconnectBtn" disabled>Disconnect</button>
    
    <h4>Microphone Level</h4>
    <div id="meter"><div id="bar"></div></div>
    
    <h4>Transcripts</h4>
    <div id="transcripts"></div>
    
    <h4>Log</h4>
    <div id="logbox"></div>

<script>
let ws, pc, stream, ctx, an;
const statusEl = document.getElementById('status');
const connectBtn = document.getElementById('connectBtn');
const disconnectBtn = document.getElementById('disconnectBtn');

function log(m, type = 'info') {
    const colors = { info: '#0af', success: '#0f0', error: '#f44', warn: '#fa0' };
    document.getElementById('logbox').innerHTML += 
        '<span style="color:' + (colors[type] || '#eee') + '">[' + new Date().toLocaleTimeString() + '] ' + m + '</span><br>';
    document.getElementById('logbox').scrollTop = document.getElementById('logbox').scrollHeight;
    console.log(m);
}

function setStatus(text, state) {
    statusEl.textContent = text;
    statusEl.className = 'status ' + state;
}

function addTranscript(role, text, completed) {
    const div = document.createElement('div');
    div.className = 'transcript ' + role;
    div.innerHTML = '<strong>' + role + ':</strong> ' + text + (completed ? '' : '...');
    document.getElementById('transcripts').appendChild(div);
    document.getElementById('transcripts').scrollTop = document.getElementById('transcripts').scrollHeight;
}

async function connect() {
    try {
        setStatus('Connecting...', 'connecting');
        connectBtn.disabled = true;
        
        log('Requesting microphone...');
        stream = await navigator.mediaDevices.getUserMedia({ 
            audio: { 
                echoCancellation: true, 
                noiseSuppression: true,
                sampleRate: 48000 
            } 
        });
        log('Microphone access granted', 'success');
        startMeter();
        
        log('Connecting to WebRTC gateway...');
        ws = new WebSocket('ws://' + location.host + '/ws');
        
        ws.onopen = () => {
            log('WebSocket connected', 'success');
            setupPeerConnection();
        };
        
        ws.onerror = (e) => {
            log('WebSocket error', 'error');
            setStatus('Connection Error', 'disconnected');
        };
        
        ws.onclose = () => {
            log('WebSocket closed');
            setStatus('Disconnected', 'disconnected');
            connectBtn.disabled = false;
            disconnectBtn.disabled = true;
        };
        
        ws.onmessage = handleSignaling;
        
    } catch (e) {
        log('Error: ' + e.message, 'error');
        setStatus('Error', 'disconnected');
        connectBtn.disabled = false;
    }
}

function handleSignaling(event) {
    const msg = JSON.parse(event.data);
    log('Received: ' + msg.type);
    
    switch (msg.type) {
        case 'answer':
            pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp });
            break;
        case 'ice_candidate':
            if (msg.candidate) {
                pc.addIceCandidate(msg.candidate);
            }
            break;
        case 'configured':
            log('Assistant conversation started!', 'success');
            break;
        case 'transcript':
            addTranscript(msg.role, msg.text, msg.completed);
            break;
        case 'interruption':
            log('Interruption detected', 'warn');
            break;
        case 'error':
            log('Server error: ' + msg.error, 'error');
            break;
    }
}

function setupPeerConnection() {
    pc = new RTCPeerConnection({
        iceServers: [
            { urls: 'stun:stun.l.google.com:19302' },
            { urls: 'stun:stun1.l.google.com:19302' }
        ]
    });
    
    // Add audio track
    stream.getTracks().forEach(track => pc.addTrack(track, stream));
    
    // Handle remote audio (assistant responses)
    pc.ontrack = (e) => {
        log('Received remote audio track', 'success');
        const audio = new Audio();
        audio.srcObject = e.streams[0];
        audio.play().catch(err => log('Audio autoplay blocked', 'warn'));
    };
    
    pc.onicecandidate = (e) => {
        if (e.candidate) {
            ws.send(JSON.stringify({ type: 'ice_candidate', candidate: e.candidate }));
        }
    };
    
    pc.onconnectionstatechange = () => {
        log('WebRTC state: ' + pc.connectionState);
        if (pc.connectionState === 'connected') {
            setStatus('Connected to Assistant', 'connected');
            disconnectBtn.disabled = false;
            log('WebRTC connected! Audio flowing to assistant-api', 'success');
        } else if (pc.connectionState === 'failed' || pc.connectionState === 'disconnected') {
            setStatus('Disconnected', 'disconnected');
        }
    };
    
    // Create and send offer
    const assistantId = parseInt(document.getElementById('assistantId').value) || 1;
    
    pc.createOffer().then(offer => {
        pc.setLocalDescription(offer);
        ws.send(JSON.stringify({ 
            type: 'offer', 
            sdp: offer.sdp,
            assistantId: assistantId
        }));
        log('Sent offer with assistant ID: ' + assistantId);
    });
}

function startMeter() {
    ctx = new AudioContext();
    an = ctx.createAnalyser();
    ctx.createMediaStreamSource(stream).connect(an);
    an.fftSize = 256;
    const data = new Uint8Array(an.frequencyBinCount);
    
    function update() {
        an.getByteFrequencyData(data);
        const level = data.reduce((a, b) => a + b) / data.length / 1.28;
        document.getElementById('bar').style.width = level + '%';
        requestAnimationFrame(update);
    }
    update();
}

function disconnect() {
    if (pc) pc.close();
    if (ws) ws.close();
    if (stream) stream.getTracks().forEach(t => t.stop());
    if (ctx) ctx.close();
    
    setStatus('Disconnected', 'disconnected');
    connectBtn.disabled = false;
    disconnectBtn.disabled = true;
    log('Disconnected');
}

log('Ready. Enter Assistant ID and click Connect.');
</script>
</body>
</html>`
