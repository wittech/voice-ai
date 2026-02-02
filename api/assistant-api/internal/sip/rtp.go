// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package sip

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/rapidaai/pkg/commons"
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
	mu     sync.RWMutex
	logger commons.Logger

	conn      *net.UDPConn
	localIP   string
	localPort int

	remoteAddr *net.UDPAddr

	ssrc           uint32
	sequenceNumber uint16
	timestamp      uint32
	payloadType    uint8
	clockRate      uint32

	audioInChan  chan []byte
	audioOutChan chan []byte

	ctx    context.Context
	cancel context.CancelFunc

	packetsSent     uint64
	packetsReceived uint64
}

// RTPConfig holds configuration for RTP handler
type RTPConfig struct {
	LocalIP     string
	LocalPort   int
	PayloadType uint8  // 0 = PCMU, 8 = PCMA
	ClockRate   uint32 // 8000 for G.711
	Logger      commons.Logger
}

// NewRTPHandler creates a new RTP handler for direct audio transport
func NewRTPHandler(ctx context.Context, config *RTPConfig) (*RTPHandler, error) {
	handlerCtx, cancel := context.WithCancel(ctx)

	addr := &net.UDPAddr{
		IP:   net.ParseIP(config.LocalIP),
		Port: config.LocalPort,
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create RTP socket: %w", err)
	}

	conn.SetReadBuffer(65536)
	conn.SetWriteBuffer(65536)

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	handler := &RTPHandler{
		logger:       config.Logger,
		conn:         conn,
		localIP:      localAddr.IP.String(),
		localPort:    localAddr.Port,
		ssrc:         rand.Uint32(),
		payloadType:  config.PayloadType,
		clockRate:    config.ClockRate,
		audioInChan:  make(chan []byte, 100),
		audioOutChan: make(chan []byte, 100),
		ctx:          handlerCtx,
		cancel:       cancel,
	}

	return handler, nil
}

// Start begins RTP processing
func (h *RTPHandler) Start() {
	go h.receiveLoop()
	go h.sendLoop()
}

// Stop stops RTP processing
func (h *RTPHandler) Stop() error {
	h.cancel()
	close(h.audioInChan)
	close(h.audioOutChan)
	if h.conn != nil {
		return h.conn.Close()
	}
	return nil
}

// SetRemoteAddr sets the remote RTP endpoint
func (h *RTPHandler) SetRemoteAddr(ip string, port int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.remoteAddr = &net.UDPAddr{
		IP:   net.ParseIP(ip),
		Port: port,
	}
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

func (h *RTPHandler) receiveLoop() {
	buf := make([]byte, 1500)

	for {
		select {
		case <-h.ctx.Done():
			return
		default:
		}

		h.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

		n, remoteAddr, err := h.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			continue
		}

		if n < 12 {
			continue
		}

		packet, err := h.parseRTPPacket(buf[:n])
		if err != nil {
			continue
		}

		h.mu.Lock()
		if h.remoteAddr == nil {
			h.remoteAddr = remoteAddr
		}
		h.packetsReceived++
		h.mu.Unlock()

		select {
		case h.audioInChan <- packet.Payload:
		default:
		}
	}
}

func (h *RTPHandler) sendLoop() {
	packetDuration := 20 * time.Millisecond
	ticker := time.NewTicker(packetDuration)
	defer ticker.Stop()

	var pendingAudio []byte

	for {
		select {
		case <-h.ctx.Done():
			return
		case audio := <-h.audioOutChan:
			pendingAudio = append(pendingAudio, audio...)
		case <-ticker.C:
			h.mu.RLock()
			remoteAddr := h.remoteAddr
			h.mu.RUnlock()

			if remoteAddr == nil || len(pendingAudio) == 0 {
				continue
			}

			samplesPerPacket := int(h.clockRate * 20 / 1000)

			for len(pendingAudio) >= samplesPerPacket {
				chunk := pendingAudio[:samplesPerPacket]
				pendingAudio = pendingAudio[samplesPerPacket:]

				packet := h.createRTPPacket(chunk)
				data := h.serializeRTPPacket(packet)

				if _, err := h.conn.WriteToUDP(data, remoteAddr); err != nil {
					continue
				}

				h.mu.Lock()
				h.packetsSent++
				h.mu.Unlock()
			}
		}
	}
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
		Version:        2,
		PayloadType:    h.payloadType,
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
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.packetsSent, h.packetsReceived
}
