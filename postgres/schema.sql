CREATE SCHEMA IF NOT EXISTS notifications;
CREATE TABLE IF NOT EXISTS notifications.closed_owners (
 owner text PRIMARY KEY,
 closure_id text NOT NULL,
 closed_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS notifications.preferences (
 owner text PRIMARY KEY,
 interaction_enabled boolean NOT NULL DEFAULT true,
 reply_enabled boolean NOT NULL DEFAULT true,
 like_enabled boolean NOT NULL DEFAULT true
);
ALTER TABLE notifications.preferences ADD COLUMN IF NOT EXISTS reply_enabled boolean;
UPDATE notifications.preferences SET reply_enabled = interaction_enabled WHERE reply_enabled IS NULL;
ALTER TABLE notifications.preferences ALTER COLUMN reply_enabled SET DEFAULT true, ALTER COLUMN reply_enabled SET NOT NULL;
ALTER TABLE notifications.preferences ADD COLUMN IF NOT EXISTS like_enabled boolean;
UPDATE notifications.preferences SET like_enabled = interaction_enabled WHERE like_enabled IS NULL;
ALTER TABLE notifications.preferences ALTER COLUMN like_enabled SET DEFAULT true, ALTER COLUMN like_enabled SET NOT NULL;
CREATE TABLE IF NOT EXISTS notifications.items (
 id uuid PRIMARY KEY,
 owner text NOT NULL,
 box text NOT NULL DEFAULT 'system' CHECK (box IN ('interaction','review','verification','system')),
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
ALTER TABLE notifications.items ADD COLUMN IF NOT EXISTS box text;
UPDATE notifications.items SET box = CASE
 WHEN title_key LIKE 'notifications.interaction.%' THEN 'interaction'
 WHEN title_key LIKE 'notifications.content.%' OR title_key LIKE 'notifications.resource_removal.%' OR title_key LIKE 'notifications.report.%' OR title_key LIKE 'notifications.appeal.%' THEN 'review'
 WHEN split_part(substring(title_key FROM 15), '.', 1) LIKE '%\_verification' ESCAPE '\' OR split_part(substring(title_key FROM 15), '.', 1) LIKE 'verification\_%' ESCAPE '\' THEN 'verification'
 ELSE 'system'
END WHERE box IS NULL;
ALTER TABLE notifications.items ALTER COLUMN box SET DEFAULT 'system', ALTER COLUMN box SET NOT NULL;
DO $$
BEGIN
 IF NOT EXISTS (
  SELECT 1 FROM pg_constraint c
  JOIN pg_class t ON c.conrelid = t.oid
  JOIN pg_namespace n ON t.relnamespace = n.oid
  WHERE n.nspname = 'notifications' AND t.relname = 'items' AND c.conname = 'items_box_check'
 ) AND NOT EXISTS (
  SELECT 1 FROM pg_constraint c
  JOIN pg_class t ON c.conrelid = t.oid
  JOIN pg_namespace n ON t.relnamespace = n.oid
  WHERE n.nspname = 'notifications' AND t.relname = 'items' AND pg_get_constraintdef(c.oid) LIKE '%box%'
 ) THEN
  ALTER TABLE notifications.items ADD CONSTRAINT items_box_check CHECK (box IN ('interaction','review','verification','system'));
 END IF;
END $$;
CREATE INDEX IF NOT EXISTS items_owner_cursor ON notifications.items(owner,created_at DESC,id DESC);
CREATE INDEX IF NOT EXISTS items_owner_box_cursor ON notifications.items(owner,box,created_at DESC,id DESC);
CREATE INDEX IF NOT EXISTS items_owner_type_cursor ON notifications.items(owner,type,created_at DESC,id DESC);
CREATE INDEX IF NOT EXISTS items_owner_unread_cursor ON notifications.items(owner,created_at DESC,id DESC) WHERE read_at IS NULL;
CREATE INDEX IF NOT EXISTS items_owner_box_unread_cursor ON notifications.items(owner,box,created_at DESC,id DESC) WHERE read_at IS NULL;
CREATE INDEX IF NOT EXISTS items_owner_type_unread_cursor ON notifications.items(owner,type,created_at DESC,id DESC) WHERE read_at IS NULL;
CREATE INDEX IF NOT EXISTS items_unread ON notifications.items(owner) WHERE read_at IS NULL;
CREATE INDEX IF NOT EXISTS items_unread_grouped ON notifications.items(owner,box,type) WHERE read_at IS NULL;
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
 interaction_title_key text NOT NULL DEFAULT '',
 PRIMARY KEY(source,subject)
);
ALTER TABLE notifications.revisions ADD COLUMN IF NOT EXISTS interaction_title_key text NOT NULL DEFAULT '';
-- Existing receipts identify the recipient and item. Its title key establishes the interaction kind.
DO $$
BEGIN
 IF to_regclass('notifications.like_subjects') IS NULL THEN
  IF EXISTS(SELECT 1 FROM notifications.inbox WHERE result IS NULL) THEN
   RAISE EXCEPTION 'legacy delivery details expired; reconstruct interaction deduplication from source records before upgrading';
  END IF;
  CREATE TEMP TABLE legacy_interaction_delivery ON COMMIT DROP AS
   SELECT i.source,i.subject,r->>'recipient' AS owner,item.title_key
   FROM notifications.inbox i
   CROSS JOIN LATERAL jsonb_array_elements(i.result->'recipients') r
   LEFT JOIN notifications.items item ON item.id=nullif(r->>'id','')::uuid
      AND item.owner=r->>'recipient' AND item.source_module=i.source
   WHERE coalesce(r->>'id','')<>'' AND NOT EXISTS
     (SELECT 1 FROM notifications.closed_owners c WHERE c.owner=r->>'recipient');
  IF EXISTS(SELECT 1 FROM legacy_interaction_delivery WHERE title_key IS NULL)
     OR EXISTS(SELECT 1 FROM legacy_interaction_delivery
       WHERE title_key IN ('notifications.interaction.like','notifications.interaction.reply')
       GROUP BY source,subject HAVING count(DISTINCT title_key)>1) THEN
   RAISE EXCEPTION 'legacy delivery has missing or conflicting interaction evidence; source reconstruction required';
  END IF;
  CREATE TABLE notifications.like_subjects (
   owner text NOT NULL,source text NOT NULL,subject text NOT NULL,PRIMARY KEY(owner,source,subject)
  );
  INSERT INTO notifications.like_subjects(owner,source,subject)
   SELECT DISTINCT owner,source,subject FROM legacy_interaction_delivery WHERE title_key='notifications.interaction.like';
  UPDATE notifications.revisions rev SET interaction_title_key=known.title_key
   FROM (SELECT DISTINCT source,subject,title_key FROM legacy_interaction_delivery
         WHERE title_key IN ('notifications.interaction.like','notifications.interaction.reply')) known
   WHERE rev.source=known.source AND rev.subject=known.subject;
 END IF;
END $$;

-- The runtime may insert immutable delivery facts. Only an account closure can erase them.
CREATE OR REPLACE FUNCTION notifications.clear_closed_like_subjects() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 DELETE FROM notifications.like_subjects WHERE owner=NEW.owner;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION notifications.clear_closed_like_subjects() FROM PUBLIC;
CREATE OR REPLACE TRIGGER clear_closed_like_subjects AFTER INSERT ON notifications.closed_owners
FOR EACH ROW EXECUTE FUNCTION notifications.clear_closed_like_subjects();
REVOKE ALL ON SCHEMA notifications FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA notifications FROM PUBLIC;
