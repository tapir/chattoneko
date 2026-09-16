-- A chat completions model only ever produces text.
ALTER TABLE models DROP COLUMN output_modality;
