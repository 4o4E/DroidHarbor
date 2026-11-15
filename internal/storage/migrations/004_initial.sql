ALTER TABLE devices ADD COLUMN removed_at INTEGER;
CREATE TABLE system_preferences (
    id INTEGER PRIMARY KEY CHECK(id=1),
    audio_default TEXT NOT NULL CHECK(audio_default IN ('muted','device','browser')),
    window_scales TEXT NOT NULL CHECK(json_valid(window_scales)),
    revision INTEGER NOT NULL CHECK(revision>0)
) STRICT;
INSERT INTO system_preferences VALUES(1,'muted','[0.5,1,2]',1);
