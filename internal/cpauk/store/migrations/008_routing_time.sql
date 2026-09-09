ALTER TABLE events ADD COLUMN routing_time_ms INTEGER CHECK(routing_time_ms >= 0);
