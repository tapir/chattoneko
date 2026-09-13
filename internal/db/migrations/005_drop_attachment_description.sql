-- The image-description (vision) feature is gone; drop its cache column.
ALTER TABLE attachments DROP COLUMN description;
