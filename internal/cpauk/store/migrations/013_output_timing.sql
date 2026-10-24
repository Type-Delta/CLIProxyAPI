ALTER TABLE events ADD COLUMN output_timing TEXT CHECK(output_timing IS NULL OR json_valid(output_timing));
ALTER TABLE events ADD COLUMN stream INTEGER CHECK(stream IS NULL OR stream IN (0, 1));
ALTER TABLE events ADD COLUMN reasoning_effort TEXT;
