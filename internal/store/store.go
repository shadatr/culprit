// Package store persists investigations and their event streams in SQLite.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/shadatr/culprit/internal/core"
)

// ErrNotFound is returned when an investigation does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrExists is returned by Create when the ID is already taken.
var ErrExists = errors.New("store: already exists")

// Investigation is one stored investigation (PLAN.md §4.8).
type Investigation struct {
	ID          string        `json:"id"`
	Incident    core.Incident `json:"incident"`
	Status      string        `json:"status"`
	Report      *core.Report  `json:"report,omitempty"`
	Error       string        `json:"error,omitempty"`
	Fingerprint string        `json:"fingerprint,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

// InvestigationSummary is the list view of an investigation. TopHypothesis and
// Confidence are set only once a report with at least one hypothesis exists.
type InvestigationSummary struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Service       string    `json:"service,omitempty"`
	Status        string    `json:"status"`
	TopHypothesis string    `json:"top_hypothesis,omitempty"`
	Confidence    *float64  `json:"confidence,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Store is a SQLite-backed investigation store. It is safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

//go:embed migrations/*.sql
var migrations embed.FS

// Open opens (creating if needed) the database at path and applies pending
// migrations. It enables WAL mode, a busy timeout and foreign keys.
func Open(path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("store: migrate: read version: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		v, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("store: migrate: bad migration name %s: %w", base, err)
		}
		if v <= current {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: migrate %s: %w", base, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migrate %s: %w", base, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, v); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migrate %s: %w", base, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migrate %s: %w", base, err)
		}
	}
	return nil
}

var idEncoding = base32.HexEncoding.WithPadding(base32.NoPadding)

// NewID returns a time-sortable random ID: "inv_" followed by the base32hex
// (lowercase) encoding of a 48-bit millisecond timestamp and 80 random bits.
func NewID() string {
	var b [16]byte
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixMilli()))
	copy(b[:6], ts[2:])
	_, _ = rand.Read(b[6:]) // crypto/rand.Read never fails
	return "inv_" + strings.ToLower(idEncoding.EncodeToString(b[:]))
}

// Create inserts inv. Empty ID, status and timestamps are filled in (ID via
// NewID, status queued, timestamps now); the incident's ID is set to the
// investigation ID. inv is updated in place.
func (s *Store) Create(ctx context.Context, inv *Investigation) error {
	if inv.ID == "" {
		inv.ID = NewID()
	}
	if inv.Status == "" {
		inv.Status = core.StatusQueued
	}
	now := s.now().UTC()
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = now
	}
	inv.UpdatedAt = inv.CreatedAt
	inv.Incident.ID = inv.ID
	incJSON, err := json.Marshal(inv.Incident)
	if err != nil {
		return fmt.Errorf("store: create: %w", err)
	}
	var repJSON any
	if inv.Report != nil {
		b, err := json.Marshal(inv.Report)
		if err != nil {
			return fmt.Errorf("store: create: %w", err)
		}
		repJSON = string(b)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO investigations (id, incident, status, report, error, fingerprint, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.ID, string(incJSON), inv.Status, repJSON, nullString(inv.Error), nullString(inv.Fingerprint),
		inv.CreatedAt.UnixNano(), inv.UpdatedAt.UnixNano())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("store: create %s: %w", inv.ID, ErrExists)
		}
		return fmt.Errorf("store: create: %w", err)
	}
	return nil
}

const investigationColumns = `id, incident, status, report, error, fingerprint, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanInvestigation(row scanner) (*Investigation, error) {
	var (
		inv                  Investigation
		incJSON              string
		repJSON, errS, fp    sql.NullString
		createdAt, updatedAt int64
	)
	if err := row.Scan(&inv.ID, &incJSON, &inv.Status, &repJSON, &errS, &fp, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(incJSON), &inv.Incident); err != nil {
		return nil, fmt.Errorf("decode incident: %w", err)
	}
	if repJSON.Valid {
		inv.Report = &core.Report{}
		if err := json.Unmarshal([]byte(repJSON.String), inv.Report); err != nil {
			return nil, fmt.Errorf("decode report: %w", err)
		}
	}
	inv.Error = errS.String
	inv.Fingerprint = fp.String
	inv.CreatedAt = time.Unix(0, createdAt).UTC()
	inv.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return &inv, nil
}

// Get returns the investigation with id, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (*Investigation, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+investigationColumns+` FROM investigations WHERE id = ?`, id)
	inv, err := scanInvestigation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: get %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get %s: %w", id, err)
	}
	return inv, nil
}

// List returns investigation summaries newest first. limit <= 0 means 50.
func (s *Store) List(ctx context.Context, limit, offset int) ([]InvestigationSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id,
		       COALESCE(json_extract(incident, '$.title'), ''),
		       COALESCE(json_extract(incident, '$.service'), ''),
		       status,
		       json_extract(report, '$.hypotheses[0].title'),
		       json_extract(report, '$.hypotheses[0].confidence'),
		       created_at
		FROM investigations
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list: %w", err)
	}
	defer rows.Close()
	out := []InvestigationSummary{}
	for rows.Next() {
		var (
			sum       InvestigationSummary
			top       sql.NullString
			conf      sql.NullFloat64
			createdAt int64
		)
		if err := rows.Scan(&sum.ID, &sum.Title, &sum.Service, &sum.Status, &top, &conf, &createdAt); err != nil {
			return nil, fmt.Errorf("store: list: %w", err)
		}
		if top.Valid {
			sum.TopHypothesis = top.String
			if conf.Valid {
				c := conf.Float64
				sum.Confidence = &c
			}
		}
		sum.CreatedAt = time.Unix(0, createdAt).UTC()
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list: %w", err)
	}
	return out, nil
}

func (s *Store) update(ctx context.Context, op, id, set string, args ...any) error {
	args = append([]any{s.now().UTC().UnixNano()}, args...)
	args = append(args, id)
	res, err := s.db.ExecContext(ctx, `UPDATE investigations SET updated_at = ?, `+set+` WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("store: %s %s: %w", op, id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: %s %s: %w", op, id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: %s %s: %w", op, id, ErrNotFound)
	}
	return nil
}

// UpdateStatus sets the status of id.
func (s *Store) UpdateStatus(ctx context.Context, id, status string) error {
	return s.update(ctx, "update status", id, `status = ?`, status)
}

// SaveReport stores r and sets the status from r.Status (completed if empty).
func (s *Store) SaveReport(ctx context.Context, id string, r *core.Report) error {
	if r == nil {
		return fmt.Errorf("store: save report %s: nil report", id)
	}
	status := r.Status
	if status == "" {
		status = core.StatusCompleted
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("store: save report %s: %w", id, err)
	}
	return s.update(ctx, "save report", id, `report = ?, status = ?`, string(b), status)
}

// SetError records msg on id. If the investigation has not reached a final
// status yet (queued or running), its status becomes failed; a status already
// set from a report is kept.
func (s *Store) SetError(ctx context.Context, id, msg string) error {
	return s.update(ctx, "set error", id,
		`error = ?, status = CASE WHEN status IN (?, ?) THEN ? ELSE status END`,
		msg, core.StatusQueued, core.StatusRunning, core.StatusFailed)
}

// MarkInterrupted fails every queued or running investigation with msg and
// returns their IDs. It is used at startup to clean up after a crash.
func (s *Store) MarkInterrupted(ctx context.Context, msg string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`UPDATE investigations SET status = ?, error = ?, updated_at = ?
		 WHERE status IN (?, ?) RETURNING id`,
		core.StatusFailed, msg, s.now().UTC().UnixNano(), core.StatusQueued, core.StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("store: mark interrupted: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: mark interrupted: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: mark interrupted: %w", err)
	}
	sort.Strings(ids)
	return ids, nil
}

// AppendEvent stores ev for investigation id with the next sequence number
// (starting at 1) and returns the stored event. ev.Seq is ignored; a zero
// ev.At is set to now and nil ev.Data to JSON null.
func (s *Store) AppendEvent(ctx context.Context, id string, ev core.Event) (core.Event, error) {
	if ev.At.IsZero() {
		ev.At = s.now()
	}
	ev.At = ev.At.UTC()
	if len(ev.Data) == 0 {
		ev.Data = json.RawMessage("null")
	}
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO events (investigation_id, seq, type, at, data)
		 SELECT ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ? FROM events WHERE investigation_id = ?
		 RETURNING seq`,
		id, ev.Type, ev.At.UnixNano(), string(ev.Data), id).Scan(&ev.Seq)
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return ev, fmt.Errorf("store: append event %s: %w", id, ErrNotFound)
		}
		return ev, fmt.Errorf("store: append event %s: %w", id, err)
	}
	return ev, nil
}

// Events returns the events of id with seq > afterSeq in order.
func (s *Store) Events(ctx context.Context, id string, afterSeq int) ([]core.Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, type, at, data FROM events WHERE investigation_id = ? AND seq > ? ORDER BY seq`,
		id, afterSeq)
	if err != nil {
		return nil, fmt.Errorf("store: events %s: %w", id, err)
	}
	defer rows.Close()
	out := []core.Event{}
	for rows.Next() {
		var (
			ev   core.Event
			at   int64
			data string
		)
		if err := rows.Scan(&ev.Seq, &ev.Type, &at, &data); err != nil {
			return nil, fmt.Errorf("store: events %s: %w", id, err)
		}
		ev.At = time.Unix(0, at).UTC()
		ev.Data = json.RawMessage(data)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: events %s: %w", id, err)
	}
	return out, nil
}

// FindRecentByFingerprint returns the newest investigation with fingerprint
// fp created at or after since, or ErrNotFound.
func (s *Store) FindRecentByFingerprint(ctx context.Context, fp string, since time.Time) (*Investigation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+investigationColumns+` FROM investigations
		 WHERE fingerprint = ? AND created_at >= ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, fp, since.UnixNano())
	inv, err := scanInvestigation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: find fingerprint %s: %w", fp, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: find fingerprint %s: %w", fp, err)
	}
	return inv, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
