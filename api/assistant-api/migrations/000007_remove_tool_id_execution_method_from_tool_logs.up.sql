-- Remove assistant_tool_id, execution_method columns and the foreign key
-- reference from assistant_tool_logs. These fields are no longer needed since
-- tool logging is now centralised in OnPacket and agentkit tools have no
-- local entity or execution method.
-- Also add tool_call_id to support create-then-update pattern keyed by the
-- LLM-provided tool call identifier.

ALTER TABLE public.assistant_tool_logs
    DROP COLUMN IF EXISTS assistant_tool_id,
    DROP COLUMN IF EXISTS execution_method,
    ADD COLUMN tool_call_id character varying(255) NOT NULL DEFAULT '';

-- Backfill existing rows so the unique index doesn't conflict on empty values.
UPDATE public.assistant_tool_logs
    SET tool_call_id = 'call_' || id
    WHERE tool_call_id = '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_assistant_tool_logs_tool_call_conversation
    ON public.assistant_tool_logs (tool_call_id, assistant_conversation_id);
