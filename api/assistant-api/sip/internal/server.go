// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package internal_sip

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/rapidaai/pkg/commons"
)

// ServerState represents the state of the SIP server
type ServerState int32

const (
	ServerStateCreated ServerState = iota
	ServerStateRunning
	ServerStateStopped
)

// InviteContext contains information about an incoming INVITE for config resolution
// Supports URI format: sip:{assistantID}.rapid-sip@in.rapida.ai
type InviteContext struct {
	CallID  string
	FromURI string
	ToURI   string
	SDPInfo *SDPMediaInfo

	// Authentication fields extracted from SIP headers
	APIKey      string // X-API-Key header for authentication
	AssistantID string // Parsed from To URI (e.g., "123456" from "123456.rapid-sip@in.rapida.ai")

	// Additional headers for context
	Headers map[string]string
}

// InviteResult contains the resolved configuration for handling the call
type InviteResult struct {
	Config      *Config // Tenant-specific config (RTP ports, credentials, etc.)
	ShouldAllow bool    // Whether to accept the call
	RejectCode  int     // SIP response code if rejecting (e.g., 403, 404)
	RejectMsg   string  // Optional message for rejection
}

// ConfigResolver resolves tenant-specific config from SIP INVITE context
type ConfigResolver func(ctx *InviteContext) (*InviteResult, error)

// Server wraps sipgo for handling SIP signaling
// Uses native SIP signaling (UDP/TCP/TLS) - no WebSocket needed
// Multi-tenant: Config is resolved per-call via ConfigResolver callback
type Server struct {
	mu     sync.RWMutex
	logger commons.Logger
	state  atomic.Int32

	ua           *sipgo.UserAgent
	server       *sipgo.Server
	client       *sipgo.Client
	listenConfig *ListenConfig // Shared server listen config (address, port, transport)

	sessions     map[string]*Session
	sessionCount atomic.Int64

	// Multi-tenant config resolver - called for each incoming INVITE
	configResolver ConfigResolver

	// Event callbacks
	onInvite func(session *Session, fromURI, toURI string) error
	onBye    func(session *Session) error
	onCancel func(session *Session) error
	onError  func(session *Session, err error)

	ctx    context.Context
	cancel context.CancelFunc
}

// ListenConfig holds shared server configuration (not tenant-specific)
type ListenConfig struct {
	Address   string    `json:"address" mapstructure:"address"`
	Port      int       `json:"port" mapstructure:"port"`
	Transport Transport `json:"transport" mapstructure:"transport"`
}

// GetListenAddr returns the address to listen on
func (c *ListenConfig) GetListenAddr() string {
	return fmt.Sprintf("%s:%d", c.Address, c.Port)
}

// ServerConfig holds configuration for creating a SIP server
// Multi-tenant: Only holds shared listen config, tenant config resolved per-call
type ServerConfig struct {
	ListenConfig   *ListenConfig  // Shared server listen configuration
	ConfigResolver ConfigResolver // Resolves tenant-specific config per-call
	Logger         commons.Logger
}

// Validate validates the server configuration
func (c *ServerConfig) Validate() error {
	if c.ListenConfig == nil {
		return fmt.Errorf("listen config is required")
	}
	if c.ListenConfig.Address == "" {
		return fmt.Errorf("listen address is required")
	}
	if c.ListenConfig.Port <= 0 || c.ListenConfig.Port > 65535 {
		return fmt.Errorf("invalid listen port: %d", c.ListenConfig.Port)
	}
	if c.Logger == nil {
		return fmt.Errorf("logger is required")
	}
	return nil
}

// NewServer creates a new shared SIP server instance
// Multi-tenant: Server listens on shared address, config resolved per-call via ConfigResolver
func NewServer(ctx context.Context, cfg *ServerConfig) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, NewSIPError("NewServer", "", "configuration validation failed", err)
	}

	serverCtx, cancel := context.WithCancel(ctx)

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("RapidaVoiceAI"),
	)
	if err != nil {
		cancel()
		return nil, NewSIPError("NewServer", "", "failed to create SIP user agent", err)
	}

	server, err := sipgo.NewServer(ua)
	if err != nil {
		cancel()
		return nil, NewSIPError("NewServer", "", "failed to create SIP server", err)
	}

	clientOpts := []sipgo.ClientOption{
		sipgo.WithClientHostname(cfg.ListenConfig.Address),
	}
	if cfg.ListenConfig.Port > 0 {
		clientOpts = append(clientOpts, sipgo.WithClientPort(cfg.ListenConfig.Port))
	}

	client, err := sipgo.NewClient(ua, clientOpts...)
	if err != nil {
		cancel()
		return nil, NewSIPError("NewServer", "", "failed to create SIP client", err)
	}

	s := &Server{
		logger:         cfg.Logger,
		ua:             ua,
		server:         server,
		client:         client,
		listenConfig:   cfg.ListenConfig,
		configResolver: cfg.ConfigResolver,
		sessions:       make(map[string]*Session),
		ctx:            serverCtx,
		cancel:         cancel,
	}

	s.state.Store(int32(ServerStateCreated))
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
	if !s.state.CompareAndSwap(int32(ServerStateCreated), int32(ServerStateRunning)) {
		return fmt.Errorf("server is not in created state")
	}

	listenAddr := s.listenConfig.GetListenAddr()
	transport := s.listenConfig.Transport.String()
	if transport == "" {
		transport = "udp"
	}

	go func() {
		err := s.server.ListenAndServe(s.ctx, transport, listenAddr)
		if err != nil && s.state.Load() == int32(ServerStateRunning) {
			s.logger.Error("SIP server stopped unexpectedly",
				"error", err,
				"address", listenAddr)
			s.state.Store(int32(ServerStateStopped))
		}
	}()

	s.logger.Info("SIP server started (multi-tenant)",
		"address", listenAddr,
		"transport", transport)

	return nil
}

// Stop stops the SIP server gracefully
func (s *Server) Stop() {
	if !s.state.CompareAndSwap(int32(ServerStateRunning), int32(ServerStateStopped)) {
		return // Already stopped or not running
	}

	s.logger.Info("Stopping SIP server")

	// Cancel context first to stop accepting new calls
	s.cancel()

	// End all active sessions
	s.mu.Lock()
	sessions := make([]*Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	s.sessions = make(map[string]*Session)
	s.mu.Unlock()

	for _, session := range sessions {
		session.End()
	}

	s.logger.Info("SIP server stopped", "sessions_ended", len(sessions))
}

// SetConfigResolver sets the callback for resolving tenant-specific config
func (s *Server) SetConfigResolver(resolver ConfigResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configResolver = resolver
}

// IsRunning returns true if the server is running
func (s *Server) IsRunning() bool {
	return s.state.Load() == int32(ServerStateRunning)
}

// SessionCount returns the number of active sessions
func (s *Server) SessionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// SetOnInvite sets the callback for incoming INVITE requests
func (s *Server) SetOnInvite(fn func(session *Session, fromURI, toURI string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onInvite = fn
}

// SetOnBye sets the callback for BYE requests
func (s *Server) SetOnBye(fn func(session *Session) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onBye = fn
}

// SetOnCancel sets the callback for CANCEL requests
func (s *Server) SetOnCancel(fn func(session *Session) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onCancel = fn
}

// SetOnError sets the callback for error events
func (s *Server) SetOnError(fn func(session *Session, err error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onError = fn
}

func (s *Server) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()
	fromURI := req.From().Address.String()
	toURI := req.To().Address.String()

	s.logger.Info("Received INVITE", "call_id", callID, "from", fromURI, "to", toURI)

	// Extract authentication headers
	apiKey := s.extractHeader(req, "X-API-Key")
	if apiKey == "" {
		// Also check X-Api-Key (case variation)
		apiKey = s.extractHeader(req, "X-Api-Key")
	}

	// Parse assistant ID from To URI
	// Format: sip:{assistantID}.rapid-sip@in.rapida.ai
	assistantID := parseAssistantIDFromURI(toURI)

	// Extract additional headers for context
	headers := s.extractAllHeaders(req)

	s.logger.Debug("Parsed INVITE context",
		"call_id", callID,
		"assistant_id", assistantID,
		"has_api_key", apiKey != "")

	// Parse SDP from incoming INVITE to get remote RTP address and codec preferences
	sdpInfo, err := ParseSDP(req.Body())
	if err != nil {
		s.logger.Warn("Failed to parse SDP, using defaults", "error", err, "call_id", callID)
		sdpInfo = &SDPMediaInfo{PreferredCodec: &CodecPCMU}
	}

	// Multi-tenant: Resolve tenant-specific config from SIP headers
	s.mu.RLock()
	resolver := s.configResolver
	s.mu.RUnlock()

	var tenantConfig *Config
	if resolver != nil {
		inviteCtx := &InviteContext{
			CallID:      callID,
			FromURI:     fromURI,
			ToURI:       toURI,
			SDPInfo:     sdpInfo,
			APIKey:      apiKey,
			AssistantID: assistantID,
			Headers:     headers,
		}
		result, err := resolver(inviteCtx)
		if err != nil {
			s.logger.Error("Config resolution failed", "error", err, "call_id", callID)
			s.sendResponse(tx, req, 500)
			return
		}
		if !result.ShouldAllow {
			s.logger.Warn("Call rejected by config resolver",
				"call_id", callID,
				"code", result.RejectCode,
				"reason", result.RejectMsg)
			s.sendResponse(tx, req, result.RejectCode)
			return
		}
		tenantConfig = result.Config
	}

	// Fall back to defaults if no resolver or no config returned
	if tenantConfig == nil {
		tenantConfig = DefaultConfig()
		s.logger.Debug("Using default config for call", "call_id", callID)
	}

	// Negotiate codec
	negotiatedCodec := sdpInfo.PreferredCodec
	if negotiatedCodec == nil {
		negotiatedCodec = &CodecPCMU
	}

	// Create session with resolved tenant config
	session, err := NewSession(s.ctx, &SessionConfig{
		Config:    tenantConfig,
		Direction: CallDirectionInbound,
		CallID:    callID,
		Codec:     negotiatedCodec,
		Logger:    s.logger,
	})
	if err != nil {
		s.logger.Error("Failed to create session", "error", err, "call_id", callID)
		s.sendResponse(tx, req, 500)
		return
	}

	// Register session
	s.mu.Lock()
	s.sessions[callID] = session
	s.sessionCount.Add(1)
	s.mu.Unlock()

	// Send 100 Trying
	s.sendResponse(tx, req, 100)

	// Send 180 Ringing
	s.sendResponse(tx, req, 180)
	session.SetState(CallStateRinging)

	s.logger.Debug("Parsed remote SDP",
		"call_id", callID,
		"remote_rtp_ip", sdpInfo.ConnectionIP,
		"remote_rtp_port", sdpInfo.AudioPort,
		"codec", negotiatedCodec.Name)

	// Create RTP handler with tenant-specific config (RTP port range, etc.)
	rtpHandler, err := NewRTPHandler(s.ctx, &RTPConfig{
		LocalIP:     tenantConfig.Server,
		LocalPort:   tenantConfig.RTPPortRangeStart, // Tenant-specific RTP port range
		PayloadType: negotiatedCodec.PayloadType,
		ClockRate:   negotiatedCodec.ClockRate,
		Logger:      s.logger,
	})
	if err != nil {
		s.logger.Error("Failed to create RTP handler", "error", err, "call_id", callID)
		s.removeSession(callID)
		s.sendResponse(tx, req, 500)
		return
	}

	// Set remote RTP address from incoming SDP
	if sdpInfo.ConnectionIP != "" && sdpInfo.AudioPort > 0 {
		rtpHandler.SetRemoteAddr(sdpInfo.ConnectionIP, sdpInfo.AudioPort)
		session.SetRemoteRTP(sdpInfo.ConnectionIP, sdpInfo.AudioPort)
	}

	// Get local RTP address
	localIP, localPort := rtpHandler.LocalAddr()
	session.SetLocalRTP(localIP, localPort)
	session.SetNegotiatedCodec(negotiatedCodec.Name, int(negotiatedCodec.ClockRate))

	// Store the RTP handler in the session
	session.SetRTPHandler(rtpHandler)

	// Start RTP processing
	rtpHandler.Start()

	// Generate SDP for response
	sdpConfig := DefaultSDPConfig(localIP, localPort)
	sdpBody := GenerateSDP(sdpConfig)

	// Send 200 OK with SDP
	s.sendResponseWithSDPBody(tx, req, sdpBody)
	session.SetState(CallStateConnected)

	s.logger.Info("SIP call answered",
		"call_id", callID,
		"local_rtp", fmt.Sprintf("%s:%d", localIP, localPort),
		"remote_rtp", fmt.Sprintf("%s:%d", sdpInfo.ConnectionIP, sdpInfo.AudioPort),
		"codec", negotiatedCodec.Name)

	// Call the invite handler (which will start the conversation)
	s.mu.RLock()
	onInvite := s.onInvite
	s.mu.RUnlock()

	if onInvite != nil {
		if err := onInvite(session, fromURI, toURI); err != nil {
			s.logger.Error("INVITE handler failed", "error", err, "call_id", callID)
			s.notifyError(session, err)
		}
	}
}

// removeSession removes a session from the sessions map
func (s *Server) removeSession(callID string) {
	s.mu.Lock()
	if _, exists := s.sessions[callID]; exists {
		delete(s.sessions, callID)
		s.sessionCount.Add(-1)
	}
	s.mu.Unlock()
}

// notifyError notifies the error handler if set
func (s *Server) notifyError(session *Session, err error) {
	s.mu.RLock()
	onError := s.onError
	s.mu.RUnlock()

	if onError != nil {
		onError(session, err)
	}
}

func (s *Server) handleAck(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()

	s.mu.RLock()
	session, exists := s.sessions[callID]
	s.mu.RUnlock()

	if !exists {
		s.logger.Warn("ACK received for unknown session", "call_id", callID)
		return
	}

	session.SetState(CallStateConnected)
	s.logger.Debug("SIP call established (ACK received)", "call_id", callID)
}

func (s *Server) handleBye(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()

	s.mu.Lock()
	session, exists := s.sessions[callID]
	if exists {
		delete(s.sessions, callID)
		s.sessionCount.Add(-1)
	}
	s.mu.Unlock()

	if !exists {
		s.logger.Warn("BYE received for unknown session", "call_id", callID)
		s.sendResponse(tx, req, 481) // Call/Transaction Does Not Exist
		return
	}

	// Get callback before calling it
	s.mu.RLock()
	onBye := s.onBye
	s.mu.RUnlock()

	if onBye != nil {
		if err := onBye(session); err != nil {
			s.logger.Warn("BYE handler returned error", "error", err, "call_id", callID)
		}
	}

	session.End()
	s.sendResponse(tx, req, 200) // OK
	info := session.GetInfo()
	s.logger.Info("SIP call ended (BYE received)", "call_id", callID, "duration", info.Duration)
}

func (s *Server) handleCancel(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()

	s.mu.Lock()
	session, exists := s.sessions[callID]
	if exists {
		delete(s.sessions, callID)
		s.sessionCount.Add(-1)
	}
	s.mu.Unlock()

	if !exists {
		s.logger.Warn("CANCEL received for unknown session", "call_id", callID)
		s.sendResponse(tx, req, 481) // Call/Transaction Does Not Exist
		return
	}

	// Get callback before calling it
	s.mu.RLock()
	onCancel := s.onCancel
	s.mu.RUnlock()

	if onCancel != nil {
		if err := onCancel(session); err != nil {
			s.logger.Warn("CANCEL handler returned error", "error", err, "call_id", callID)
		}
	}

	session.End()
	s.sendResponse(tx, req, 200) // OK
	s.logger.Info("SIP call cancelled", "call_id", callID)
}

func (s *Server) handleRegister(req *sip.Request, tx sip.ServerTransaction) {
	s.logger.Debug("REGISTER received")
	s.sendResponse(tx, req, 200) // OK
}

func (s *Server) handleOptions(req *sip.Request, tx sip.ServerTransaction) {
	s.logger.Debug("OPTIONS received")
	s.sendResponse(tx, req, 200) // OK
}

func (s *Server) sendResponse(tx sip.ServerTransaction, req *sip.Request, statusCode int) {
	resp := sip.NewResponseFromRequest(req, statusCode, "", nil)
	if err := tx.Respond(resp); err != nil {
		s.logger.Error("Failed to send SIP response",
			"error", err,
			"status", statusCode,
			"call_id", req.CallID().Value())
	}
}

// sendResponseWithSDPBody sends a SIP 200 OK response with the given SDP body
func (s *Server) sendResponseWithSDPBody(tx sip.ServerTransaction, req *sip.Request, sdpBody string) {
	resp := sip.NewSDPResponseFromRequest(req, []byte(sdpBody))
	if err := tx.Respond(resp); err != nil {
		s.logger.Error("Failed to send SIP response with SDP",
			"error", err,
			"call_id", req.CallID().Value())
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

// extractHeader extracts a specific header value from a SIP request
func (s *Server) extractHeader(req *sip.Request, headerName string) string {
	h := req.GetHeader(headerName)
	if h == nil {
		return ""
	}
	return h.Value()
}

// extractAllHeaders extracts all headers from a SIP request into a map
func (s *Server) extractAllHeaders(req *sip.Request) map[string]string {
	headers := make(map[string]string)

	// Extract common custom headers that might be useful
	customHeaders := []string{
		"X-API-Key",
		"X-Api-Key",
		"X-Tenant-ID",
		"X-Assistant-ID",
		"X-Caller-ID",
		"X-Session-ID",
		"User-Agent",
	}

	for _, name := range customHeaders {
		if value := s.extractHeader(req, name); value != "" {
			headers[name] = value
		}
	}

	return headers
}

// parseAssistantIDFromURI extracts assistant ID from SIP URI
// Supported formats:
//   - sip:{assistantID}.rapid-sip@in.rapida.ai → {assistantID}
//   - sip:{assistantID}@in.rapida.ai → {assistantID}
//   - sip:+{phoneNumber}@domain.com → +{phoneNumber}
func parseAssistantIDFromURI(uri string) string {
	// Remove sip: prefix
	uri = strings.TrimPrefix(uri, "sip:")
	uri = strings.TrimPrefix(uri, "sips:")

	// Split user@host
	parts := strings.SplitN(uri, "@", 2)
	if len(parts) == 0 {
		return ""
	}
	user := parts[0]

	// Check for {assistantID}.rapid-sip format
	if idx := strings.Index(user, ".rapid-sip"); idx > 0 {
		return user[:idx]
	}

	// Check for {assistantID}.rapida format
	if idx := strings.Index(user, ".rapida"); idx > 0 {
		return user[:idx]
	}

	// Return the user part as-is (could be phone number or assistant ID)
	return user
}
