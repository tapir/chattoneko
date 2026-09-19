-- Stored pictures are always plain PNGs now; the toggle that picked an indexed
-- one is gone, so its row must not survive in an existing database.
DELETE FROM config WHERE key = 'image_quantization';
