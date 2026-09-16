ALTER TABLE models ADD COLUMN endpoint TEXT NOT NULL DEFAULT 'chat'; -- chat | transcription | image | speech

-- The configured default audio model is a transcriber.
UPDATE models SET endpoint = 'transcription'
 WHERE model_id = (SELECT value FROM config WHERE key = 'default_audio_model');

UPDATE config SET key = 'default_transcription_model' WHERE key = 'default_audio_model';
