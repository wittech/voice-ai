// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package sip_infra

import (
	"fmt"
	"strconv"
	"strings"
)

// Codec represents an audio codec with its RTP configuration
type Codec struct {
	Name        string
	PayloadType uint8
	ClockRate   uint32
	Channels    int
}

// Common codecs used in telephony
var (
	CodecPCMU = Codec{Name: "PCMU", PayloadType: 0, ClockRate: 8000, Channels: 1}
	CodecPCMA = Codec{Name: "PCMA", PayloadType: 8, ClockRate: 8000, Channels: 1}
	CodecG722 = Codec{Name: "G722", PayloadType: 9, ClockRate: 8000, Channels: 1}
)

// SupportedCodecs lists codecs in order of preference
var SupportedCodecs = []Codec{CodecPCMU, CodecPCMA}

// SDPMediaInfo contains parsed media information from SDP
type SDPMediaInfo struct {
	ConnectionIP   string
	AudioPort      int
	PayloadTypes   []uint8
	PreferredCodec *Codec
}

// SDPConfig holds configuration for SDP generation
type SDPConfig struct {
	SessionID   string
	SessionName string
	LocalIP     string
	RTPPort     int
	Codecs      []Codec
	PTime       int // Packetization time in milliseconds
}

// DefaultSDPConfig returns a default SDP configuration
func DefaultSDPConfig(localIP string, rtpPort int) *SDPConfig {
	return &SDPConfig{
		SessionID:   "0",
		SessionName: "Rapida Voice AI",
		LocalIP:     localIP,
		RTPPort:     rtpPort,
		Codecs:      SupportedCodecs,
		PTime:       20,
	}
}

// GenerateSDP creates an SDP body for SIP responses
func GenerateSDP(cfg *SDPConfig) string {
	var sb strings.Builder

	// Version
	sb.WriteString("v=0\r\n")

	// Origin: o=<username> <sess-id> <sess-version> <nettype> <addrtype> <unicast-address>
	sb.WriteString(fmt.Sprintf("o=rapida %s 0 IN IP4 %s\r\n", cfg.SessionID, cfg.LocalIP))

	// Session Name
	sb.WriteString(fmt.Sprintf("s=%s\r\n", cfg.SessionName))

	// Connection Information
	sb.WriteString(fmt.Sprintf("c=IN IP4 %s\r\n", cfg.LocalIP))

	// Time (0 0 = session is permanent)
	sb.WriteString("t=0 0\r\n")

	// Media Description
	payloadTypes := make([]string, len(cfg.Codecs))
	for i, codec := range cfg.Codecs {
		payloadTypes[i] = strconv.Itoa(int(codec.PayloadType))
	}
	sb.WriteString(fmt.Sprintf("m=audio %d RTP/AVP %s\r\n", cfg.RTPPort, strings.Join(payloadTypes, " ")))

	// Codec attributes
	for _, codec := range cfg.Codecs {
		sb.WriteString(fmt.Sprintf("a=rtpmap:%d %s/%d\r\n", codec.PayloadType, codec.Name, codec.ClockRate))
	}

	// Packetization time
	sb.WriteString(fmt.Sprintf("a=ptime:%d\r\n", cfg.PTime))

	// Direction (send and receive)
	sb.WriteString("a=sendrecv\r\n")

	return sb.String()
}

// ParseSDP extracts media information from an SDP body
func ParseSDP(sdpBody []byte) (*SDPMediaInfo, error) {
	if len(sdpBody) == 0 {
		return nil, fmt.Errorf("empty SDP body")
	}

	info := &SDPMediaInfo{
		PayloadTypes: make([]uint8, 0),
	}

	sdpStr := string(sdpBody)
	lines := strings.Split(sdpStr, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimSuffix(line, "\r")

		switch {
		case strings.HasPrefix(line, "c=IN IP4 "):
			// Connection line: c=IN IP4 192.168.1.5
			info.ConnectionIP = strings.TrimSpace(strings.TrimPrefix(line, "c=IN IP4 "))

		case strings.HasPrefix(line, "m=audio "):
			// Media line: m=audio 10000 RTP/AVP 0 8 101
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				port, err := strconv.Atoi(parts[1])
				if err == nil {
					info.AudioPort = port
				}
				// Parse payload types
				for i := 3; i < len(parts); i++ {
					pt, err := strconv.Atoi(parts[i])
					if err == nil && pt >= 0 && pt <= 127 {
						info.PayloadTypes = append(info.PayloadTypes, uint8(pt))
					}
				}
			}

		case strings.HasPrefix(line, "a=rtpmap:"):
			// RTP map: a=rtpmap:0 PCMU/8000
			// We use this to confirm codec selection
		}
	}

	// Determine preferred codec based on first matching payload type
	for _, pt := range info.PayloadTypes {
		for _, codec := range SupportedCodecs {
			if codec.PayloadType == pt {
				info.PreferredCodec = &codec
				break
			}
		}
		if info.PreferredCodec != nil {
			break
		}
	}

	// Default to PCMU if no match found
	if info.PreferredCodec == nil && len(info.PayloadTypes) > 0 {
		info.PreferredCodec = &CodecPCMU
	}

	return info, nil
}

// NegotiateCodec selects the best codec based on remote SDP
func NegotiateCodec(remotePayloadTypes []uint8) *Codec {
	// Find first matching codec in our supported list
	for _, supported := range SupportedCodecs {
		for _, remotePT := range remotePayloadTypes {
			if supported.PayloadType == remotePT {
				return &supported
			}
		}
	}
	// Default to PCMU
	return &CodecPCMU
}

// GetCodecByPayloadType returns a codec by its payload type
func GetCodecByPayloadType(pt uint8) *Codec {
	for _, codec := range SupportedCodecs {
		if codec.PayloadType == pt {
			return &codec
		}
	}
	return nil
}

// GetCodecByName returns a codec by its name
func GetCodecByName(name string) *Codec {
	name = strings.ToUpper(name)
	for _, codec := range SupportedCodecs {
		if codec.Name == name {
			return &codec
		}
	}
	return nil
}
