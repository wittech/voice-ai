-- Re-add assistant_tool_id and execution_method to assistant_tool_logs
-- and remove tool_call_id
DROP INDEX IF EXISTS idx_assistant_tool_logs_tool_call_conversation;

ALTER TABLE public.assistant_tool_logs
    DROP COLUMN IF EXISTS tool_call_id,
    ADD COLUMN assistant_tool_id bigint,
    ADD COLUMN execution_method character varying(20);
