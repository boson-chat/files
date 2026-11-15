// Package files is the data access for the files table (files + quota
// ledger).
package files

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/boson-chat/files/internal/db"
	"github.com/boson-chat/files/metrics"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Statuses.
const (
	StatusPending = "pending"
	StatusReady   = "ready"
	StatusDeleted = "deleted"
	StatusExpired = "expired"
)

// Owner kinds.
const (
	KindAccount = "account"
	KindGuest   = "guest"
)

// ErrNotFound: no such row.
var ErrNotFound = errors.New("files: not found")

// File is one upload.
type File struct {
	ID          string `gorm:"primaryKey"`
	Owner       string
	OwnerKind   string
	IPKey       *string
	Name        string
	ContentType string
	Size        int64
	StorageKey  string
	Status      string
	Width       *int
	Height      *int
	CreatedAt   time.Time `gorm:"autoCreateTime:false"`
	ExpiresAt   time.Time
	DeletedAt   *time.Time
	DeletedBy   *string
	PurgedAt    *time.Time
}

func (File) TableName() string { return "files" }

// Entry is one upload counted in a quota window.
type Entry struct {
	Size      int64
	CreatedAt time.Time
}

// Key selects whose window: the owner column or the guest IP key.
type Key struct {
	IP    bool
	Value string
}

// Query filters the moderation list.
type Query struct {
	Owner string
	// Q matches the name (case-insensitive substring) or the exact id.
	Q string
	// Status: "" (live: ready), "all", or one status.
	Status string
	// Cursor: return rows strictly older than (CreatedAt, ID).
	CursorTime *time.Time
	CursorID   string
	Limit      int
	Now        time.Time
}

// Totals for the status endpoint.
type Totals struct {
	LiveFiles    int64 `json:"live_files"`
	LiveBytes    int64 `json:"live_bytes"`
	StoredBytes  int64 `json:"stored_bytes"`
	WindowFiles  int64 `json:"window_uploads"`
	WindowBytes  int64 `json:"window_bytes"`
	WindowOwners int64 `json:"window_users"`
	Pending      int64 `json:"pending"`
	Unpurged     int64 `json:"awaiting_purge"`
}

type FileRepositoryImpl interface {
	// Lock takes transaction-scoped advisory locks on the quota keys (in a
	// fixed order, so two uploads locking the same keys can't deadlock).
	// Must run inside db.WithTx.
	Lock(ctx context.Context, keys ...string) error
	// WindowEntries are the uploads of key created after since (any status),
	// oldest first.
	WindowEntries(ctx context.Context, key Key, since time.Time) ([]Entry, error)
	Create(ctx context.Context, f *File) error
	MarkReady(ctx context.Context, id string, width, height *int) error
	// Remove deletes the row (a failed upload's reservation).
	Remove(ctx context.Context, id string) error
	Get(ctx context.Context, id string) (*File, error)
	// ListLive are owner's ready, unexpired files, newest first.
	ListLive(ctx context.Context, owner string, now time.Time, limit int) ([]File, error)
	// MarkDeleted flips a ready file to deleted; false when it wasn't ready
	// (or doesn't belong to owner, when owner is set).
	MarkDeleted(ctx context.Context, id, owner, by string, now time.Time) (bool, error)
	MarkPurged(ctx context.Context, id string, now time.Time) error
	// ExpireDue flips ready files whose expires_at passed to expired.
	ExpireDue(ctx context.Context, now time.Time, limit int) (int64, error)
	// Unpurged are deleted/expired files whose body is still in storage.
	Unpurged(ctx context.Context, limit int) ([]File, error)
	// StalePending are reservations created before before.
	StalePending(ctx context.Context, before time.Time, limit int) ([]File, error)
	// Prune deletes purged rows created at or before before (out of the
	// quota window).
	Prune(ctx context.Context, before time.Time) (int64, error)
	Search(ctx context.Context, q Query) ([]File, error)
	Totals(ctx context.Context, now, windowStart time.Time) (Totals, error)
	// StoredBytes is the size of every body still in storage (pending,
	// ready, and deleted/expired not yet purged).
	StoredBytes(ctx context.Context) (int64, error)
}

type FileRepository struct{ db *db.DB }

func NewFileRepository(d *db.DB) FileRepositoryImpl { return &FileRepository{db: d} }

func record(ctx context.Context, start time.Time, op string, errp *error) {
	err := *errp
	res := metrics.Success
	if err != nil && !errors.Is(err, ErrNotFound) {
		res = metrics.Error
	}
	metrics.GetAppMetrics().DatabaseMetric(ctx, time.Since(start).Seconds(), "files", op, res)
}

func (r *FileRepository) Lock(ctx context.Context, keys ...string) (err error) {
	defer record(ctx, time.Now(), "lock", &err)
	ks := append([]string(nil), keys...)
	sort.Strings(ks)
	for i, k := range ks {
		if i > 0 && ks[i-1] == k {
			continue
		}
		if err = r.db.Conn(ctx).Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "files.quota:"+k).Error; err != nil {
			return err
		}
	}
	return nil
}

func (r *FileRepository) WindowEntries(ctx context.Context, key Key, since time.Time) (out []Entry, err error) {
	defer record(ctx, time.Now(), "window", &err)
	col := "owner"
	if key.IP {
		col = "ip_key"
	}
	err = r.db.Conn(ctx).Model(&File{}).Select("size, created_at").
		Where(col+" = ? AND created_at > ?", key.Value, since).
		Order("created_at ASC, id ASC").Scan(&out).Error
	return out, err
}

func (r *FileRepository) Create(ctx context.Context, f *File) (err error) {
	defer record(ctx, time.Now(), "insert", &err)
	return r.db.Conn(ctx).Create(f).Error
}

func (r *FileRepository) MarkReady(ctx context.Context, id string, width, height *int) (err error) {
	defer record(ctx, time.Now(), "update", &err)
	res := r.db.Conn(ctx).Model(&File{}).Where("id = ? AND status = ?", id, StatusPending).
		Updates(map[string]any{"status": StatusReady, "width": width, "height": height})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *FileRepository) Remove(ctx context.Context, id string) (err error) {
	defer record(ctx, time.Now(), "delete", &err)
	return r.db.Conn(ctx).Where("id = ?", id).Delete(&File{}).Error
}

func (r *FileRepository) Get(ctx context.Context, id string) (_ *File, err error) {
	defer record(ctx, time.Now(), "select", &err)
	var f File
	err = r.db.Conn(ctx).Where("id = ?", id).Take(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (r *FileRepository) ListLive(ctx context.Context, owner string, now time.Time, limit int) (out []File, err error) {
	defer record(ctx, time.Now(), "select", &err)
	err = r.db.Conn(ctx).Where("owner = ? AND status = ? AND expires_at > ?", owner, StatusReady, now).
		Order("created_at DESC, id DESC").Limit(limit).Find(&out).Error
	return out, err
}

func (r *FileRepository) MarkDeleted(ctx context.Context, id, owner, by string, now time.Time) (_ bool, err error) {
	defer record(ctx, time.Now(), "update", &err)
	q := r.db.Conn(ctx).Model(&File{}).Where("id = ? AND status = ?", id, StatusReady)
	if owner != "" {
		q = q.Where("owner = ?", owner)
	}
	res := q.Updates(map[string]any{"status": StatusDeleted, "deleted_at": now, "deleted_by": by})
	return res.RowsAffected > 0, res.Error
}

func (r *FileRepository) MarkPurged(ctx context.Context, id string, now time.Time) (err error) {
	defer record(ctx, time.Now(), "update", &err)
	return r.db.Conn(ctx).Model(&File{}).Where("id = ?", id).Update("purged_at", now).Error
}

func (r *FileRepository) ExpireDue(ctx context.Context, now time.Time, limit int) (_ int64, err error) {
	defer record(ctx, time.Now(), "update", &err)
	res := r.db.Conn(ctx).Exec(`UPDATE files SET status = ? WHERE id IN (
		SELECT id FROM files WHERE status = ? AND expires_at <= ? ORDER BY expires_at LIMIT ? FOR UPDATE SKIP LOCKED)`,
		StatusExpired, StatusReady, now, limit)
	return res.RowsAffected, res.Error
}

func (r *FileRepository) Unpurged(ctx context.Context, limit int) (out []File, err error) {
	defer record(ctx, time.Now(), "select", &err)
	err = r.db.Conn(ctx).Where("purged_at IS NULL AND status IN ?", []string{StatusDeleted, StatusExpired}).
		Order("created_at").Limit(limit).Find(&out).Error
	return out, err
}

func (r *FileRepository) StalePending(ctx context.Context, before time.Time, limit int) (out []File, err error) {
	defer record(ctx, time.Now(), "select", &err)
	err = r.db.Conn(ctx).Where("status = ? AND created_at < ?", StatusPending, before).
		Order("created_at").Limit(limit).Find(&out).Error
	return out, err
}

func (r *FileRepository) Prune(ctx context.Context, before time.Time) (_ int64, err error) {
	defer record(ctx, time.Now(), "delete", &err)
	res := r.db.Conn(ctx).Where("purged_at IS NOT NULL AND created_at <= ?", before).Delete(&File{})
	return res.RowsAffected, res.Error
}

func (r *FileRepository) Search(ctx context.Context, q Query) (out []File, err error) {
	defer record(ctx, time.Now(), "select", &err)
	tx := r.db.Conn(ctx).Model(&File{})
	switch q.Status {
	case "":
		tx = tx.Where("status = ? AND expires_at > ?", StatusReady, q.Now)
	case "all":
	default:
		tx = tx.Where("status = ?", q.Status)
	}
	if q.Owner != "" {
		tx = tx.Where("owner = ?", q.Owner)
	}
	if s := strings.TrimSpace(q.Q); s != "" {
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
		tx = tx.Where("(id = ? OR name ILIKE ?)", s, "%"+esc+"%")
	}
	if q.CursorTime != nil {
		tx = tx.Where("(created_at, id) < (?, ?)", *q.CursorTime, q.CursorID)
	}
	err = tx.Order(clause.OrderBy{Columns: []clause.OrderByColumn{
		{Column: clause.Column{Name: "created_at"}, Desc: true}, {Column: clause.Column{Name: "id"}, Desc: true},
	}}).Limit(q.Limit).Find(&out).Error
	return out, err
}

func (r *FileRepository) Totals(ctx context.Context, now, windowStart time.Time) (t Totals, err error) {
	defer record(ctx, time.Now(), "select", &err)
	err = r.db.Conn(ctx).Raw(`SELECT
		count(*) FILTER (WHERE status = 'ready' AND expires_at > @now) AS live_files,
		coalesce(sum(size) FILTER (WHERE status = 'ready' AND expires_at > @now), 0) AS live_bytes,
		coalesce(sum(size) FILTER (WHERE purged_at IS NULL), 0) AS stored_bytes,
		count(*) FILTER (WHERE created_at > @since) AS window_files,
		coalesce(sum(size) FILTER (WHERE created_at > @since), 0) AS window_bytes,
		count(DISTINCT owner) FILTER (WHERE created_at > @since) AS window_owners,
		count(*) FILTER (WHERE status = 'pending') AS pending,
		count(*) FILTER (WHERE purged_at IS NULL AND status IN ('deleted', 'expired')) AS unpurged
		FROM files`, map[string]any{"now": now, "since": windowStart}).Scan(&t).Error
	return t, err
}

func (r *FileRepository) StoredBytes(ctx context.Context) (n int64, err error) {
	defer record(ctx, time.Now(), "select", &err)
	err = r.db.Conn(ctx).Raw("SELECT coalesce(sum(size), 0) FROM files WHERE purged_at IS NULL").Scan(&n).Error
	return n, err
}
