-- +goose Up
-- Rename: scans are backups, copies are restores. SQLite updates the
-- references in other tables and in index definitions; index names stay.
ALTER TABLE scans RENAME TO backups;
ALTER TABLE backups RENAME COLUMN results_ready TO ready;
ALTER TABLE scan_roots RENAME TO backup_roots;
ALTER TABLE backup_roots RENAME COLUMN scan_id TO backup_id;
ALTER TABLE scan_dirs RENAME TO backup_dirs;
ALTER TABLE backup_dirs RENAME COLUMN scan_id TO backup_id;
ALTER TABLE items RENAME COLUMN scan_id TO backup_id;
ALTER TABLE copies RENAME TO restores;
ALTER TABLE restores RENAME COLUMN scan_id TO backup_id;
ALTER TABLE copy_items RENAME TO restore_items;
ALTER TABLE restore_items RENAME COLUMN copy_id TO restore_id;

-- +goose Down
ALTER TABLE restore_items RENAME COLUMN restore_id TO copy_id;
ALTER TABLE restore_items RENAME TO copy_items;
ALTER TABLE restores RENAME COLUMN backup_id TO scan_id;
ALTER TABLE restores RENAME TO copies;
ALTER TABLE items RENAME COLUMN backup_id TO scan_id;
ALTER TABLE backup_dirs RENAME COLUMN backup_id TO scan_id;
ALTER TABLE backup_dirs RENAME TO scan_dirs;
ALTER TABLE backup_roots RENAME COLUMN backup_id TO scan_id;
ALTER TABLE backup_roots RENAME TO scan_roots;
ALTER TABLE backups RENAME COLUMN ready TO results_ready;
ALTER TABLE backups RENAME TO scans;
