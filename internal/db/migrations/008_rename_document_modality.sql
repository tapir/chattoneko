-- OpenRouter's /models reports "file" for PDF input, so stored modalities use
-- that word: a fetched model needs no translation and its chip arrives
-- pre-selected.
UPDATE models SET input_modality = replace(input_modality, '"document"', '"file"')
 WHERE input_modality LIKE '%"document"%';
