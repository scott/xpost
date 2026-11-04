package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Image upload states (see migration 0004).
const (
	UpUploading = "uploading" // intent recorded; upload call in flight
	UpUploaded  = "uploaded"
	UpRetry     = "retry"
	UpFailed    = "failed"
	UpUnknown   = "unknown"
)

// MediaUpload is the state of one image of one part on X.
type MediaUpload struct {
	DraftID    int64
	Account    string
	Part       int // 1-based
	Slot       int // 1-based position in the part
	MediaID    int64
	IdemKey    string
	Status     string
	XMediaID   string
	ExpiresAt  time.Time
	Processing bool
	CheckAt    time.Time
	AltDone    bool
	Attempts   int
	Error      string
	At         time.Time
}

// Usable reports whether the upload can be attached to a post made at
// now: uploaded, processed, and not expiring within margin.
func (u *MediaUpload) Usable(now time.Time, margin time.Duration) bool {
	return u != nil && u.Status == UpUploaded && u.XMediaID != "" && !u.Processing && u.ExpiresAt.After(now.Add(margin))
}

const uploadCols = `draft_id, account, part, slot, media_id, idem_key, status, x_media_id, expires_at, processing, check_at, alt_done, attempts, error, at`

func scanUpload(r scanner) (*MediaUpload, error) {
	var u MediaUpload
	var exp, chk, at int64
	var proc, alt int
	if err := r.Scan(&u.DraftID, &u.Account, &u.Part, &u.Slot, &u.MediaID, &u.IdemKey, &u.Status, &u.XMediaID, &exp, &proc, &chk, &alt, &u.Attempts, &u.Error, &at); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Processing, u.AltDone, u.At = proc == 1, alt == 1, time.Unix(at, 0)
	if exp > 0 {
		u.ExpiresAt = time.Unix(exp, 0)
	}
	if chk > 0 {
		u.CheckAt = time.Unix(chk, 0)
	}
	return &u, nil
}

// DraftUploads returns a draft's image uploads by idempotency key.
func (s *Store) DraftUploads(ctx context.Context, draftID int64) (map[string]*MediaUpload, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+uploadCols+` FROM media_uploads WHERE draft_id = ? ORDER BY part, slot`, draftID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*MediaUpload{}
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out[u.IdemKey] = u
	}
	return out, rows.Err()
}

// Upload returns one upload by key.
func (s *Store) Upload(ctx context.Context, key string) (*MediaUpload, error) {
	return scanUpload(s.DB.QueryRowContext(ctx, `SELECT `+uploadCols+` FROM media_uploads WHERE idem_key = ?`, key))
}

// BeginUpload records the intent to upload one image and commits it, with
// a spend ledger row, before any call to X. It succeeds for a new key, one
// that definitely did not upload (retry, failed), one whose outcome is
// unknown, or an uploaded one that is no longer usable at now (expired):
// an upload posts nothing, so sending it again cannot double-post. A key
// already 'uploading' or still usable is refused with ErrConflict.
func (s *Store) BeginUpload(ctx context.Context, u MediaUpload, now time.Time, margin time.Duration) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := s.now()
	res, err := tx.ExecContext(ctx, `INSERT INTO media_uploads(draft_id, account, part, slot, media_id, idem_key, status, attempts, at)
		VALUES (?,?,?,?,?,?,'uploading',1,?)
		ON CONFLICT(idem_key) DO UPDATE SET status = 'uploading', media_id = excluded.media_id, x_media_id = '', expires_at = 0, processing = 0, check_at = 0, alt_done = 0,
			error = '', attempts = media_uploads.attempts + 1, at = excluded.at
		WHERE media_uploads.status IN ('retry','failed','unknown') OR (media_uploads.status = 'uploaded' AND (media_uploads.expires_at <= ? OR media_uploads.x_media_id = ''))`,
		u.DraftID, u.Account, u.Part, u.Slot, u.MediaID, u.IdemKey, at, now.Add(margin).Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO media_calls(at, idem_key, kind, status) VALUES (?,?,'upload','uploading')`, at, u.IdemKey); err != nil {
		return err
	}
	return tx.Commit()
}

// UploadOutcome is how one upload call ended.
type UploadOutcome struct {
	Status     string // UpUploaded, UpRetry, UpFailed or UpUnknown
	XMediaID   string
	ExpiresAt  time.Time
	Processing bool
	CheckAt    time.Time
	Error      string
}

// FinishUpload records the outcome of an upload that BeginUpload opened,
// and settles its ledger row.
func (s *Store) FinishUpload(ctx context.Context, key string, o UploadOutcome) error {
	switch o.Status {
	case UpUploaded, UpRetry, UpFailed, UpUnknown:
	default:
		return ErrBadInput
	}
	if o.Status == UpUploaded && (o.XMediaID == "" || o.ExpiresAt.IsZero()) {
		return ErrBadInput
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE media_uploads SET status = ?, x_media_id = ?, expires_at = ?, processing = ?, check_at = ?, error = ?, at = ?
		WHERE idem_key = ? AND status = 'uploading'`, o.Status, o.XMediaID, unix(o.ExpiresAt), b2i(o.Processing), unix(o.CheckAt), o.Error, s.now(), key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE media_calls SET status = ? WHERE id = (SELECT MAX(id) FROM media_calls WHERE idem_key = ? AND kind = 'upload') AND status = 'uploading'`, o.Status, key); err != nil {
		return err
	}
	return tx.Commit()
}

// UploadProcessed records a processing check on an uploaded image: still
// processing (check again at checkAt), done, or failed (errText set: X
// could not process it, so it can't be used).
func (s *Store) UploadProcessed(ctx context.Context, key string, processing bool, checkAt time.Time, errText string) error {
	status := UpUploaded
	if errText != "" {
		status, processing = UpFailed, false
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE media_uploads SET status = ?, processing = ?, check_at = ?, error = ? WHERE idem_key = ? AND status = 'uploaded'`,
		status, b2i(processing), unix(checkAt), errText, key)
	return err
}

// UploadAltDone records that the image's alt text is set on X.
func (s *Store) UploadAltDone(ctx context.Context, key string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE media_uploads SET alt_done = 1, error = '' WHERE idem_key = ? AND status = 'uploaded'`, key)
	return err
}

// UploadNote records why an uploaded image is waiting (its alt text call
// failed); the upload itself stays usable.
func (s *Store) UploadNote(ctx context.Context, key, errText string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE media_uploads SET error = ? WHERE idem_key = ? AND status = 'uploaded'`, errText, key)
	return err
}

// BeginAlt writes the ledger row for one alt text (media metadata) call
// before it is sent, so it counts toward the cap even if the run dies
// mid-call. It returns the row id for FinishAlt.
func (s *Store) BeginAlt(ctx context.Context, key string) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO media_calls(at, idem_key, kind, status) VALUES (?,?,'metadata','uploading')`, s.now(), key)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishAlt settles an alt text call's ledger row: UpUploaded (X set it),
// UpRetry (not sent), UpFailed (refused) or UpUnknown (may have been sent;
// counted as billed).
func (s *Store) FinishAlt(ctx context.Context, id int64, status string) error {
	switch status {
	case UpUploaded, UpRetry, UpFailed, UpUnknown:
	default:
		return ErrBadInput
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE media_calls SET status = ? WHERE id = ? AND kind = 'metadata' AND status = 'uploading'`, status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

// InterruptedUploads turns a draft's uploads left 'uploading' by a run that
// died mid-call into 'unknown' (counted as billed). They are uploaded again
// on the next run; nothing was posted, so no review is needed. Alt text
// calls left in flight become 'unknown' (billed) the same way.
func (s *Store) InterruptedUploads(ctx context.Context, draftID int64) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE media_calls SET status = 'unknown' WHERE status = 'uploading' AND kind = 'upload' AND idem_key IN (SELECT idem_key FROM media_uploads WHERE draft_id = ? AND status = 'uploading')`, draftID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE media_calls SET status = 'unknown' WHERE status = 'uploading' AND kind = 'metadata' AND idem_key IN (SELECT idem_key FROM media_uploads WHERE draft_id = ?)`, draftID); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE media_uploads SET status = 'unknown', error = 'Interrupted during the upload; it will be uploaded again.' WHERE draft_id = ? AND status = 'uploading'`, draftID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), tx.Commit()
}

// MediaData returns a stored image for upload to X.
func (s *Store) MediaData(ctx context.Context, id int64) (mime string, data []byte, err error) {
	if err := s.DB.QueryRowContext(ctx, `SELECT mime, data FROM media WHERE id = ?`, id).Scan(&mime, &data); err != nil {
		return "", nil, ErrNotFound
	}
	return mime, data, nil
}

// Ledger kinds.
const (
	CallUpload   = "upload"
	CallMetadata = "metadata"
)

// RecordMediaCall writes a finished ledger row directly (demo data).
func (s *Store) RecordMediaCall(ctx context.Context, at time.Time, key, kind, status string, example bool) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO media_calls(at, idem_key, kind, status, example) VALUES (?,?,?,?,?)`, at.Unix(), key, kind, status, b2i(example))
	return err
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
