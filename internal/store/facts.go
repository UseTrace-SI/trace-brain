package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

type Fact struct {
	ID               string `json:"id"`
	Scope            string `json:"scope"`
	Subject          string `json:"subject"`
	Predicate        string `json:"predicate"`
	Object           string `json:"object"`
	ValidFrom        int64  `json:"valid_from"`
	ValidTo          *int64 `json:"valid_to,omitempty"`
	RecordedAt       int64  `json:"recorded_at"`
	RetractedAt      *int64 `json:"retracted_at,omitempty"`
	EvidenceID       string `json:"evidence_id"`
	EvidenceHash     string `json:"evidence_hash"`
	Exclusive        bool   `json:"exclusive"`
	EvidenceStale    bool   `json:"evidence_stale"`
	EvidenceCaptured bool   `json:"evidence_captured"`
}
type FactQuery struct {
	Scope, Subject, Predicate, Object, Entity string
	At, KnownAt                               int64
	Limit                                     int
	IncludeStale                              bool
	EvidenceMode                              string
}
type FactResults struct {
	Facts                      []Fact `json:"facts"`
	StaleOmitted               int    `json:"stale_omitted"`
	Truncated                  bool   `json:"truncated"`
	SourceVerified             bool   `json:"source_verified"`
	EvidenceMode               string `json:"evidence_mode"`
	EvidenceUnavailableOmitted int    `json:"evidence_unavailable_omitted"`
}

var predicatePattern = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,63}$`)
var factIDPattern = regexp.MustCompile(`^fact-[a-f0-9]{32}$`)

func factText(text string, max int) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > max || !utf8.ValidString(text) || strings.ContainsFunc(text, unicode.IsControl) {
		return "", fmt.Errorf("invalid or oversized fact field")
	}
	text = (redact.Redactor{}).Text(text)
	if len(text) > max {
		return "", fmt.Errorf("redacted fact field exceeds budget")
	}
	return text, nil
}
func factHash(content string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte((redact.Redactor{}).Text(content))))
}

// Assertions are user-supplied claims, never truth certifications. Evidence is
// bound to the current memory and captured atomically. Later source edits become
// explicitly stale while the original redacted evidence remains inspectable.
func AssertFact(ctx context.Context, db *sql.DB, input Fact, at int64) (Fact, error) {
	var err error
	if input.Scope, err = factText(input.Scope, 1024); err != nil {
		return Fact{}, err
	}
	if input.Subject, err = factText(input.Subject, 256); err != nil {
		return Fact{}, err
	}
	if input.Object, err = factText(input.Object, 1024); err != nil {
		return Fact{}, err
	}
	if !predicatePattern.MatchString(input.Predicate) || (redact.Redactor{}).Text(input.Predicate) != input.Predicate || input.ValidFrom < 0 || (input.ValidTo != nil && *input.ValidTo <= input.ValidFrom) || at < 0 || input.EvidenceID == "" || len(input.EvidenceID) > 256 || (redact.Redactor{}).Text(input.EvidenceID) != input.EvidenceID {
		return Fact{}, fmt.Errorf("invalid fact predicate, interval or evidence")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Fact{}, err
	}
	defer tx.Rollback()
	var content string
	if err := tx.QueryRowContext(ctx, "SELECT substr(content,1,65537) FROM memories WHERE id=?", input.EvidenceID).Scan(&content); err != nil {
		return Fact{}, fmt.Errorf("fact evidence is unavailable: %w", err)
	}
	if len(content) > 65536 {
		return Fact{}, fmt.Errorf("fact evidence exceeds 64 KiB")
	}
	content = (redact.Redactor{}).Text(content)
	if len(content) > 65536 || !utf8.ValidString(content) {
		return Fact{}, fmt.Errorf("redacted fact evidence exceeds budget or is invalid UTF-8")
	}
	var conflicts int
	var end any
	if input.ValidTo != nil {
		end = *input.ValidTo
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM facts WHERE scope=? AND subject=? AND predicate=? AND object<>? AND retracted_at IS NULL
 AND (is_exclusive=1 OR ?=1) AND valid_from<coalesce(?,9223372036854775807) AND coalesce(valid_to,9223372036854775807)>?`, input.Scope, input.Subject, input.Predicate, input.Object, input.Exclusive, end, input.ValidFrom).Scan(&conflicts); err != nil {
		return Fact{}, err
	}
	if conflicts > 0 {
		return Fact{}, fmt.Errorf("conflicting active exclusive assertion; query and explicitly retract the prior claim first")
	}
	input.EvidenceHash = factHash(content)
	input.RetractedAt = nil
	input.EvidenceStale = false
	err = tx.QueryRowContext(ctx, `SELECT id,recorded_at FROM facts WHERE scope=? AND subject=? AND predicate=? AND object=? AND valid_from=? AND valid_to IS ?
 AND evidence_id=? AND evidence_hash=? AND is_exclusive=? AND retracted_at IS NULL LIMIT 1`, input.Scope, input.Subject, input.Predicate, input.Object, input.ValidFrom, end, input.EvidenceID, input.EvidenceHash, input.Exclusive).Scan(&input.ID, &input.RecordedAt)
	if err == nil {
		if input.RecordedAt > at {
			return Fact{}, fmt.Errorf("clock precedes existing assertion")
		}
		var capturedContent string
		var capturedAt int64
		err = tx.QueryRowContext(ctx, "SELECT substr(content,1,65537),captured_at FROM fact_evidence WHERE fact_id=?", input.ID).Scan(&capturedContent, &capturedAt)
		if err != nil && err != sql.ErrNoRows {
			return Fact{}, err
		}
		input.EvidenceCaptured = err == nil && validFactCapture(capturedContent, input.EvidenceHash, capturedAt, at)
		return input, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return Fact{}, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Fact{}, err
	}
	input.ID = fmt.Sprintf("fact-%x", random[:])
	input.RecordedAt = at
	input.RetractedAt = nil
	input.EvidenceHash = factHash(content)
	input.EvidenceStale = false
	if _, err := tx.ExecContext(ctx, "INSERT INTO facts VALUES(?,?,?,?,?,?,?,?,NULL,?,?,?)", input.ID, input.Scope, input.Subject, input.Predicate, input.Object, input.ValidFrom, end, at, input.EvidenceID, input.EvidenceHash, input.Exclusive); err != nil {
		return Fact{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO fact_evidence(fact_id,content,captured_at,reserved_bytes) VALUES(?,?,?,?)", input.ID, content, at, len(content)+1024); err != nil {
		if strings.Contains(err.Error(), "fact_evidence_budget_exceeded") {
			return Fact{}, fmt.Errorf("captured evidence capacity is full; forget an unused fact or its source memory: %w", err)
		}
		return Fact{}, fmt.Errorf("capturing fact evidence: %w", err)
	}
	input.EvidenceCaptured = true
	return input, tx.Commit()
}
func RetractFact(ctx context.Context, db *sql.DB, id string, at int64) error {
	if !factIDPattern.MatchString(id) || at < 0 {
		return fmt.Errorf("invalid fact ID or time")
	}
	result, err := db.ExecContext(ctx, "UPDATE facts SET retracted_at=coalesce(retracted_at,?) WHERE id=? AND recorded_at<=? AND (retracted_at IS NULL OR retracted_at<=?)", at, id, at, at)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("fact is unavailable at this recorded time")
	}
	return nil
}

func QueryFacts(ctx context.Context, db *sql.DB, input FactQuery) (FactResults, error) {
	result := FactResults{Facts: []Fact{}}
	mode, err := FactEvidenceMode(input.EvidenceMode)
	if err != nil {
		return result, err
	}
	result.EvidenceMode = mode
	if input.At < 0 || input.KnownAt < 0 || input.Limit < 1 || input.Limit > 100 {
		return result, fmt.Errorf("invalid fact query time or limit")
	}
	scope, err := factText(input.Scope, 1024)
	if err != nil {
		return result, err
	}
	installed, err := hasFactCaptures(ctx, db)
	if err != nil {
		return result, err
	}
	capturedColumns, capturedJoin := "NULL,NULL", ""
	if installed {
		capturedColumns = "substr(e.content,1,65537),e.captured_at"
		capturedJoin = " LEFT JOIN fact_evidence e ON e.fact_id=f.id"
	}
	query := `SELECT f.id,f.scope,f.subject,f.predicate,f.object,f.valid_from,f.valid_to,f.recorded_at,f.retracted_at,f.evidence_id,f.evidence_hash,f.is_exclusive,substr(m.content,1,65537),` + capturedColumns + `
 FROM facts f JOIN memories m ON m.id=f.evidence_id` + capturedJoin + ` WHERE f.scope=? AND f.valid_from<=? AND (f.valid_to IS NULL OR f.valid_to>?)
 AND f.recorded_at<=? AND (f.retracted_at IS NULL OR f.retracted_at>?)`
	args := []any{scope, input.At, input.At, input.KnownAt, input.KnownAt}
	for _, filter := range []struct {
		column, value string
		max           int
	}{{"subject", input.Subject, 256}, {"predicate", input.Predicate, 64}, {"object", input.Object, 1024}} {
		if filter.value != "" {
			value, err := factText(filter.value, filter.max)
			if err != nil {
				return result, err
			}
			query += " AND f." + filter.column + "=?"
			args = append(args, value)
		}
	}
	if input.Entity != "" {
		value, err := factText(input.Entity, 1024)
		if err != nil {
			return result, err
		}
		query += " AND (f.subject=? OR f.object=?)"
		args = append(args, value, value)
	}
	query += " ORDER BY f.recorded_at DESC,f.id LIMIT 1001"
	var claimsInstalled int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='facts'").Scan(&claimsInstalled); err != nil {
		return result, err
	}
	if claimsInstalled == 0 {
		return result, nil
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	candidates, bytes := 0, 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		candidates++
		if candidates > 1000 {
			result.Truncated = true
			break
		}
		var fact Fact
		var until, retracted sql.NullInt64
		var content string
		var captured sql.NullString
		var capturedAt sql.NullInt64
		if err := rows.Scan(&fact.ID, &fact.Scope, &fact.Subject, &fact.Predicate, &fact.Object, &fact.ValidFrom, &until, &fact.RecordedAt, &retracted, &fact.EvidenceID, &fact.EvidenceHash, &fact.Exclusive, &content, &captured, &capturedAt); err != nil {
			return result, err
		}
		bytes += len(content) + len(captured.String)
		if bytes > 8*1024*1024 {
			result.Truncated = true
			break
		}
		if until.Valid {
			value := until.Int64
			fact.ValidTo = &value
		}
		if retracted.Valid && retracted.Int64 <= input.KnownAt {
			value := retracted.Int64
			fact.RetractedAt = &value
		}
		fact.EvidenceStale = len(content) > 65536 || fact.EvidenceHash != factHash(content)
		fact.EvidenceCaptured = captured.Valid && capturedAt.Valid && validFactCapture(captured.String, fact.EvidenceHash, capturedAt.Int64, input.KnownAt)
		if mode == "captured" && !fact.EvidenceCaptured {
			result.EvidenceUnavailableOmitted++
			continue
		}
		if mode == "current" && fact.EvidenceStale && !input.IncludeStale {
			result.StaleOmitted++
			continue
		}
		if len(result.Facts) >= input.Limit {
			result.Truncated = true
			break
		}
		result.Facts = append(result.Facts, fact)
	}
	return result, rows.Err()
}
