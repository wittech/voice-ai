// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package assistant_socket

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/rapidaai/api/assistant-api/config"
	internal_adapter "github.com/rapidaai/api/assistant-api/internal/adapters"
	internal_assistant_entity "github.com/rapidaai/api/assistant-api/internal/entity/assistants"
	internal_conversation_entity "github.com/rapidaai/api/assistant-api/internal/entity/conversations"
	internal_services "github.com/rapidaai/api/assistant-api/internal/services"
	internal_assistant_service "github.com/rapidaai/api/assistant-api/internal/services/assistant"
	internal_telephony "github.com/rapidaai/api/assistant-api/internal/telephony"
	web_client "github.com/rapidaai/pkg/clients/web"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/pkg/connectors"
	"github.com/rapidaai/pkg/storages"
	storage_files "github.com/rapidaai/pkg/storages/file-storage"
	"github.com/rapidaai/pkg/types"
	type_enums "github.com/rapidaai/pkg/types/enums"
	"github.com/rapidaai/pkg/utils"
)

// AudioSocketManager manages Asterisk AudioSocket TCP connections.
type AudioSocketManager struct {
	logger   commons.Logger
	cfg      *config.AssistantConfig
	listener net.Listener
	mu       sync.RWMutex

	postgres   connectors.PostgresConnector
	redis      connectors.RedisConnector
	opensearch connectors.OpenSearchConnector
	storage    storages.Storage

	assistantConversationService internal_services.AssistantConversationService
	assistantService             internal_services.AssistantService
	vaultClient                  web_client.VaultClient
	authClient                   web_client.AuthClient
}

// NewAudioSocketManager creates a new AudioSocket manager.
func NewAudioSocketManager(config *config.AssistantConfig, logger commons.Logger,
	postgres connectors.PostgresConnector,
	redis connectors.RedisConnector,
	opensearch connectors.OpenSearchConnector,
	vectordb connectors.VectorConnector) *AudioSocketManager {
	return &AudioSocketManager{
		cfg:                          config,
		logger:                       logger,
		postgres:                     postgres,
		redis:                        redis,
		opensearch:                   opensearch,
		assistantConversationService: internal_assistant_service.NewAssistantConversationService(logger, postgres, storage_files.NewStorage(config.AssetStoreConfig, logger)),
		assistantService:             internal_assistant_service.NewAssistantService(config, logger, postgres, opensearch),
		storage:                      storage_files.NewStorage(config.AssetStoreConfig, logger),
		vaultClient:                  web_client.NewVaultClientGRPC(&config.AppConfig, logger, redis),
		authClient:                   web_client.NewAuthenticator(&config.AppConfig, logger, redis),
	}
}

// Start begins the AudioSocket TCP listener.
func (m *AudioSocketManager) Start(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", m.cfg.AudioSocketConfig.Host, m.cfg.AudioSocketConfig.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("audiosocket listen failed: %w", err)
	}
	m.listener = listener

	m.logger.Info("AudioSocket server started", "addr", addr)

	go m.acceptLoop(ctx)
	return nil
}

// Close stops the AudioSocket listener.
func (m *AudioSocketManager) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listener == nil {
		return nil
	}
	_ = m.listener.Close()
	m.listener = nil
	return nil
}

func (m *AudioSocketManager) acceptLoop(ctx context.Context) {
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.logger.Warn("AudioSocket accept error", "error", err)
			continue
		}

		go m.handleConnection(ctx, conn)
	}
}

func (m *AudioSocketManager) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	auth, assistant, conversation, callerID, err := m.resolveCallContext(connCtx, "channelID")
	if err != nil {
		m.logger.Warn("AudioSocket context resolution failed", "error", err)
		return
	}

	streamer, err := internal_telephony.Telephony(internal_telephony.Asterisk).AudioSocketStreamer(m.logger, conn, reader, writer, assistant, conversation, nil)
	if err != nil {
		m.logger.Warn("AudioSocket streamer create failed", "error", err)
		return
	}

	// if s, ok := streamer.(*internal_telephony.Streamer); ok {
	// 	s.SetInitialUUID("channelID")

	identifier := internal_adapter.Identifier(utils.PhoneCall, connCtx, auth, callerID)
	talker, err := internal_adapter.GetTalker(
		utils.PhoneCall,
		connCtx,
		m.cfg,
		m.logger,
		m.postgres,
		m.opensearch,
		m.redis,
		m.storage,
		streamer,
	)
	if err != nil {
		m.logger.Warn("AudioSocket talker create failed", "error", err)
		return
	}

	if err := talker.Talk(connCtx, auth, identifier); err != nil {
		m.logger.Warn("AudioSocket talker exited", "error", err)
	}

}

// resolveCallContext parses the UUID which contains apiKey:assistantId format
// Example: rpd-prj-xxx:123 or rpd-prj-xxx:123:callerID
func (m *AudioSocketManager) resolveCallContext(ctx context.Context, uuidParam string) (types.SimplePrinciple, *internal_assistant_entity.Assistant, *internal_conversation_entity.AssistantConversation, string, error) {

	auth := &types.ServiceScope{
		ProjectId:      utils.Ptr(uint64(2257831930382778368)),
		OrganizationId: utils.Ptr(uint64(2257831925018263552)),
		CurrentToken:   "3dd5c2eef53d27942bccd892750fda23ea0b92965d4699e73d8e754ab882955f",
	}

	assistant, err := m.assistantService.Get(ctx, auth, 2263072539095859200, utils.GetVersionDefinition("latest"), &internal_services.GetAssistantOption{InjectPhoneDeployment: true})
	if err != nil {
		return nil, nil, nil, "", err
	}

	conversation, err := m.assistantConversationService.CreateConversation(
		ctx,
		auth,
		internal_adapter.Identifier(utils.PhoneCall, ctx, auth, "2263072539095859200"),
		assistant.Id,
		assistant.AssistantProviderId,
		type_enums.DIRECTION_INBOUND,
		utils.PhoneCall,
	)
	if err != nil {
		return nil, nil, nil, "", err
	}

	_, _ = m.assistantConversationService.ApplyConversationMetadata(ctx, auth, assistant.Id, conversation.Id,
		[]*types.Metadata{types.NewMetadata("telephony.uuid", uuidParam)})

	return auth, assistant, conversation, "2263072539095859200", nil
}
