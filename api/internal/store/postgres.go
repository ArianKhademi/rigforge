package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// migrationLockID is an arbitrary constant for pg_advisory_lock. Both api
// replicas run Migrate on boot; the lock makes the second one wait and then
// find nothing left to apply.
const migrationLockID = 7421001

// Migrate applies embedded SQL files in filename order, each in its own
// transaction, and records them in schema_migrations.
func (p *Postgres) Migrate(ctx context.Context) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		var applied bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		sql, err := migrationFiles.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ---- uploads ----

const uploadCols = `id, user_id, asset_id, filename, content_type, size_bytes, part_size,
	part_count, object_key, s3_upload_id, status, created_at, updated_at`

func scanUpload(row pgx.Row) (*Upload, error) {
	var u Upload
	err := row.Scan(&u.ID, &u.UserID, &u.AssetID, &u.Filename, &u.ContentType, &u.Size, &u.PartSize,
		&u.PartCount, &u.ObjectKey, &u.S3UploadID, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

func (p *Postgres) CreateUpload(ctx context.Context, u *Upload) error {
	return p.pool.QueryRow(ctx, `
		INSERT INTO uploads (id, user_id, asset_id, filename, content_type, size_bytes, part_size,
			part_count, object_key, s3_upload_id, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING created_at, updated_at`,
		u.ID, u.UserID, u.AssetID, u.Filename, u.ContentType, u.Size, u.PartSize,
		u.PartCount, u.ObjectKey, u.S3UploadID, u.Status,
	).Scan(&u.CreatedAt, &u.UpdatedAt)
}

func (p *Postgres) GetUpload(ctx context.Context, userID, id string) (*Upload, error) {
	return scanUpload(p.pool.QueryRow(ctx,
		`SELECT `+uploadCols+` FROM uploads WHERE id = $1 AND user_id = $2`, id, userID))
}

func (p *Postgres) ListParts(ctx context.Context, uploadID string) ([]Part, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT part_number, etag, recorded_at FROM upload_parts
		WHERE upload_id = $1 ORDER BY part_number`, uploadID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Part, error) {
		var pt Part
		err := row.Scan(&pt.PartNumber, &pt.ETag, &pt.RecordedAt)
		return pt, err
	})
}

func (p *Postgres) RecordPart(ctx context.Context, uploadID string, partNumber int, etag string) (Part, error) {
	// One statement, so it is atomic without an explicit transaction: the CTE
	// both checks that the upload is still in flight and bumps its idle clock;
	// the insert only happens if that row came back. Re-reporting the same ETag
	// keeps the original recorded_at, which is what lets the resume test tell a
	// duplicate report apart from a part that was really uploaded again.
	var pt Part
	err := p.pool.QueryRow(ctx, `
		WITH touched AS (
			UPDATE uploads SET updated_at = now()
			WHERE id = $1 AND status = 'uploading'
			RETURNING id
		)
		INSERT INTO upload_parts (upload_id, part_number, etag)
		SELECT id, $2, $3 FROM touched
		ON CONFLICT (upload_id, part_number) DO UPDATE SET
			etag = EXCLUDED.etag,
			recorded_at = CASE WHEN upload_parts.etag = EXCLUDED.etag
				THEN upload_parts.recorded_at ELSE now() END
		RETURNING part_number, etag, recorded_at`,
		uploadID, partNumber, etag,
	).Scan(&pt.PartNumber, &pt.ETag, &pt.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return pt, ErrConflict
	}
	return pt, err
}

func (p *Postgres) FinishUpload(ctx context.Context, uploadID string, asset *Asset, job *Job) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE uploads SET status = 'completed', updated_at = now()
			WHERE id = $1 AND status = 'uploading'`, uploadID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrConflict
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO assets (id, user_id, name, source_key, source_size, content_type, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING created_at, updated_at`,
			asset.ID, asset.UserID, asset.Name, asset.SourceKey, asset.SourceSize, asset.ContentType, asset.Status,
		).Scan(&asset.CreatedAt, &asset.UpdatedAt); err != nil {
			return err
		}
		return insertJob(ctx, tx, job)
	})
}

func (p *Postgres) AbortUpload(ctx context.Context, uploadID string) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE uploads SET status = 'aborted', updated_at = now()
		WHERE id = $1 AND status = 'uploading'`, uploadID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

func (p *Postgres) StaleUploads(ctx context.Context, cutoff time.Time, limit int) ([]Upload, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+uploadCols+` FROM uploads
		WHERE status = 'uploading' AND updated_at < $1
		ORDER BY updated_at LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Upload, error) {
		u, err := scanUpload(row)
		if err != nil {
			return Upload{}, err
		}
		return *u, nil
	})
}

// ---- assets ----

const assetCols = `id, user_id, name, source_key, source_size, content_type, status,
	duration_seconds, width, height, fps, frame_count, created_at, updated_at`

func scanAsset(row pgx.Row) (*Asset, error) {
	var a Asset
	err := row.Scan(&a.ID, &a.UserID, &a.Name, &a.SourceKey, &a.SourceSize, &a.ContentType, &a.Status,
		&a.DurationSeconds, &a.Width, &a.Height, &a.FPS, &a.FrameCount, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &a, err
}

// likeEscaper makes user search text match literally inside an ILIKE pattern.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (p *Postgres) ListAssets(ctx context.Context, userID string, f AssetFilter) ([]Asset, error) {
	// ORDER BY comes from a fixed whitelist, never from request text.
	order := "created_at DESC"
	switch f.Sort {
	case SortOldest:
		order = "created_at ASC"
	case SortName:
		order = "lower(name) ASC, created_at DESC"
	case SortDuration:
		order = "duration_seconds DESC NULLS LAST, created_at DESC"
	}
	rows, err := p.pool.Query(ctx, `
		SELECT `+assetCols+` FROM assets
		WHERE user_id = $1 AND ($2 = '' OR name ILIKE '%' || $2 || '%')
		ORDER BY `+order, userID, likeEscaper.Replace(f.Query))
	if err != nil {
		return nil, err
	}
	assets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Asset, error) {
		a, err := scanAsset(row)
		if err != nil {
			return Asset{}, err
		}
		return *a, nil
	})
	if err != nil || len(assets) == 0 {
		return assets, err
	}

	// Second query: the latest job per asset. DISTINCT ON keeps the first row
	// of each asset_id group in the given order, i.e. the newest job.
	ids := make([]string, len(assets))
	for i := range assets {
		ids[i] = assets[i].ID
	}
	jobRows, err := p.pool.Query(ctx, `
		SELECT DISTINCT ON (asset_id) `+jobCols+` FROM jobs
		WHERE asset_id = ANY($1::uuid[])
		ORDER BY asset_id, created_at DESC, id DESC`, ids)
	if err != nil {
		return nil, err
	}
	latest := map[string]*Job{}
	if _, err := pgx.CollectRows(jobRows, func(row pgx.CollectableRow) (struct{}, error) {
		j, err := scanJob(row)
		if err == nil {
			latest[j.AssetID] = j
		}
		return struct{}{}, err
	}); err != nil {
		return nil, err
	}
	for i := range assets {
		assets[i].Job = latest[assets[i].ID]
	}
	return assets, nil
}

func (p *Postgres) GetAsset(ctx context.Context, userID, id string) (*Asset, error) {
	a, err := scanAsset(p.pool.QueryRow(ctx,
		`SELECT `+assetCols+` FROM assets WHERE id = $1 AND user_id = $2`, id, userID))
	if err != nil {
		return nil, err
	}
	job, err := p.LatestJobForAsset(ctx, a.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	a.Job = job
	return a, nil
}

func (p *Postgres) DeleteAsset(ctx context.Context, userID, id string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		// jobs go with the asset via ON DELETE CASCADE.
		tag, err := tx.Exec(ctx, `DELETE FROM assets WHERE id = $1 AND user_id = $2`, id, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `DELETE FROM uploads WHERE asset_id = $1`, id)
		return err
	})
}

// ---- jobs ----

const jobCols = `id, asset_id, user_id, type, character_id, status, progress, attempt, max_attempts,
	error, retry_at, enqueued_at, created_at, updated_at, started_at, finished_at`

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.AssetID, &j.UserID, &j.Type, &j.CharacterID, &j.Status, &j.Progress,
		&j.Attempt, &j.MaxAttempts, &j.Error, &j.RetryAt, &j.EnqueuedAt, &j.CreatedAt, &j.UpdatedAt,
		&j.StartedAt, &j.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &j, err
}

func insertJob(ctx context.Context, tx pgx.Tx, j *Job) error {
	return tx.QueryRow(ctx, `
		INSERT INTO jobs (id, asset_id, user_id, type, character_id, status, attempt, max_attempts)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at, updated_at`,
		j.ID, j.AssetID, j.UserID, j.Type, j.CharacterID, j.Status, j.Attempt, j.MaxAttempts,
	).Scan(&j.CreatedAt, &j.UpdatedAt)
}

func (p *Postgres) CreateJob(ctx context.Context, job *Job) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE assets SET status = 'processing', updated_at = now()
			WHERE id = $1 AND user_id = $2`, job.AssetID, job.UserID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return insertJob(ctx, tx, job)
	})
}

func (p *Postgres) GetJob(ctx context.Context, userID, id string) (*Job, error) {
	return scanJob(p.pool.QueryRow(ctx,
		`SELECT `+jobCols+` FROM jobs WHERE id = $1 AND user_id = $2`, id, userID))
}

func (p *Postgres) LatestJobForAsset(ctx context.Context, assetID string) (*Job, error) {
	return scanJob(p.pool.QueryRow(ctx, `
		SELECT `+jobCols+` FROM jobs WHERE asset_id = $1
		ORDER BY created_at DESC, id DESC LIMIT 1`, assetID))
}

func (p *Postgres) MarkJobEnqueued(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE jobs SET enqueued_at = now() WHERE id = $1 AND enqueued_at IS NULL`, id)
	return err
}

func (p *Postgres) UnenqueuedJobs(ctx context.Context, cutoff time.Time, limit int) ([]Job, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+jobCols+` FROM jobs
		WHERE enqueued_at IS NULL AND status = 'queued' AND created_at < $1
		ORDER BY created_at LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Job, error) {
		j, err := scanJob(row)
		if err != nil {
			return Job{}, err
		}
		return *j, nil
	})
}

// ---- characters ----

func scanCharacter(row pgx.Row) (*Character, error) {
	var c Character
	err := row.Scan(&c.ID, &c.UserID, &c.Name, &c.ObjectKey, &c.Size, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (p *Postgres) CreateCharacter(ctx context.Context, c *Character) error {
	return p.pool.QueryRow(ctx, `
		INSERT INTO characters (id, user_id, name, object_key, size_bytes)
		VALUES ($1, $2, $3, $4, $5) RETURNING created_at`,
		c.ID, c.UserID, c.Name, c.ObjectKey, c.Size,
	).Scan(&c.CreatedAt)
}

func (p *Postgres) ListCharacters(ctx context.Context, userID string) ([]Character, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, user_id, name, object_key, size_bytes, created_at FROM characters
		WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Character, error) {
		c, err := scanCharacter(row)
		if err != nil {
			return Character{}, err
		}
		return *c, nil
	})
}

func (p *Postgres) GetCharacter(ctx context.Context, userID, id string) (*Character, error) {
	return scanCharacter(p.pool.QueryRow(ctx, `
		SELECT id, user_id, name, object_key, size_bytes, created_at FROM characters
		WHERE id = $1 AND user_id = $2`, id, userID))
}

func (p *Postgres) DeleteCharacter(ctx context.Context, userID, id string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM characters WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
