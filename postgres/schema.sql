CREATE SCHEMA IF NOT EXISTS notifications;
CREATE TABLE IF NOT EXISTS notifications.preferences (
 owner text PRIMARY KEY,
 interaction_enabled boolean NOT NULL DEFAULT true
);
CREATE TABLE IF NOT EXISTS notifications.items (
 id uuid PRIMARY KEY,
 owner text NOT NULL,
 type text NOT NULL CHECK (type IN ('interaction','business','security','account')),
 source_module text NOT NULL,
 target_kind text NOT NULL,
 target_id text NOT NULL,
 title_key text NOT NULL,
 title_params jsonb NOT NULL,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 read_at timestamptz,
 count bigint NOT NULL DEFAULT 1 CHECK (count>0),
 CHECK (updated_at >= created_at)
);
ALTER TABLE notifications.items ADD COLUMN IF NOT EXISTS updated_at timestamptz;
UPDATE notifications.items SET updated_at = created_at WHERE updated_at IS NULL;
-- Previous writers specify only created_at. Preserve their event timestamp on insert.
CREATE OR REPLACE FUNCTION notifications.set_initial_updated_at() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 NEW.updated_at:=NEW.created_at;
 RETURN NEW;
END $$;
CREATE OR REPLACE TRIGGER set_initial_updated_at BEFORE INSERT ON notifications.items
FOR EACH ROW WHEN (NEW.updated_at IS NULL) EXECUTE FUNCTION notifications.set_initial_updated_at();
ALTER TABLE notifications.items ALTER COLUMN updated_at SET NOT NULL;
DO $$
BEGIN
 IF NOT EXISTS (
  SELECT 1 FROM pg_constraint c
  JOIN pg_class t ON c.conrelid = t.oid
  JOIN pg_namespace n ON t.relnamespace = n.oid
  WHERE n.nspname = 'notifications' AND t.relname = 'items' AND pg_get_constraintdef(c.oid) LIKE '%updated_at%'
 ) THEN
  ALTER TABLE notifications.items ADD CONSTRAINT items_updated_at_check CHECK (updated_at >= created_at);
 END IF;
END $$;
CREATE INDEX IF NOT EXISTS items_owner_cursor ON notifications.items(owner,created_at DESC,id DESC);
CREATE INDEX IF NOT EXISTS items_owner_updated_cursor ON notifications.items(owner,updated_at DESC,id DESC);
CREATE INDEX IF NOT EXISTS items_unread ON notifications.items(owner) WHERE read_at IS NULL;
CREATE INDEX IF NOT EXISTS items_expiry ON notifications.items(created_at,id);
CREATE INDEX IF NOT EXISTS items_aggregation ON notifications.items(owner,source_module,target_kind,target_id,title_key,created_at DESC) WHERE type='interaction' AND read_at IS NULL;
-- Upgrade an empty candidate inbox in place. Existing single-recipient receipts
-- omit the recipient on suppression and cannot be converted without source events.
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='notifications' AND table_name='inbox' AND column_name='item_id') THEN
  IF EXISTS (SELECT 1 FROM notifications.inbox) THEN
   RAISE EXCEPTION 'notifications inbox migration requires recipient reconstruction from source events';
  END IF;
  ALTER TABLE notifications.inbox DROP COLUMN item_id, DROP COLUMN suppressed, DROP COLUMN ignored;
  ALTER TABLE notifications.inbox ADD COLUMN result jsonb NOT NULL
   CHECK (jsonb_typeof(result->'recipients')='array' AND jsonb_array_length(result->'recipients') BETWEEN 1 AND 20);
 END IF;
END $$;
-- Retain inbox results independently of notification retention.
CREATE TABLE IF NOT EXISTS notifications.inbox (
 source text NOT NULL,
 event_id text NOT NULL,
 subject text NOT NULL,
 revision bigint NOT NULL CHECK (revision>0),
 fingerprint text NOT NULL,
 result jsonb NOT NULL CHECK (jsonb_typeof(result->'recipients')='array' AND jsonb_array_length(result->'recipients') BETWEEN 1 AND 20),
 PRIMARY KEY(source,event_id)
);
CREATE TABLE IF NOT EXISTS notifications.revisions (
 source text NOT NULL,
 subject text NOT NULL,
 revision bigint NOT NULL CHECK (revision>=0),
 PRIMARY KEY(source,subject)
);
REVOKE ALL ON SCHEMA notifications FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA notifications FROM PUBLIC;
