-- One row per upload. The row is both the file and its quota ledger entry:
-- it keeps counting against the owner's rolling window after the file is
-- deleted or expires (status deleted / expired, body purged from storage),
-- and is pruned once it has left the window.
CREATE TABLE files (
    id           text PRIMARY KEY,
    -- Quota key: auth user id (usr_…) or guest key (gst_…).
    owner        text NOT NULL,
    owner_kind   text NOT NULL CHECK (owner_kind IN ('account', 'guest')),
    -- Guests: hashed client IP, for the per-IP total.
    ip_key       text,
    name         text NOT NULL,
    content_type text NOT NULL,
    size         bigint NOT NULL CHECK (size > 0),
    storage_key  text NOT NULL,
    -- pending: reserved, body streaming in. ready: live. deleted / expired:
    -- gone for users (body purged when purged_at is set).
    status       text NOT NULL CHECK (status IN ('pending', 'ready', 'deleted', 'expired')),
    width        integer,
    height       integer,
    created_at   timestamptz NOT NULL,
    expires_at   timestamptz NOT NULL,
    deleted_at   timestamptz,
    deleted_by   text,
    purged_at    timestamptz
);

CREATE INDEX files_owner_created ON files (owner, created_at);
CREATE INDEX files_ip_created ON files (ip_key, created_at) WHERE ip_key IS NOT NULL;
CREATE INDEX files_created_id ON files (created_at DESC, id DESC);
CREATE INDEX files_ready_expires ON files (expires_at) WHERE status = 'ready';
CREATE INDEX files_pending_created ON files (created_at) WHERE status = 'pending';
CREATE INDEX files_unpurged ON files (created_at) WHERE purged_at IS NULL AND status IN ('deleted', 'expired');

-- Per-user limit overrides (PUT /internal/quota/{user}). NULL column = the
-- class default.
CREATE TABLE quota_overrides (
    owner          text PRIMARY KEY,
    quota_bytes    bigint CHECK (quota_bytes IS NULL OR quota_bytes > 0),
    max_file_bytes bigint CHECK (max_file_bytes IS NULL OR max_file_bytes > 0),
    updated_at     timestamptz NOT NULL,
    updated_by     text
);
