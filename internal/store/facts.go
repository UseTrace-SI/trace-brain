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

	"github.com/UseTrace-SI/trace-brain/internal/extract"
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
	// Origin is "manual" for asserted claims or "extracted" for claims derived
	// automatically; extracted claims cite a bounded quote of their evidence.
	Origin            string `json:"origin"`
	Extractor         string `json:"extractor,omitempty"`
	ExtractorVersion  string `json:"extractor_version,omitempty"`
	Quote             string `json:"quote,omitempty"`
	EvidenceSessionID string `json:"evidence_session_id,omitempty"`
	EvidenceTurnID    string `json:"evidence_turn_id,omitempty"`
	// SupersededBy names the later claim whose start closes ValidTo.
	SupersededBy *Supersession `json:"superseded_by,omitempty"`
	// Supersedes lists extracted claims this assertion closed (assert only).
	Supersedes []string `json:"supersedes,omitempty"`
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
	var end any
	if input.ValidTo != nil {
		end = *input.ValidTo
	}
	// Overlapping contradictions are rejected, except older extracted claims:
	// a newer manual assertion closes those (derived at read time) and the
	// closed claims are reported. Manual claims are never closed automatically.
	sameKey := "f.scope=@scope AND f.subject=@subject AND f.predicate=@predicate AND f.object<>@object"
	if !input.Exclusive {
		sameKey += " AND f.is_exclusive=1"
	}
	overlapping := ` AND f.retracted_at IS NULL AND f.valid_from<coalesce(@end,` + sqlMaxInt + `) AND coalesce(` + effectiveEndSQL("f.valid_to", closureSQL("@at")) + `,` + sqlMaxInt + `)>@from`
	rows, err := tx.QueryContext(ctx, `SELECT f.id,f.valid_from,s.fact_id IS NOT NULL FROM facts f LEFT JOIN fact_sources s ON s.fact_id=f.id WHERE `+sameKey+overlapping+`
 UNION ALL SELECT f.id,f.valid_from,1 FROM facts f JOIN fact_sources s ON s.fact_id=f.id
 WHERE f.scope=@scope AND f.subject=@subject AND f.predicate=@negation AND f.object=@object`+overlapping,
		sql.Named("scope", input.Scope), sql.Named("subject", input.Subject), sql.Named("predicate", input.Predicate), sql.Named("object", input.Object),
		sql.Named("negation", extract.Negation(input.Predicate)), sql.Named("end", end), sql.Named("from", input.ValidFrom), sql.Named("at", at))
	if err != nil {
		return Fact{}, err
	}
	supersedes, conflicting := []string{}, false
	for rows.Next() {
		var id string
		var from int64
		var extracted bool
		if err := rows.Scan(&id, &from, &extracted); err != nil {
			rows.Close()
			return Fact{}, err
		}
		if extracted && from < input.ValidFrom {
			supersedes = append(supersedes, id)
		} else {
			conflicting = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Fact{}, err
	}
	if conflicting {
		return Fact{}, fmt.Errorf("conflicting active exclusive assertion; query and explicitly retract the prior claim first")
	}
	input.EvidenceHash = factHash(content)
	input.RetractedAt = nil
	input.EvidenceStale = false
	input.Origin = OriginManual
	if len(supersedes) > 0 {
		input.Supersedes = supersedes
	}
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
	where := "f.scope=@scope AND f.valid_from<=@at AND f.recorded_at<=@known AND (f.retracted_at IS NULL OR f.retracted_at>@known)"
	args := []any{sql.Named("scope", scope), sql.Named("at", input.At), sql.Named("known", input.KnownAt)}
	for _, filter := range []struct {
		column, value string
		max           int
	}{{"subject", input.Subject, 256}, {"predicate", input.Predicate, 64}, {"object", input.Object, 1024}} {
		if filter.value != "" {
			value, err := factText(filter.value, filter.max)
			if err != nil {
				return result, err
			}
			where += " AND f." + filter.column + "=@" + filter.column
			args = append(args, sql.Named(filter.column, value))
		}
	}
	if input.Entity != "" {
		value, err := factText(input.Entity, 1024)
		if err != nil {
			return result, err
		}
		where += " AND (f.subject=@entity OR f.object=@entity)"
		args = append(args, sql.Named("entity", value))
	}
	schema, installed, err := readClaimSchema(ctx, db)
	if err != nil || !installed {
		return result, err
	}
	// Validity uses the effective end: the asserted end or, for extracted
	// claims, the start of a later contradicting claim known at KnownAt.
	query := claimSelect(schema, where) + " WHERE effective_to IS NULL OR effective_to>@at ORDER BY recorded_at DESC,id LIMIT 1001"
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return result, err
	}
	collected, candidates, bytes := []claimRow{}, 0, 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			rows.Close()
			return result, err
		}
		candidates++
		if candidates > 1000 {
			result.Truncated = true
			break
		}
		row, err := scanClaim(rows)
		if err != nil {
			rows.Close()
			return result, err
		}
		bytes += len(row.content.String) + len(row.captured.String)
		if bytes > 8*1024*1024 {
			result.Truncated = true
			break
		}
		row.evaluate(input.KnownAt)
		if mode == "captured" && !row.fact.EvidenceCaptured {
			result.EvidenceUnavailableOmitted++
			continue
		}
		if mode == "current" && row.fact.EvidenceStale && !input.IncludeStale {
			result.StaleOmitted++
			continue
		}
		if len(collected) >= input.Limit {
			result.Truncated = true
			break
		}
		collected = append(collected, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	if err := attachSupersessions(ctx, db, collected, input.KnownAt); err != nil {
		return result, err
	}
	for _, row := range collected {
		result.Facts = append(result.Facts, row.fact)
	}
	return result, nil
}
