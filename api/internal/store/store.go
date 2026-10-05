// Package store is the persistence layer. Store is implemented twice: Postgres
// for real use and Memory for unit tests. storetest runs one behavioural suite
// against both so the fake cannot drift from the real thing.
package store

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound is also returned when a row exists but belongs to another
	// user, so the api never reveals whether someone else's id is valid.
	ErrNotFound = errors.New("not found")
	// ErrConflict means the row is not in a state that allows the operation.
	ErrConflict = errors.New("conflict")
)

type UploadStatus string

const (
	UploadUploading UploadStatus = "uploading"
	UploadCompleted UploadStatus = "completed"
	UploadAborted   UploadStatus = "aborted"
)

// Upload tracks one multipart upload. AssetID is allocated when the upload is
// created so the source object can already live under assets/{assetId}/; the
// asset row itself only exists once the upload completes.
type Upload struct {
	ID          string
	UserID      string
	AssetID     string
	Filename    string
	ContentType string
	Size        int64
	PartSize    int64
	PartCount   int
	ObjectKey   string
	S3UploadID  string
	Status      UploadStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Part struct {
	PartNumber int
	ETag       string
	RecordedAt time.Time
}

type AssetStatus string

const (
	AssetProcessing AssetStatus = "processing"
	AssetReady      AssetStatus = "ready"
	AssetFailed     AssetStatus = "failed"
)

type Asset struct {
	ID          string
	UserID      string
	Name        string
	SourceKey   string
	SourceSize  int64
	ContentType string
	Status      AssetStatus
	// Filled in by the worker after ffprobe / processing.
	DurationSeconds *float64
	Width           *int
	Height          *int
	FPS             *float64
	FrameCount      *int
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// Job is the most recent job for the asset (nil only in a narrow window
	// that the complete transaction makes impossible in practice).
	Job *Job
}

// Job statuses double as pipeline stages; the worker moves a job through them
// in order. queued -> transcoding -> extracting -> writing -> uploading -> done,
// with failed reachable from any of them.
type JobStatus string

const (
	JobQueued      JobStatus = "queued"
	JobTranscoding JobStatus = "transcoding"
	JobExtracting  JobStatus = "extracting"
	JobWriting     JobStatus = "writing"
	JobUploading   JobStatus = "uploading"
	JobDone        JobStatus = "done"
	JobFailed      JobStatus = "failed"
)

const JobTypeProcessVideo = "process_video"

type Job struct {
	ID          string
	AssetID     string
	UserID      string
	Type        string
	CharacterID string // "default", "none" (skeleton only) or a character uuid
	Status      JobStatus
	Progress    int // 0-100 within the whole job
	Attempt     int // 1-based attempt currently running or last run
	MaxAttempts int
	Error       *string    // last failure, kept across retries so the UI can show it
	RetryAt     *time.Time // set while the job waits out a backoff
	EnqueuedAt  *time.Time // nil until the message is known to be on the stream
	CreatedAt   time.Time
	UpdatedAt   time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
}

type Character struct {
	ID        string
	UserID    string
	Name      string
	ObjectKey string
	Size      int64
	CreatedAt time.Time
}

type AssetSort string

const (
	SortNewest   AssetSort = "newest"
	SortOldest   AssetSort = "oldest"
	SortName     AssetSort = "name"
	SortDuration AssetSort = "duration"
)

type AssetFilter struct {
	Query string // case-insensitive substring of the name
	Sort  AssetSort
}

type Store interface {
	Ping(ctx context.Context) error

	CreateUpload(ctx context.Context, u *Upload) error
	GetUpload(ctx context.Context, userID, id string) (*Upload, error)
	ListParts(ctx context.Context, uploadID string) ([]Part, error)
	// RecordPart upserts a part's ETag and bumps the upload's updated_at (the
	// reaper's idle clock). Reporting the same ETag twice is a no-op that keeps
	// the original RecordedAt. Returns ErrConflict if the upload is not uploading.
	RecordPart(ctx context.Context, uploadID string, partNumber int, etag string) (Part, error)
	// FinishUpload atomically marks the upload completed and inserts the asset
	// and its first job. Returns ErrConflict if the upload is not uploading.
	FinishUpload(ctx context.Context, uploadID string, asset *Asset, job *Job) error
	// AbortUpload marks an uploading upload aborted. ErrConflict otherwise.
	AbortUpload(ctx context.Context, uploadID string) error
	// StaleUploads returns uploading uploads not touched since the cutoff.
	StaleUploads(ctx context.Context, cutoff time.Time, limit int) ([]Upload, error)

	ListAssets(ctx context.Context, userID string, f AssetFilter) ([]Asset, error)
	GetAsset(ctx context.Context, userID, id string) (*Asset, error)
	DeleteAsset(ctx context.Context, userID, id string) error

	// CreateJob adds a job to an existing asset (reprocess) and flips the asset
	// back to processing.
	CreateJob(ctx context.Context, job *Job) error
	GetJob(ctx context.Context, userID, id string) (*Job, error)
	// LatestJobForAsset is used to make "complete" idempotent.
	LatestJobForAsset(ctx context.Context, assetID string) (*Job, error)
	MarkJobEnqueued(ctx context.Context, id string) error
	// UnenqueuedJobs returns queued jobs created before the cutoff that never
	// made it onto the stream (the api died between commit and XADD).
	UnenqueuedJobs(ctx context.Context, cutoff time.Time, limit int) ([]Job, error)

	CreateCharacter(ctx context.Context, c *Character) error
	ListCharacters(ctx context.Context, userID string) ([]Character, error)
	GetCharacter(ctx context.Context, userID, id string) (*Character, error)
	DeleteCharacter(ctx context.Context, userID, id string) error
}
