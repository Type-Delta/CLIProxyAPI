-- Session digests are hashed by the collector. Historic events remain NULL and
-- therefore do not participate in distinct-session metrics.
ALTER TABLE events ADD COLUMN session_digest TEXT CHECK(session_digest IS NULL OR length(session_digest) = 64);
