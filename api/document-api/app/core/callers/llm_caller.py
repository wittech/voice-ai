"""
author: prashant.srivastav
"""
import logging
from typing import Dict

from app.bridges.artifacts.protos.integration_api_pb2 import EmbeddingRequest
from app.bridges.bridge_factory import get_me_integration_client
from app.bridges.internals.integration_bridge import IntegrationBridge
from app.configs.internal_service_config import InternalServiceConfig
from app.exceptions import RapidaException

_log = logging.getLogger("app.core.callers.llm_caller")


class LLMCaller:
    integration_client: IntegrationBridge

    def __init__(self, cfg: InternalServiceConfig):
        self.integration_client = get_me_integration_client(cfg.integration_host)

    # async def get_text_message(self, message):
    #     return commons.to_message_content(message).string_content()
    #
    # async def complete(self, ctx, auth, provider_model, request):
    #     try:
    #         res = await self.integration_client.Generate(
    #             ctx, auth, provider_model.provider.name, request
    #         )
    #         return res
    #     except grpc.RpcError as err:
    #         self.logger.debug(f"Error complete from LLM {err}")
    #         raise err
    #
    # async def chat(self, ctx, auth, provider_model, request):
    #     start = time.time()
    #     response, err = await self.get_chat(ctx, auth, provider_model.provider.name, request)
    #     self.logger.benchmark("llmCaller.Chat", time.time() - start)
    #     return response, err
    #
    # async def get_chat(self, ctx, auth, provider_name, request):
    #     try:
    #         res = await self.integration_client.Chat(ctx, auth, provider_name, request)
    #         return res
    #     except grpc.RpcError as err:
    #         raise err
    #
    async def get_embedding(self, auth: str, provider_name: str, request: EmbeddingRequest) -> Dict:
        try:
            return await self.integration_client.get_embedding(auth, provider_name, request)
        except RapidaException as err:
            _log.debug(f"Error while creating embedding from LLM {err}")
            raise err
    #
    # async def get_reranking(self, ctx, auth, provider_name, request):
    #     try:
    #         res = await self.integration_client.Reranking(ctx, auth, provider_name, request)
    #         return res
    #     except grpc.RpcError as err:
    #         self.logger.debug(f"Error while reranking from LLM {err}")
    #         raise err
    #
    # def system_text_prompt(self, system_instruction, instruction_variables):
    #     return self.format_prompt(system_instruction, instruction_variables)
    #
    # def within_message(self, role, message):
    #     return lexatic_backend_pb2.Message(
    #         Role=role,
    #         Contents=[
    #             lexatic_backend_pb2.Content(
    #                 ContentType=commons.TEXT_CONTENT,
    #                 ContentFormat=commons.TEXT_CONTENT_FORMAT_RAW,
    #                 Content=message.encode('utf-8')
    #             )
    #         ]
    #     )
    #
    # def format_prompt(self, prompt, args):
    #     if args is None:
    #         return prompt
    #     parser = PromptTemplateParser(prompt, False)
    #     return parser.format(args, True)
