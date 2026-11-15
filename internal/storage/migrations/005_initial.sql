ALTER TABLE system_preferences ADD COLUMN audio_gain_max REAL NOT NULL DEFAULT 3.0 CHECK(audio_gain_max >= 1.0);
