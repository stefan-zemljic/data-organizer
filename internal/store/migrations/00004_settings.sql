-- +goose Up
-- Small named values of the app, e.g. the access key of the web UI.
CREATE TABLE settings (
    name  TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- +goose Down
DROP TABLE settings;
