package store

import "context"

// Truncate empties every table. It lives in a _test.go file so it only exists
// in test binaries and can never be called by the api.
func (p *Postgres) Truncate(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, `TRUNCATE uploads, upload_parts, assets, jobs, characters`)
	return err
}
