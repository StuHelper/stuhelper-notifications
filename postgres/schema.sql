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
-- Retain inbox results independently of notification retention.
CREATE TABLE IF NOT EXISTS notifications.inbox (
 source text NOT NULL,
 event_id text NOT NULL,
 subject text NOT NULL,
 revision bigint NOT NULL CHECK (revision>0),
 fingerprint text NOT NULL,
 item_id uuid,
 suppressed boolean NOT NULL,
 ignored boolean NOT NULL,
 PRIMARY KEY(source,event_id),
 CHECK (NOT (suppressed AND ignored)),
 CHECK ((item_id IS NOT NULL) = (NOT suppressed AND NOT ignored))
);
CREATE TABLE IF NOT EXISTS notifications.revisions (
 source text NOT NULL,
 subject text NOT NULL,
 revision bigint NOT NULL CHECK (revision>=0),
 PRIMARY KEY(source,subject)
);
REVOKE ALL ON SCHEMA notifications FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA notifications FROM PUBLIC;
