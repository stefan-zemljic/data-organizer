-- +goose Up
-- Directories of unfinished scans as found (scan.Item.Listed), so a scan
-- can resume after a restart or crash; removed when the scan finishes.
CREATE TABLE scan_dirs (
    id        INTEGER PRIMARY KEY, -- the directory's item id
    scan_id   INTEGER NOT NULL REFERENCES scans (id),
    parent_id INTEGER,             -- NULL for roots
    name      TEXT    NOT NULL     -- the full path for roots
);
CREATE INDEX scan_dirs_scan ON scan_dirs (scan_id);

-- Scans that record scan_dirs; older unfinished scans cannot resume.
ALTER TABLE scans ADD COLUMN resumable INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE scans DROP COLUMN resumable;
DROP TABLE scan_dirs;
