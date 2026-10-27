package store

import (
	"context"
	"crypto/rand"
)

// AccessKey returns the secret that unlocks the web UI, creating it on first
// use. It is kept in the database so browser sessions survive restarts.
func (s *Store) AccessKey(ctx context.Context) (string, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (name, value) VALUES ('access_key', ?) ON CONFLICT (name) DO NOTHING`,
		rand.Text())
	if err != nil {
		return "", err
	}
	var key string
	err = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = 'access_key'`).Scan(&key)
	return key, err
}
