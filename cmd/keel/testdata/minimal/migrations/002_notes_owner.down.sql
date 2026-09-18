DROP INDEX IF EXISTS notes_user_created_at_id_idx;
ALTER TABLE notes DROP COLUMN IF EXISTS user_id;
