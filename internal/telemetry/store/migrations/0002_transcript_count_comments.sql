-- Agents read these comments through /v8/management/telemetry/schema. Entry IDs are shared across
-- sessions with identical history, so counting them alone undercounts per-session occurrences.
ALTER TABLE {database}.transcript_entries COMMENT COLUMN entry_id 'v2 occurrence ID: unique provider calls/results anchor to actor, protocol, role, kind and sorted call IDs, other content hashes its parent and semantics. Shared across sessions with identical history: count per-session membership through client request_contexts';
ALTER TABLE {database}.transcript_entries MODIFY COMMENT 'Entries can be shared across sessions. Join client request_contexts and count distinct (session_id, entry_id). Monthly first-membership recipe: /llms/sql.md.';
ALTER TABLE {database}.transcript_content COMMENT COLUMN text 'Searchable text. Join entries and client request_contexts for distinct (session_id, entry_id) counts';
ALTER TABLE {database}.transcript_content MODIFY COMMENT 'Content-addressed elements and envelopes. For per-session counts join entries and client request_contexts, see /llms/sql.md.';
