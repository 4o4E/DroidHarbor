ALTER TABLE devices ADD COLUMN display_info TEXT
    CHECK(display_info IS NULL OR (json_valid(display_info) AND json_type(display_info) = 'object'));
