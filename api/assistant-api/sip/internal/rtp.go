// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package internal_sip

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rapidaai/pkg/commons"
)

// RTP constants
const (
	rtpVersion         = 2
	rtpHeaderSize      = 12
	rtpReadBufferSize  = 65536
	rtpWriteBufferSize = 65536
	rtpPacketMaxSize   = 1500
	rtpReadTimeout     = 100 * time.Millisecond
	rtpPacketInterval  = 20 * time.Millisecond

	// Audio channel buffer sizes
	rtpAudioInBufferSize  = 100
	rtpAudioOutBufferSize = 100
)

// RTPPacket represents an RTP packet
type RTPPacket struct {
	Version        uint8
	Padding        bool
	Extension      bool
	CSRCCount      uint8
	Marker         bool
	PayloadType    uint8
	SequenceNumber uint16
	Timestamp      uint32
	SSRC           uint32
	CSRC           []uint32
	Payload        []byte
}

// RTPHandler manages RTP streams for SIP calls
// No WebSocket needed - audio goes directly over RTP/UDP
type RTPHandler struct {
	mu      sync.RWMutex
	logger  commons.Logger
	running atomic.Bool

	conn      *net.UDPConn
	localIP   string
	localPort int

	remoteAddr *net.UDPAddr

	// RTP state
	ssrc           uint32
	sequenceNumber uint16
	timestamp      uint32
	codec          *Codec

	// Audio channels
	audioInChan  chan []byte
	audioOutChan chan []byte

	ctx    context.Context
	cancel context.CancelFunc

	// Statistics
	packetsSent     atomic.Uint64
	packetsReceived atomic.Uint64
	bytesReceived   atomic.Uint64
	bytesSent       atomic.Uint64
}

// RTPConfig holds configuration for RTP handler
type RTPConfig struct {
	LocalIP     string
	LocalPort   int
	PayloadType uint8  // 0 = PCMU, 8 = PCMA
	ClockRate   uint32 // 8000 for G.711
	Logger      commons.Logger
}

// Validate validates the RTP configuration
func (c *RTPConfig) Validate() error {
	if c.LocalIP == "" {
		return fmt.Errorf("local_ip is required")
	}
	if c.LocalPort < 0 || c.LocalPort > 65535 {
		return fmt.Errorf("invalid local_port: %d", c.LocalPort)
	}
	if c.ClockRate == 0 {
		c.ClockRate = 8000 // Default to 8kHz
	}
	return nil
}

// NewRTPHandler creates a new RTP handler for direct audio transport
func NewRTPHandler(ctx context.Context, config *RTPConfig) (*RTPHandler, error) {
	if err := config.Validate(); err != nil {
		return nil, NewSIPError("NewRTPHandler", "", "invalid configuration", err)
	}

	handlerCtx, cancel := context.WithCancel(ctx)

	addr := &net.UDPAddr{
		IP:   net.ParseIP(config.LocalIP),
		Port: config.LocalPort,
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		cancel()
		return nil, NewSIPError("NewRTPHandler", "", "failed to create RTP socket", err)
	}

	// Set buffer sizes
	if err := conn.SetReadBuffer(rtpReadBufferSize); err != nil && config.Logger != nil {
		config.Logger.Warn("Failed to set RTP read buffer size", "error", err)
	}
	if err := conn.SetWriteBuffer(rtpWriteBufferSize); err != nil && config.Logger != nil {
		config.Logger.Warn("Failed to set RTP write buffer size", "error", err)
	}

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	// Get codec from payload type or use default
	codec := GetCodecByPayloadType(config.PayloadType)
	if codec == nil {
		codec = &CodecPCMU
	}

	handler := &RTPHandler{
		logger:       config.Logger,
		conn:         conn,
		localIP:      localAddr.IP.String(),
		localPort:    localAddr.Port,
		ssrc:         rand.Uint32(),
		codec:        codec,
		audioInChan:  make(chan []byte, rtpAudioInBufferSize),
		audioOutChan: make(chan []byte, rtpAudioOutBufferSize),
		ctx:          handlerCtx,
		cancel:       cancel,
	}

	return handler, nil
}

// Start begins RTP processing
func (h *RTPHandler) Start() {
	if !h.running.CompareAndSwap(false, true) {
		return // Already running
	}

	go h.receiveLoop()
	go h.sendLoop()

	if h.logger != nil {
		h.logger.Debug("RTP handler started",
			"local_addr", fmt.Sprintf("%s:%d", h.localIP, h.localPort),
			"codec", h.codec.Name)
	}
}

// Stop stops RTP processing gracefully
func (h *RTPHandler) Stop() error {
	if !h.running.CompareAndSwap(true, false) {
		return nil // Already stopped
	}

	h.cancel()

	// Close channels safely
	h.closeChannels()

	var err error
	if h.conn != nil {
		err = h.conn.Close()
	}

	if h.logger != nil {
		sent, received := h.GetStats()
		h.logger.Debug("RTP handler stopped",
			"packets_sent", sent,
			"packets_received", received)
	}

	return err
}

// closeChannels safely closes audio channels
func (h *RTPHandler) closeChannels() {
	defer func() {
		recover() // Recover if channels are already closed
	}()
	close(h.audioInChan)
	close(h.audioOutChan)
}

// IsRunning returns whether the RTP handler is running
func (h *RTPHandler) IsRunning() bool {
	return h.running.Load()
}

// SetRemoteAddr sets the remote RTP endpoint
func (h *RTPHandler) SetRemoteAddr(ip string, port int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.remoteAddr = &net.UDPAddr{
		IP:   net.ParseIP(ip),
		Port: port,
	}

	if h.logger != nil {
		h.logger.Debug("RTP remote address set", "ip", ip, "port", port)
	}
}

// GetRemoteAddr returns the remote RTP address
func (h *RTPHandler) GetRemoteAddr() *net.UDPAddr {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.remoteAddr
}

// LocalAddr returns the local RTP address
func (h *RTPHandler) LocalAddr() (string, int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.localIP, h.localPort
}

// AudioIn returns the channel for received audio
func (h *RTPHandler) AudioIn() <-chan []byte {
	return h.audioInChan
}

// AudioOut returns the channel for sending audio
func (h *RTPHandler) AudioOut() chan<- []byte {
	return h.audioOutChan
}

// GetCodec returns the codec used by this handler
func (h *RTPHandler) GetCodec() *Codec {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.codec
}

func (h *RTPHandler) receiveLoop() {
	buf := make([]byte, rtpPacketMaxSize)
	logCounter := 0

	for {
		select {
		case <-h.ctx.Done():
			return
		default:
		}

		if err := h.conn.SetReadDeadline(time.Now().Add(rtpReadTimeout)); err != nil {
			continue
		}

		n, remoteAddr, err := h.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			// Connection closed or other error
			if h.running.Load() && h.logger != nil {
				h.logger.Warn("RTP receive error", "error", err)
			}
			continue
		}

		if n < rtpHeaderSize {
			if h.logger != nil {
				h.logger.Warn("RTP packet too small", "size", n)
			}
			continue
		}

		packet, err := h.parseRTPPacket(buf[:n])
		if err != nil {
			if h.logger != nil {
				h.logger.Warn("Failed to parse RTP packet", "error", err)
			}
			continue
		}

		// Auto-detect remote address if not set
		h.mu.Lock()
		if h.remoteAddr == nil {
			h.remoteAddr = remoteAddr
			if h.logger != nil {
				h.logger.Info("RTP: Auto-detected remote address", "addr", remoteAddr.String())
			}
		}
		h.mu.Unlock()

		// Update statistics
		h.packetsReceived.Add(1)
		h.bytesReceived.Add(uint64(len(packet.Payload)))
		count := h.packetsReceived.Load()

		// Log periodically
		logCounter++
		if h.logger != nil && logCounter%rtpLogInterval == 1 {
			h.logger.Debug("RTP: Received audio packet",
				"seq", packet.SequenceNumber,
				"payload_size", len(packet.Payload),
				"total_received", count)
		}

		// Send to channel (non-blocking)
		select {
		case h.audioInChan <- packet.Payload:
		default:
			if h.logger != nil && logCounter%rtpLogInterval == 1 {
				h.logger.Warn("RTP: Audio input channel full, dropping packet")
			}
		}
	}
}

func (h *RTPHandler) sendLoop() {
	// Calculate samples per packet based on codec (20ms packets)
	samplesPerPacket := int(h.codec.ClockRate * 20 / 1000) // e.g., 160 bytes for PCMU at 8kHz

	// Pre-create silence chunk (μ-law silence is 0xFF, PCMA silence is 0xD5)
	silenceChunk := h.createSilenceChunk(samplesPerPacket)

	var pendingAudio []byte
	logCounter := 0
	nextSendTime := time.Now().Add(rtpPacketInterval)

	for {
		// Check for context cancellation
		select {
		case <-h.ctx.Done():
			return
		default:
		}

		// Collect pending audio (non-blocking)
		select {
		case audio, ok := <-h.audioOutChan:
			if ok {
				pendingAudio = append(pendingAudio, audio...)
			}
		default:
		}

		// Wait until next send time with precision
		now := time.Now()
		if sleepDuration := nextSendTime.Sub(now); sleepDuration > 0 {
			time.Sleep(sleepDuration)
		}

		// Schedule next send immediately to minimize drift
		nextSendTime = nextSendTime.Add(rtpPacketInterval)

		// If we've fallen behind, reset timing (don't try to catch up)
		if time.Now().After(nextSendTime) {
			nextSendTime = time.Now().Add(rtpPacketInterval)
		}

		h.mu.RLock()
		remoteAddr := h.remoteAddr
		h.mu.RUnlock()

		if remoteAddr == nil {
			continue
		}

		// Get exactly ONE chunk: audio if available, otherwise silence
		chunk := h.getAudioChunk(&pendingAudio, samplesPerPacket, silenceChunk)

		packet := h.createRTPPacket(chunk)
		data := h.serializeRTPPacket(packet)

		if _, err := h.conn.WriteToUDP(data, remoteAddr); err != nil {
			if h.running.Load() && h.logger != nil {
				h.logger.Error("RTP: Failed to send packet", "error", err, "remote", remoteAddr.String())
			}
			continue
		}

		// Update statistics
		h.packetsSent.Add(1)
		h.bytesSent.Add(uint64(len(chunk)))
		count := h.packetsSent.Load()

		// Log periodically
		logCounter++
		if h.logger != nil && logCounter%rtpLogInterval == 1 {
			isSilence := len(pendingAudio) == 0
			h.logger.Debug("RTP: Sent packet",
				"seq", packet.SequenceNumber,
				"total_sent", count,
				"pending", len(pendingAudio),
				"silence", isSilence)
		}
	}
}

// createSilenceChunk creates a silence chunk for the codec
func (h *RTPHandler) createSilenceChunk(size int) []byte {
	chunk := make([]byte, size)
	silenceValue := byte(0xFF) // μ-law silence
	if h.codec.Name == "PCMA" {
		silenceValue = 0xD5 // A-law silence
	}
	for i := range chunk {
		chunk[i] = silenceValue
	}
	return chunk
}

// getAudioChunk extracts or creates an audio chunk for sending
func (h *RTPHandler) getAudioChunk(pendingAudio *[]byte, size int, silenceChunk []byte) []byte {
	if len(*pendingAudio) >= size {
		chunk := (*pendingAudio)[:size]
		*pendingAudio = (*pendingAudio)[size:]
		return chunk
	}

	if len(*pendingAudio) > 0 {
		// Partial audio - pad with silence
		silenceValue := silenceChunk[0]
		chunk := make([]byte, size)
		copy(chunk, *pendingAudio)
		for i := len(*pendingAudio); i < size; i++ {
			chunk[i] = silenceValue
		}
		*pendingAudio = nil
		return chunk
	}

	// No audio - return silence
	return silenceChunk
}

func (h *RTPHandler) parseRTPPacket(data []byte) (*RTPPacket, error) {
	if len(data) < 12 {
		return nil, fmt.Errorf("packet too small")
	}

	packet := &RTPPacket{
		Version:        (data[0] >> 6) & 0x03,
		Padding:        (data[0] & 0x20) != 0,
		Extension:      (data[0] & 0x10) != 0,
		CSRCCount:      data[0] & 0x0F,
		Marker:         (data[1] & 0x80) != 0,
		PayloadType:    data[1] & 0x7F,
		SequenceNumber: binary.BigEndian.Uint16(data[2:4]),
		Timestamp:      binary.BigEndian.Uint32(data[4:8]),
		SSRC:           binary.BigEndian.Uint32(data[8:12]),
	}

	if packet.Version != 2 {
		return nil, fmt.Errorf("unsupported RTP version: %d", packet.Version)
	}

	headerLen := 12 + int(packet.CSRCCount)*4

	if packet.Extension && len(data) >= headerLen+4 {
		extLen := binary.BigEndian.Uint16(data[headerLen+2 : headerLen+4])
		headerLen += 4 + int(extLen)*4
	}

	payloadLen := len(data) - headerLen
	if packet.Padding && payloadLen > 0 {
		paddingLen := int(data[len(data)-1])
		payloadLen -= paddingLen
	}

	if payloadLen < 0 || headerLen+payloadLen > len(data) {
		return nil, fmt.Errorf("invalid packet length")
	}

	packet.Payload = make([]byte, payloadLen)
	copy(packet.Payload, data[headerLen:headerLen+payloadLen])

	return packet, nil
}

func (h *RTPHandler) createRTPPacket(payload []byte) *RTPPacket {
	h.mu.Lock()
	defer h.mu.Unlock()

	packet := &RTPPacket{
		Version:        rtpVersion,
		PayloadType:    h.codec.PayloadType,
		SequenceNumber: h.sequenceNumber,
		Timestamp:      h.timestamp,
		SSRC:           h.ssrc,
		Payload:        payload,
	}

	h.sequenceNumber++
	h.timestamp += uint32(len(payload))

	return packet
}

func (h *RTPHandler) serializeRTPPacket(packet *RTPPacket) []byte {
	headerLen := 12 + len(packet.CSRC)*4
	data := make([]byte, headerLen+len(packet.Payload))

	data[0] = (packet.Version << 6)
	if packet.Padding {
		data[0] |= 0x20
	}
	if packet.Extension {
		data[0] |= 0x10
	}
	data[0] |= packet.CSRCCount & 0x0F

	data[1] = packet.PayloadType & 0x7F
	if packet.Marker {
		data[1] |= 0x80
	}

	binary.BigEndian.PutUint16(data[2:4], packet.SequenceNumber)
	binary.BigEndian.PutUint32(data[4:8], packet.Timestamp)
	binary.BigEndian.PutUint32(data[8:12], packet.SSRC)

	for i, csrc := range packet.CSRC {
		binary.BigEndian.PutUint32(data[12+i*4:16+i*4], csrc)
	}

	copy(data[headerLen:], packet.Payload)

	return data
}

// GetStats returns RTP statistics
func (h *RTPHandler) GetStats() (sent, received uint64) {
	return h.packetsSent.Load(), h.packetsReceived.Load()
}

// GetDetailedStats returns detailed RTP statistics
func (h *RTPHandler) GetDetailedStats() RTPStats {
	return RTPStats{
		PacketsSent:     h.packetsSent.Load(),
		PacketsReceived: h.packetsReceived.Load(),
		BytesSent:       h.bytesSent.Load(),
		BytesReceived:   h.bytesReceived.Load(),
	}
}
