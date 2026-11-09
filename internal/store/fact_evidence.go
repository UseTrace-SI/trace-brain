package store

import (
	"context"
	"database/sql"
	"fmt"
	"unicode/utf8"

	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

// Captured evidence proves which redacted bytes were bound to a claim, not
// that the claim is true or that its source has been independently verified.
type FactEvidence struct {
	FactID               string `json:"fact_id"`
	Scope                string `json:"scope"`
	EvidenceID           string `json:"evidence_id"`
	EvidenceHash         string `json:"evidence_hash"`
	Content              string `json:"content"`
	RecordedAt           int64  `json:"recorded_at"`
	CapturedAt           int64  `json:"captured_at"`
	RetractedAt          *int64 `json:"retracted_at,omitempty"`
	CurrentEvidenceStale bool   `json:"current_evidence_stale"`
	SourceVerified       bool   `json:"source_verified"`
}

func FactEvidenceMode(mode string) (string, error) {
	if mode == "" {
		return "current", nil
	}
	if mode != "current" && mode != "captured" {
		return "", fmt.Errorf("evidence mode must be current or captured")
	}
	return mode, nil
}
func FactEvidenceCheck(mode string) string {
	if mode == "captured" {
		return "assertion_capture_hash"
	}
	return "current_memory_hash"
}
func hasFactCaptures(ctx context.Context, db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='fact_evidence'").Scan(&count)
	return count == 1, err
}
func validFactCapture(content, hash string, capturedAt, knownAt int64) bool {
	return capturedAt >= 0 && capturedAt <= knownAt && len(content) <= 65536 && utf8.ValidString(content) &&
		(redact.Redactor{}).Text(content) == content && factHash(content) == hash
}

// Legacy facts are deliberately not backfilled from today's source content.
// Read-only older databases keep current-hash queries without a migration.
func GetFactEvidence(ctx context.Context, db *sql.DB, id, scope string, knownAt int64) (FactEvidence, error) {
	var result FactEvidence
	if !factIDPattern.MatchString(id) || knownAt < 0 {
		return result, fmt.Errorf("invalid fact ID or recorded time")
	}
	normalized, err := factText(scope, 1024)
	if err != nil {
		return result, err
	}
	installed, err := hasFactCaptures(ctx, db)
	if err != nil {
		return result, err
	}
	if !installed {
		return result, sql.ErrNoRows
	}
	var retracted sql.NullInt64
	var current string
	err = db.QueryRowContext(ctx, `SELECT f.id,f.scope,f.evidence_id,f.evidence_hash,f.recorded_at,f.retracted_at,
 substr(e.content,1,65537),e.captured_at,substr(m.content,1,65537)
 FROM facts f JOIN fact_evidence e ON e.fact_id=f.id JOIN memories m ON m.id=f.evidence_id
 WHERE f.id=? AND f.scope=? AND f.recorded_at<=? AND e.captured_at<=?`, id, normalized, knownAt, knownAt).
		Scan(&result.FactID, &result.Scope, &result.EvidenceID, &result.EvidenceHash, &result.RecordedAt, &retracted, &result.Content, &result.CapturedAt, &current)
	if err != nil {
		return FactEvidence{}, err
	}
	if !validFactCapture(result.Content, result.EvidenceHash, result.CapturedAt, knownAt) {
		return FactEvidence{}, fmt.Errorf("captured evidence failed its content hash or redaction check")
	}
	if retracted.Valid && retracted.Int64 <= knownAt {
		value := retracted.Int64
		result.RetractedAt = &value
	}
	result.CurrentEvidenceStale = len(current) > 65536 || factHash(current) != result.EvidenceHash
	return result, nil
}

// ForgetFact removes one claim and its captured copy while retaining its
// source memory. Retraction instead preserves the claim's recorded history.
func ForgetFact(ctx context.Context, db *sql.DB, id, scope string) error {
	if !factIDPattern.MatchString(id) {
		return fmt.Errorf("invalid fact ID")
	}
	normalized, err := factText(scope, 1024)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, "DELETE FROM facts WHERE id=? AND scope=?", id, normalized)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
