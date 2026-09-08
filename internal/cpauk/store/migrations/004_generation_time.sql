ALTER TABLE events ADD COLUMN generation_time_ms INTEGER CHECK(generation_time_ms >= 0);
