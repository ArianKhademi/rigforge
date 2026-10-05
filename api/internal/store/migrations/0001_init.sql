-- Rigforge schema. The api owns migrations; the Python worker only reads and
-- updates rows in jobs and assets.

CREATE TABLE uploads (
    id            uuid PRIMARY KEY,
    user_id       text        NOT NULL,
    asset_id      uuid        NOT NULL UNIQUE,
    filename      text        NOT NULL,
    content_type  text        NOT NULL,
    size_bytes    bigint      NOT NULL CHECK (size_bytes > 0),
    part_size     bigint      NOT NULL CHECK (part_size > 0),
    part_count    integer     NOT NULL CHECK (part_count > 0),
    object_key    text        NOT NULL,
    s3_upload_id  text        NOT NULL,
    status        text        NOT NULL CHECK (status IN ('uploading', 'completed', 'aborted')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- The reaper scans for idle in-flight uploads; a partial index keeps that
-- scan proportional to the number of uploads actually in flight.
CREATE INDEX uploads_uploading_idx ON uploads (updated_at) WHERE status = 'uploading';

CREATE TABLE upload_parts (
    upload_id    uuid        NOT NULL REFERENCES uploads (id) ON DELETE CASCADE,
    part_number  integer     NOT NULL CHECK (part_number >= 1),
    etag         text        NOT NULL,
    recorded_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (upload_id, part_number)
);

CREATE TABLE assets (
    id                uuid PRIMARY KEY,
    user_id           text        NOT NULL,
    name              text        NOT NULL,
    source_key        text        NOT NULL,
    source_size       bigint      NOT NULL,
    content_type      text        NOT NULL,
    status            text        NOT NULL CHECK (status IN ('processing', 'ready', 'failed')),
    duration_seconds  double precision,
    width             integer,
    height            integer,
    fps               double precision,
    frame_count       integer,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX assets_user_idx ON assets (user_id, created_at DESC);

CREATE TABLE jobs (
    id            uuid PRIMARY KEY,
    asset_id      uuid        NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
    user_id       text        NOT NULL,
    type          text        NOT NULL,
    character_id  text        NOT NULL,
    status        text        NOT NULL CHECK (status IN
                      ('queued', 'transcoding', 'extracting', 'writing', 'uploading', 'done', 'failed')),
    progress      integer     NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    attempt       integer     NOT NULL DEFAULT 1,
    max_attempts  integer     NOT NULL DEFAULT 3,
    error         text,
    retry_at      timestamptz,
    enqueued_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    started_at    timestamptz,
    finished_at   timestamptz
);

CREATE INDEX jobs_asset_idx ON jobs (asset_id, created_at DESC);
-- Jobs committed but not yet confirmed on the Redis stream (see job.Dispatcher).
CREATE INDEX jobs_unenqueued_idx ON jobs (created_at) WHERE enqueued_at IS NULL;

CREATE TABLE characters (
    id          uuid PRIMARY KEY,
    user_id     text        NOT NULL,
    name        text        NOT NULL,
    object_key  text        NOT NULL,
    size_bytes  bigint      NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX characters_user_idx ON characters (user_id, created_at DESC);
