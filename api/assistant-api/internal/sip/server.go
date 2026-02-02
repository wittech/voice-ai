// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package sip

import (
	"context"
	"fmt"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/rapidaai/pkg/commons"
)

// Server wraps sipgo for handling SIP signaling per-tenant
// Uses native SIP signaling (UDP/TCP/TLS) - no WebSocket needed
type Server struct {
	mu     sync.RWMutex
	logger commons.Logger

	ua       *sipgo.UserAgent
	server   *sipgo.Server
	client   *sipgo.Client
	config   *Config
	tenantID string

	sessions map[string]*Session

	onInvite func(session *Session, fromURI, toURI string) error
	onBye    func(session *Session) error
	onCancel func(session *Session) error

	ctx    context.Context
	cancel context.CancelFunc
}

// ServerConfig holds configuration for creating a SIP server
type ServerConfig struct {
	TenantID string
	Config   *Config
	Logger   commons.Logger
}

// NewServer creates a new SIP server instance for a specific tenant
func NewServer(ctx context.Context, cfg *ServerConfig) (*Server, error) {
	if err := cfg.Config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid SIP config: %w", err)
	}

	serverCtx, cancel := context.WithCancel(ctx)

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(fmt.Sprintf("RapidaVoiceAI/%s", cfg.TenantID)),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create SIP UA: %w", err)
	}

	server, err := sipgo.NewServer(ua)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create SIP server: %w", err)
	}

	clientOpts := []sipgo.ClientOption{
		sipgo.WithClientHostname(cfg.Config.Server),
	}
	if cfg.Config.Port > 0 {
		clientOpts = append(clientOpts, sipgo.WithClientPort(cfg.Config.Port))
	}

	client, err := sipgo.NewClient(ua, clientOpts...)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create SIP client: %w", err)
	}

	s := &Server{
		logger:   cfg.Logger,
		ua:       ua,
		server:   server,
		client:   client,
		config:   cfg.Config,
		tenantID: cfg.TenantID,
		sessions: make(map[string]*Session),
		ctx:      serverCtx,
		cancel:   cancel,
	}

	s.registerHandlers()

	return s, nil
}

func (s *Server) registerHandlers() {
	s.server.OnInvite(s.handleInvite)
	s.server.OnAck(s.handleAck)
	s.server.OnBye(s.handleBye)
	s.server.OnCancel(s.handleCancel)
	s.server.OnRegister(s.handleRegister)
	s.server.OnOptions(s.handleOptions)
}

// Start begins listening for SIP traffic
func (s *Server) Start() error {
	listenAddr := fmt.Sprintf("%s:%d", s.config.Server, s.config.Port)

	go func() {
		var err error
		switch s.config.Transport {
		case TransportUDP:
			err = s.server.ListenAndServe(s.ctx, "udp", listenAddr)
		case TransportTCP:
			err = s.server.ListenAndServe(s.ctx, "tcp", listenAddr)
		case TransportTLS:
			err = s.server.ListenAndServe(s.ctx, "tls", listenAddr)
		default:
			err = s.server.ListenAndServe(s.ctx, "udp", listenAddr)
		}
		if err != nil {
			s.logger.Error("SIP server stopped", "error", err, "tenant", s.tenantID)
		}
	}()

	s.logger.Info("SIP server started",
		"tenant", s.tenantID,
		"address", listenAddr,
		"transport", s.config.Transport)

	return nil
}

// Stop stops the SIP server
func (s *Server) Stop() {
	s.cancel()
	s.mu.Lock()
	for _, session := range s.sessions {
		session.End()
	}
	s.sessions = make(map[string]*Session)
	s.mu.Unlock()
}

// SetOnInvite sets the callback for incoming INVITE requests
func (s *Server) SetOnInvite(fn func(session *Session, fromURI, toURI string) error) {
	s.onInvite = fn
}

// SetOnBye sets the callback for BYE requests
func (s *Server) SetOnBye(fn func(session *Session) error) {
	s.onBye = fn
}

// SetOnCancel sets the callback for CANCEL requests
func (s *Server) SetOnCancel(fn func(session *Session) error) {
	s.onCancel = fn
}

func (s *Server) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()

	session, err := NewSession(s.ctx, s.config, "inbound")
	if err != nil {
		s.logger.Error("Failed to create session", "error", err)
		s.sendResponse(tx, req, 500) // Internal Server Error
		return
	}

	s.mu.Lock()
	s.sessions[callID] = session
	s.mu.Unlock()

	// Send 100 Trying
	s.sendResponse(tx, req, 100)

	// Send 180 Ringing
	s.sendResponse(tx, req, 180)
	session.SetState(CallStateRinging)

	fromURI := req.From().Address.String()
	toURI := req.To().Address.String()

	if s.onInvite != nil {
		if err := s.onInvite(session, fromURI, toURI); err != nil {
			s.logger.Error("INVITE handler failed", "error", err)
			s.sendResponse(tx, req, 503) // Service Unavailable
			return
		}
	}

	// Send 200 OK with SDP
	s.sendResponse(tx, req, 200)
	session.SetState(CallStateConnected)
}

func (s *Server) handleAck(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()

	s.mu.RLock()
	session, exists := s.sessions[callID]
	s.mu.RUnlock()

	if !exists {
		return
	}

	session.SetState(CallStateConnected)
	s.logger.Info("SIP call established", "call_id", callID)
}

func (s *Server) handleBye(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()

	s.mu.Lock()
	session, exists := s.sessions[callID]
	if exists {
		delete(s.sessions, callID)
	}
	s.mu.Unlock()

	if !exists {
		s.sendResponse(tx, req, 481) // Call/Transaction Does Not Exist
		return
	}

	if s.onBye != nil {
		s.onBye(session)
	}

	session.End()
	s.sendResponse(tx, req, 200) // OK
	s.logger.Info("SIP call ended", "call_id", callID)
}

func (s *Server) handleCancel(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()

	s.mu.Lock()
	session, exists := s.sessions[callID]
	if exists {
		delete(s.sessions, callID)
	}
	s.mu.Unlock()

	if !exists {
		s.sendResponse(tx, req, 481) // Call/Transaction Does Not Exist
		return
	}

	if s.onCancel != nil {
		s.onCancel(session)
	}

	session.End()
	s.sendResponse(tx, req, 200) // OK
}

func (s *Server) handleRegister(req *sip.Request, tx sip.ServerTransaction) {
	s.sendResponse(tx, req, 200) // OK
}

func (s *Server) handleOptions(req *sip.Request, tx sip.ServerTransaction) {
	s.sendResponse(tx, req, 200) // OK
}

func (s *Server) sendResponse(tx sip.ServerTransaction, req *sip.Request, statusCode int) {
	resp := sip.NewResponseFromRequest(req, statusCode, "", nil)
	if err := tx.Respond(resp); err != nil {
		s.logger.Error("Failed to send SIP response", "error", err, "status", statusCode)
	}
}

// GetSession returns a session by call ID
func (s *Server) GetSession(callID string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, exists := s.sessions[callID]
	return session, exists
}

// EndCall ends a specific call
func (s *Server) EndCall(session *Session) error {
	if session == nil {
		return fmt.Errorf("session is nil")
	}
	session.End()
	return nil
}
