-- A chat completions model only ever produces text, so the output modality
-- was a column nothing could vary.
ALTER TABLE models DROP COLUMN output_modality;
