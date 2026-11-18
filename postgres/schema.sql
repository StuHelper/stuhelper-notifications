CREATE SCHEMA IF NOT EXISTS notifications;
CREATE TABLE IF NOT EXISTS notifications.closed_owners (
 owner text PRIMARY KEY,
 closure_id text NOT NULL,
 closed_at timestamptz NOT NULL DEFAULT now()
);
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
 read_at timestamptz,
 count bigint NOT NULL DEFAULT 1 CHECK (count>0)
);
CREATE INDEX IF NOT EXISTS items_owner_cursor ON notifications.items(owner,created_at DESC,id DESC);
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
-- Retain inbox deduplication keys independently of notification retention;
-- recipient delivery details in result may be pruned after the retention window.
CREATE TABLE IF NOT EXISTS notifications.inbox (
 source text NOT NULL,
 event_id text NOT NULL,
 subject text NOT NULL,
 revision bigint NOT NULL CHECK (revision>0),
 fingerprint text NOT NULL,
 result jsonb,
 consumed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(source,event_id)
);
ALTER TABLE notifications.inbox ADD COLUMN IF NOT EXISTS consumed_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE notifications.inbox ALTER COLUMN result DROP NOT NULL;
ALTER TABLE notifications.inbox DROP CONSTRAINT IF EXISTS inbox_result_check;
ALTER TABLE notifications.inbox ADD CONSTRAINT inbox_result_check
 CHECK (result IS NULL OR (jsonb_typeof(result)='object' AND (result ? 'recipients') AND jsonb_typeof(result->'recipients')='array' AND jsonb_array_length(result->'recipients') BETWEEN 1 AND 20));
CREATE INDEX IF NOT EXISTS inbox_expiry ON notifications.inbox(consumed_at,source,event_id) WHERE result IS NOT NULL;
CREATE TABLE IF NOT EXISTS notifications.revisions (
 source text NOT NULL,
 subject text NOT NULL,
 revision bigint NOT NULL CHECK (revision>=0),
 PRIMARY KEY(source,subject)
);
REVOKE ALL ON SCHEMA notifications FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA notifications FROM PUBLIC;
