ALTER TABLE events ADD COLUMN routing_trace_json TEXT CHECK(routing_trace_json IS NULL OR json_valid(routing_trace_json));
