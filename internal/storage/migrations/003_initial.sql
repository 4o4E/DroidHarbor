CREATE TABLE global_video_settings (
    id INTEGER PRIMARY KEY CHECK(id = 1),
    max_fps INTEGER NOT NULL CHECK(max_fps > 0 AND max_fps <= 2147483647),
    bit_rate INTEGER NOT NULL CHECK(bit_rate > 0 AND bit_rate <= 2147483647),
    revision INTEGER NOT NULL CHECK(revision > 0)
) STRICT;
INSERT INTO global_video_settings(id,max_fps,bit_rate,revision) VALUES(1,60,12000000,1);
ALTER TABLE devices ADD COLUMN video_last_good TEXT
    CHECK(video_last_good IS NULL OR (json_valid(video_last_good) AND json_type(video_last_good) = 'object'));
ALTER TABLE devices DROP COLUMN video_max_fps;
ALTER TABLE devices DROP COLUMN video_bit_rate;
