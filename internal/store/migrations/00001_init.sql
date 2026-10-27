-- +goose Up
CREATE TABLE scans (
    id            INTEGER PRIMARY KEY,
    status        TEXT    NOT NULL,
    started_at    INTEGER NOT NULL, -- unix milliseconds
    finished_at   INTEGER,
    first_item_id INTEGER,          -- items of a scan: ids from here to the next scan's
    dirs_found    INTEGER NOT NULL DEFAULT 0,
    dirs_done     INTEGER NOT NULL DEFAULT 0,
    files_found   INTEGER NOT NULL DEFAULT 0,
    files_done    INTEGER NOT NULL DEFAULT 0,
    bytes_found   INTEGER NOT NULL DEFAULT 0,
    bytes_done    INTEGER NOT NULL DEFAULT 0,
    errors        INTEGER NOT NULL DEFAULT 0,
    results_ready INTEGER NOT NULL DEFAULT 0,
    error         TEXT
);

CREATE TABLE scan_roots (
    scan_id  INTEGER NOT NULL REFERENCES scans (id),
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (scan_id, position)
) WITHOUT ROWID;

-- Every distinct item name once, shared by all scans.
CREATE TABLE names (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE items (
    id        INTEGER PRIMARY KEY,
    scan_id   INTEGER NOT NULL REFERENCES scans (id),
    parent_id INTEGER REFERENCES items (id),
    name_id   INTEGER NOT NULL REFERENCES names (id),
    kind      INTEGER NOT NULL, -- 0 file, 1 directory
    size      INTEGER,          -- files only
    mtime     INTEGER,          -- unix nanoseconds
    hash      BLOB,             -- 32 bytes; NULL when error is set; files: key of contents
    error     TEXT,             -- set when the item could not be read
    dup_count INTEGER NOT NULL DEFAULT 0, -- other items of the scan with the same hash
    dup_rank  INTEGER NOT NULL DEFAULT 0, -- 1 = first copy by path
    excluded  INTEGER NOT NULL DEFAULT 0  -- explicit mark only; ancestors are resolved on read
);
CREATE INDEX items_parent ON items (parent_id);
CREATE INDEX items_scan_roots ON items (scan_id) WHERE parent_id IS NULL;
CREATE INDEX items_dup_rank ON items (scan_id, hash, dup_rank) WHERE dup_count > 0;
CREATE INDEX items_excluded ON items (scan_id) WHERE excluded != 0;

-- File contents, stored once for all scans. The bytes live in pack files
-- next to the database (internal/content); these tables are their index.
CREATE TABLE chunks (
    id     INTEGER PRIMARY KEY,
    hash   BLOB    NOT NULL, -- sha256(0x00 ‖ data); unique, enforced by the writer
    pack   INTEGER NOT NULL, -- pack file number
    offset INTEGER NOT NULL, -- position in the pack
    stored INTEGER NOT NULL, -- bytes in the pack
    size   INTEGER NOT NULL, -- bytes of data
    codec  INTEGER NOT NULL  -- 0 raw, 1 zstd
);

CREATE TABLE contents (
    hash   BLOB    PRIMARY KEY, -- the file hash, as in items.hash
    size   INTEGER NOT NULL,
    chunks BLOB    NOT NULL     -- chunk ids in order, varint-encoded deltas
) WITHOUT ROWID;

CREATE TABLE copies (
    id           INTEGER PRIMARY KEY,
    scan_id      INTEGER NOT NULL REFERENCES scans (id),
    target       TEXT    NOT NULL,
    status       TEXT    NOT NULL,
    started_at   INTEGER NOT NULL, -- unix milliseconds
    finished_at  INTEGER,
    files_total  INTEGER NOT NULL DEFAULT 0,
    bytes_total  INTEGER NOT NULL DEFAULT 0,
    files_done   INTEGER NOT NULL DEFAULT 0,
    bytes_done   INTEGER NOT NULL DEFAULT 0,
    files_ok     INTEGER NOT NULL DEFAULT 0,
    files_failed INTEGER NOT NULL DEFAULT 0, -- not written, or stored content damaged
    dirs_created INTEGER NOT NULL DEFAULT 0,
    error        TEXT
);

CREATE TABLE copy_items (
    copy_id INTEGER NOT NULL REFERENCES copies (id),
    item_id INTEGER NOT NULL REFERENCES items (id),
    status  INTEGER NOT NULL, -- 1 ok, 2 failed
    flags   INTEGER NOT NULL DEFAULT 0, -- 1 = failures below
    error   TEXT,
    PRIMARY KEY (copy_id, item_id)
) WITHOUT ROWID;

-- +goose Down
DROP TABLE copy_items;
DROP TABLE copies;
DROP TABLE contents;
DROP TABLE chunks;
DROP TABLE items;
DROP TABLE names;
DROP TABLE scan_roots;
DROP TABLE scans;
