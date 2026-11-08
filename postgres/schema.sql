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
 read_at timestamptz,
 count bigint NOT NULL DEFAULT 1 CHECK (count>0)
);
CREATE INDEX IF NOT EXISTS items_owner_cursor ON notifications.items(owner,created_at DESC,id DESC);
CREATE INDEX IF NOT EXISTS items_unread ON notifications.items(owner) WHERE read_at IS NULL;
CREATE INDEX IF NOT EXISTS items_expiry ON notifications.items(created_at,id);
CREATE INDEX IF NOT EXISTS items_aggregation ON notifications.items(owner,source_module,target_kind,target_id,title_key,created_at DESC) WHERE type='interaction' AND read_at IS NULL;
-- Receipts survive inbox retention: replaying an old event cannot resurrect it.
CREATE TABLE IF NOT EXISTS notifications.receipts (
 owner text NOT NULL,
 source_module text NOT NULL,
 key text NOT NULL,
 fingerprint text NOT NULL,
 item_id uuid,
 PRIMARY KEY(owner,source_module,key)
);
REVOKE ALL ON SCHEMA notifications FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA notifications FROM PUBLIC;
