package sip

// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

import (
	"fmt"
	"time"
)

// Transport represents the transport protocol for SIP
type Transport string

const (
	TransportUDP Transport = "udp"
	TransportTCP Transport = "tcp"
	TransportTLS Transport = "tls"
)

// Config holds per-tenant SIP configuration from vault credentials
type Config struct {
	Server            string    `json:"sip_server"`
	Port              int       `json:"sip_port"`
	Transport         Transport `json:"sip_transport"`
	Username          string    `json:"sip_username"`
	Password          string    `json:"sip_password"`
	Realm             string    `json:"sip_realm"`
	RTPPortRangeStart int       `json:"rtp_port_range_start"`
	RTPPortRangeEnd   int       `json:"rtp_port_range_end"`
	SRTPEnabled       bool      `json:"srtp_enabled"`
	Domain            string    `json:"sip_domain,omitempty"`
}

// Validate validates the SIP configuration
func (c *Config) Validate() error {
	if c.Server == "" {
		return fmt.Errorf("sip_server is required")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("sip_port must be between 1 and 65535")
	}
	if c.Username == "" {
		return fmt.Errorf("sip_username is required")
	}
	if c.Password == "" {
		return fmt.Errorf("sip_password is required")
	}
	if c.RTPPortRangeStart <= 0 || c.RTPPortRangeEnd <= 0 {
		return fmt.Errorf("rtp_port_range must be specified")
	}
	if c.RTPPortRangeStart >= c.RTPPortRangeEnd {
		return fmt.Errorf("rtp_port_range_start must be less than rtp_port_range_end")
	}
	return nil
}

// GetSIPURI returns the full SIP URI for the server
func (c *Config) GetSIPURI() string {
	domain := c.Domain
	if domain == "" {
		domain = c.Server
	}
	return fmt.Sprintf("sip:%s@%s:%d", c.Username, domain, c.Port)
}

// CallState represents the state of a SIP call
type CallState string

const (
	CallStateInitializing CallState = "initializing"
	CallStateRinging      CallState = "ringing"
	CallStateConnected    CallState = "connected"
	CallStateOnHold       CallState = "on_hold"
	CallStateEnding       CallState = "ending"
	CallStateEnded        CallState = "ended"
	CallStateFailed       CallState = "failed"
)

// SessionInfo contains information about an active SIP session
type SessionInfo struct {
	CallID           string     `json:"call_id"`
	LocalTag         string     `json:"local_tag"`
	RemoteTag        string     `json:"remote_tag"`
	LocalURI         string     `json:"local_uri"`
	RemoteURI        string     `json:"remote_uri"`
	State            CallState  `json:"state"`
	Direction        string     `json:"direction"` // "inbound" or "outbound"
	StartTime        time.Time  `json:"start_time"`
	ConnectedTime    *time.Time `json:"connected_time,omitempty"`
	EndTime          *time.Time `json:"end_time,omitempty"`
	LocalRTPAddress  string     `json:"local_rtp_address"`
	RemoteRTPAddress string     `json:"remote_rtp_address"`
	Codec            string     `json:"codec"`
}

// Event represents events from SIP stack
type Event struct {
	Type      string                 `json:"type"`
	CallID    string                 `json:"call_id"`
	Timestamp time.Time              `json:"timestamp"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// DTMFEvent represents DTMF input
type DTMFEvent struct {
	Digit    string `json:"digit"`
	Duration int    `json:"duration"`
}
