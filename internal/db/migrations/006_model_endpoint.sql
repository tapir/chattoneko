-- A model is now added with the endpoint it is called through, so a
-- transcription model is its own kind instead of a chat model with a flag on.
ALTER TABLE models ADD COLUMN endpoint TEXT NOT NULL DEFAULT 'chat'; -- chat | transcription | image | speech

-- The model that carried the Audio flag was already a transcriber.
UPDATE models SET endpoint = 'transcription'
 WHERE model_id = (SELECT value FROM config WHERE key = 'default_audio_model');

-- The role is named after what it does.
UPDATE config SET key = 'default_transcription_model' WHERE key = 'default_audio_model';
