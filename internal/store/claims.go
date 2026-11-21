package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/UseTrace-SI/trace-brain/internal/extract"
	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

// Claim origins reported with every fact.
const (
	OriginManual    = "manual"
	OriginExtracted = "extracted"
)

// Extractor families recorded with extracted claims and extraction state.
const (
	ExtractorRules  = "rules"
	ExtractorOllama = "ollama"
)

// ExtractorID identifies who derives claims. Rules claims are reconciled
// across rule versions; learned claims only against the same model's earlier
// claims, so running a second model never retracts the first one's claims.
type ExtractorID struct {
	Kind    string // ExtractorRules or ExtractorOllama
	Model   string // learned model name; empty for rules
	Version string // rules version, or model@digest
}

// RulesExtractor is the deterministic extractor's identity.
var RulesExtractor = ExtractorID{Kind: ExtractorRules, Version: extract.RulesVersion}

// LearnedExtractor is the identity of an installed local model at a digest.
func LearnedExtractor(model, digest string) ExtractorID {
	return ExtractorID{Kind: ExtractorOllama, Model: model, Version: model + "@" + digest}
}

// family keys extraction state: one row per memory and rules or model.
func (id ExtractorID) family() string {
	if id.Kind == ExtractorOllama {
		return id.Kind + ":" + id.Model
	}
	return id.Kind
}

func (id ExtractorID) valid() bool {
	return (id.Kind == ExtractorRules && id.Model == "" || id.Kind == ExtractorOllama && id.Model != "" && strings.HasPrefix(id.Version, id.Model+"@")) &&
		id.Version != "" && len(id.Version) <= 512
}

// claimsOf selects the claims of memory @memory derived by this extractor.
const claimsOfSQL = `f.evidence_id=@memory AND s.extractor=@kind AND (@model='' OR substr(s.version,1,length(@model)+1)=@model||'@')`

// ExtractionFactLimit keeps part of the shared 10,000-claim budget available
// for manual assertions; extraction stops adding claims at this total.
const ExtractionFactLimit = 8000

// extractionFactLimit is ExtractionFactLimit; tests lower it.
var extractionFactLimit = ExtractionFactLimit

// extractionSources lists the memory sources eligible for automatic
// extraction. Documents and Trace session headers are never extracted.
const extractionSources = "('trace-import','manual','mcp','agent-extract')"

const sqlMaxInt = "9223372036854775807"

// Supersession names the later claim whose valid-time start closes an
// extracted claim, and the evidence memory behind it.
type Supersession struct {
	FactID     string `json:"fact_id"`
	EvidenceID string `json:"evidence_id"`
	At         int64  `json:"at"`
	RecordedAt int64  `json:"recorded_at"`
}

// negationSQL maps a predicate column to its negation, or NULL.
func negationSQL(column string) string {
	var b strings.Builder
	b.WriteString("(CASE " + column)
	for _, pair := range extract.NegationPairs() {
		fmt.Fprintf(&b, " WHEN '%s' THEN '%s' WHEN '%s' THEN '%s'", pair[0], pair[1], pair[1], pair[0])
	}
	b.WriteString(" END)")
	return b.String()
}

// closureSQL is the valid-time end that later contradicting claims impose on
// an extracted claim f (joined to fact_sources as s), as known at the named
// parameter. Invalidation is derived from recorded claims at read time, so it
// is independent of import order, exact for historical --known-at queries,
// and follows retraction or deletion of the newer claim. Manual claims are
// never closed automatically.
//
// Each kind of contradiction is a separate ordered probe on its own index, so
// the cost per row is logarithmic rather than a scan of every claim about the
// subject: a different value of the same predicate (any claim when f is
// exclusive, otherwise only exclusive claims) and the negated predicate for
// the same value.
func closureSQL(known string) string {
	probe := func(conditions string) string {
		return "coalesce((SELECT y.valid_from FROM facts y WHERE " + conditions + " AND y.valid_from>f.valid_from AND y.recorded_at<=" + known +
			" AND (y.retracted_at IS NULL OR y.retracted_at>" + known + ") ORDER BY y.valid_from LIMIT 1)," + sqlMaxInt + ")"
	}
	sameKey := "y.scope=f.scope AND y.subject=f.subject AND y.predicate=f.predicate AND y.object<>f.object"
	negated := "y.scope=f.scope AND y.subject=f.subject AND y.predicate=" + negationSQL("f.predicate") + " AND y.object=f.object"
	return "(CASE WHEN s.fact_id IS NULL THEN NULL ELSE nullif(min(CASE WHEN f.is_exclusive=1 THEN " + probe(sameKey) +
		" ELSE " + probe(sameKey+" AND y.is_exclusive=1") + " END," + probe(negated) + ")," + sqlMaxInt + ") END)"
}

func effectiveEndSQL(validTo, closedAt string) string {
	return "nullif(min(coalesce(" + validTo + "," + sqlMaxInt + "),coalesce(" + closedAt + "," + sqlMaxInt + "))," + sqlMaxInt + ")"
}

type claimSchema struct{ captures, sources bool }

func readClaimSchema(ctx context.Context, db queryer) (claimSchema, bool, error) {
	var facts, captures, sources int
	err := db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE name='facts'),count(*) FILTER (WHERE name='fact_evidence'),count(*) FILTER (WHERE name='fact_sources')
 FROM sqlite_master WHERE type='table'`).Scan(&facts, &captures, &sources)
	return claimSchema{captures: captures == 1, sources: sources == 1}, facts == 1, err
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// claimSelect returns claim rows matching where, with derived closure and
// effective end. Pre-feature databases without optional tables stay readable.
func claimSelect(schema claimSchema, where string) string {
	captured, capturedJoin := "NULL,NULL", ""
	if schema.captures {
		captured, capturedJoin = "substr(e.content,1,65537),e.captured_at", " LEFT JOIN fact_evidence e ON e.fact_id=f.id"
	}
	sources, sourceJoin, closure := "NULL,NULL,NULL,NULL", "", "NULL"
	content := "substr(m.content,1,65537)"
	if schema.sources {
		sources = "s.extractor,s.version,s.quote,CASE WHEN s.fact_id IS NULL THEN NULL ELSE instr(m.content,s.quote)>0 END"
		sourceJoin = " LEFT JOIN fact_sources s ON s.fact_id=f.id"
		closure = closureSQL("@known")
		content = "CASE WHEN s.fact_id IS NULL THEN substr(m.content,1,65537) END"
	}
	inner := `SELECT f.id AS id,f.scope AS scope,f.subject AS subject,f.predicate AS predicate,f.object AS object,f.valid_from AS valid_from,
 f.valid_to AS valid_to,f.recorded_at AS recorded_at,f.retracted_at AS retracted_at,f.evidence_id AS evidence_id,f.evidence_hash AS evidence_hash,
 f.is_exclusive AS is_exclusive,` + content + ` AS content,` + captured + `,` + sources + `,COALESCE(m.source,'') AS evidence_source,COALESCE(m.session_id,'') AS session_id,
 COALESCE(m.turn_id,'') AS turn_id,` + closure + ` AS closed_at
 FROM facts f JOIN memories m ON m.id=f.evidence_id` + capturedJoin + sourceJoin + ` WHERE ` + where
	return `SELECT * FROM (SELECT *,` + effectiveEndSQL("valid_to", "closed_at") + ` AS effective_to FROM (` + inner + `))`
}

type claimRow struct {
	fact       Fact
	retracted  sql.NullInt64
	content    sql.NullString
	captured   sql.NullString
	capturedAt sql.NullInt64
	quoteFound sql.NullBool
	closedAt   sql.NullInt64
}

func scanClaim(rows *sql.Rows) (claimRow, error) {
	var row claimRow
	var assertedEnd, effectiveEnd sql.NullInt64
	var extractor, version, quote sql.NullString
	f := &row.fact
	err := rows.Scan(&f.ID, &f.Scope, &f.Subject, &f.Predicate, &f.Object, &f.ValidFrom, &assertedEnd, &f.RecordedAt, &row.retracted, &f.EvidenceID,
		&f.EvidenceHash, &f.Exclusive, &row.content, &row.captured, &row.capturedAt, &extractor, &version, &quote, &row.quoteFound,
		&f.EvidenceSource, &f.EvidenceSessionID, &f.EvidenceTurnID, &row.closedAt, &effectiveEnd)
	if err != nil {
		return row, err
	}
	if effectiveEnd.Valid {
		value := effectiveEnd.Int64
		f.ValidTo = &value
	}
	f.Origin = OriginManual
	if extractor.Valid {
		f.Origin, f.Extractor, f.ExtractorVersion, f.Quote = OriginExtracted, extractor.String, version.String, quote.String
	}
	if !row.closedAt.Valid || (assertedEnd.Valid && assertedEnd.Int64 <= row.closedAt.Int64) {
		row.closedAt = sql.NullInt64{}
	}
	return row, nil
}

// evaluate applies the evidence checks of the selected mode.
func (row *claimRow) evaluate(knownAt int64) {
	f := &row.fact
	// Never report a retraction recorded after the requested knowledge time.
	if row.retracted.Valid && row.retracted.Int64 <= knownAt {
		value := row.retracted.Int64
		f.RetractedAt = &value
	}
	if f.Origin == OriginExtracted {
		// Extracted claims cite a bounded quote, not a captured memory copy.
		f.EvidenceStale = !row.quoteFound.Valid || !row.quoteFound.Bool
		return
	}
	content := row.content.String
	f.EvidenceStale = len(content) > 65536 || f.EvidenceHash != factHash(content)
	f.EvidenceCaptured = row.captured.Valid && row.capturedAt.Valid && validFactCapture(row.captured.String, f.EvidenceHash, row.capturedAt.Int64, knownAt)
}

// attachSupersessions names the claim behind each derived closure, preferring
// a new value of the same predicate over an explicit negation at that instant.
func attachSupersessions(ctx context.Context, db queryer, rows []claimRow, knownAt int64) error {
	for i := range rows {
		if !rows[i].closedAt.Valid {
			continue
		}
		f := &rows[i].fact
		var s Supersession
		visible := " AND y.valid_from=@closed AND y.recorded_at<=@known AND (y.retracted_at IS NULL OR y.retracted_at>@known)"
		err := db.QueryRowContext(ctx, `SELECT id,evidence_id,valid_from,recorded_at FROM (
 SELECT 0 AS preference,y.id,y.evidence_id,y.valid_from,y.recorded_at FROM facts f JOIN facts y ON y.scope=f.scope AND y.subject=f.subject AND y.predicate=f.predicate
  WHERE f.id=@id AND y.object<>f.object AND (f.is_exclusive=1 OR y.is_exclusive=1)`+visible+`
 UNION ALL
 SELECT 1,y.id,y.evidence_id,y.valid_from,y.recorded_at FROM facts f JOIN facts y ON y.scope=f.scope AND y.subject=f.subject AND y.predicate=`+negationSQL("f.predicate")+` AND y.object=f.object
  WHERE f.id=@id`+visible+`) ORDER BY preference,recorded_at,id LIMIT 1`, sql.Named("id", f.ID), sql.Named("closed", rows[i].closedAt.Int64), sql.Named("known", knownAt)).
			Scan(&s.FactID, &s.EvidenceID, &s.At, &s.RecordedAt)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			f.SupersededBy = &s
		}
	}
	return nil
}

// TimelineQuery reads every claim about a subject recorded and not retracted
// at KnownAt, whatever its validity interval, so superseded values remain
// inspectable.
type TimelineQuery struct {
	Scope, Subject, Predicate string
	KnownAt                   int64
	Limit                     int
}

// FactTimeline returns claims ordered by valid-time start and predicate with
// their effective (possibly superseded) end. Stale claims are included and
// marked; the result is bounded by Limit (1–100).
func FactTimeline(ctx context.Context, db *sql.DB, input TimelineQuery) (FactResults, error) {
	result := FactResults{Facts: []Fact{}, EvidenceMode: "current"}
	if input.KnownAt < 0 || input.Limit < 1 || input.Limit > 100 {
		return result, fmt.Errorf("invalid fact timeline time or limit")
	}
	scope, err := factText(input.Scope, 1024)
	if err != nil {
		return result, err
	}
	subject, err := factText(input.Subject, 256)
	if err != nil {
		return result, err
	}
	schema, installed, err := readClaimSchema(ctx, db)
	if err != nil || !installed {
		return result, err
	}
	where := "f.scope=@scope AND f.subject=@subject AND f.recorded_at<=@known AND (f.retracted_at IS NULL OR f.retracted_at>@known)"
	args := []any{sql.Named("scope", scope), sql.Named("subject", subject), sql.Named("known", input.KnownAt)}
	if input.Predicate != "" {
		if !predicatePattern.MatchString(input.Predicate) {
			return result, fmt.Errorf("invalid fact predicate")
		}
		where += " AND f.predicate=@predicate"
		args = append(args, sql.Named("predicate", input.Predicate))
	}
	rows, err := db.QueryContext(ctx, claimSelect(schema, where)+" ORDER BY valid_from,predicate,recorded_at,id LIMIT @limit", append(args, sql.Named("limit", input.Limit+1))...)
	if err != nil {
		return result, err
	}
	collected, bytes := []claimRow{}, 0
	for rows.Next() {
		row, err := scanClaim(rows)
		if err != nil {
			rows.Close()
			return result, err
		}
		bytes += len(row.content.String)
		if len(collected) == input.Limit || bytes > 8*1024*1024 {
			result.Truncated = true
			break
		}
		row.evaluate(input.KnownAt)
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

// ExtractionMemory is one memory selected for claim extraction. Content is
// redacted; ValidFrom is the time the source statement was made.
type ExtractionMemory struct {
	ID, Scope, Source, SessionID, TurnID string
	Content                              string
	ValidFrom                            int64
	ContentHash                          string
	// Oversized marks content beyond the 64 KiB budget or with invalid UTF-8;
	// Content is then empty and no claim can be derived from it.
	Oversized bool
}

// Reference names the source entity cited by reference claims.
func (m ExtractionMemory) Reference() string {
	switch {
	case m.TurnID != "":
		return "turn:" + m.TurnID
	case m.Source == "agent-extract" && m.SessionID != "":
		return "session:" + m.SessionID
	}
	return "memory:" + m.ID
}

// TurnFormat reports whether Content uses the Trace turn layout.
func (m ExtractionMemory) TurnFormat() bool { return m.Source == "trace-import" && m.TurnID != "" }

func extractionHash(content string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte("trace-brain-extraction-v1\x00"+content)))
}

// quoteHash is the evidence hash of an extracted claim. It is domain-separated
// from captured-memory hashes so the two can never be confused.
func quoteHash(quote string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte("trace-brain-quote-v1\x00"+quote)))
}

// PendingExtraction selects memories never extracted by extractor at version,
// or changed since. Updates clear extraction state through a trigger.
func PendingExtraction(ctx context.Context, q queryer, id ExtractorID, limit, offset int) ([]ExtractionMemory, error) {
	rows, err := q.QueryContext(ctx, `SELECT m.id,m.scope,COALESCE(m.source,''),COALESCE(m.session_id,''),COALESCE(m.turn_id,''),substr(m.content,1,65537),
 COALESCE((SELECT t.source_at FROM memory_source_times t WHERE t.memory_id=m.id),(SELECT max(r.recorded_at) FROM memory_revisions r WHERE r.memory_id=m.id),m.created_at)
 FROM memories m LEFT JOIN fact_extraction_state s ON s.memory_id=m.id AND s.extractor=?
 WHERE m.source IN `+extractionSources+` AND NOT (m.source='trace-import' AND COALESCE(m.turn_id,'')='') AND (s.memory_id IS NULL OR s.version<>?)
 ORDER BY m.created_at,m.id LIMIT ? OFFSET ?`, id.family(), id.Version, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExtractionMemory{}
	r := redact.Redactor{}
	for rows.Next() {
		var m ExtractionMemory
		if err := rows.Scan(&m.ID, &m.Scope, &m.Source, &m.SessionID, &m.TurnID, &m.Content, &m.ValidFrom); err != nil {
			return nil, err
		}
		m.Content = r.Text(m.Content)
		m.ContentHash = extractionHash(m.Content)
		if len(m.Content) > maxMemoryContentBytes || !utf8.ValidString(m.Content) {
			m.Content, m.Oversized = "", true
		}
		m.ValidFrom = max(m.ValidFrom, 0)
		if m.Source == "trace-import" || m.Source == "agent-extract" {
			m.Scope = "project"
		} else if scope, err := factText(m.Scope, 1024); err == nil {
			m.Scope = scope
		} else {
			m.Scope = ""
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountPendingExtraction counts pending memories, capped at 100,001.
func CountPendingExtraction(ctx context.Context, q queryer, id ExtractorID) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM memories m LEFT JOIN fact_extraction_state s ON s.memory_id=m.id AND s.extractor=?
 WHERE m.source IN `+extractionSources+` AND NOT (m.source='trace-import' AND COALESCE(m.turn_id,'')='') AND (s.memory_id IS NULL OR s.version<>?) LIMIT 100001)`, id.family(), id.Version).Scan(&n)
	return n, err
}

// ExtractionReport summarizes extraction. Counts are claims unless named
// otherwise; Pending is the number of memories still awaiting extraction.
type ExtractionReport struct {
	Extractor       string `json:"extractor"`
	Version         string `json:"version"`
	Memories        int    `json:"memories"`
	Added           int    `json:"claims_added"`
	Retracted       int    `json:"claims_retracted"`
	Unchanged       int    `json:"claims_unchanged"`
	Conflicts       int    `json:"conflicts_skipped"`
	Suppressed      int    `json:"retracted_not_restored"`
	Rejected        int    `json:"claims_rejected"`
	Corroborated    int    `json:"claims_already_held"`
	Changed         int    `json:"memories_changed_during_extraction"`
	Skipped         int    `json:"memories_skipped"`
	Pending         int    `json:"pending"`
	BudgetExhausted bool   `json:"budget_exhausted"`
}

// Add accumulates another report into r.
func (r *ExtractionReport) Add(o ExtractionReport) {
	r.Memories += o.Memories
	r.Added += o.Added
	r.Retracted += o.Retracted
	r.Unchanged += o.Unchanged
	r.Conflicts += o.Conflicts
	r.Suppressed += o.Suppressed
	r.Rejected += o.Rejected
	r.Corroborated += o.Corroborated
	r.Changed += o.Changed
	r.Skipped += o.Skipped
	r.BudgetExhausted = r.BudgetExhausted || o.BudgetExhausted
}

type normalizedClaim struct {
	extract.Claim
	key string
}

// normalizeClaims redacts and validates candidates. Quotes must be exact,
// redaction-stable substrings of the memory content they cite.
func normalizeClaims(scope, content string, claims []extract.Claim) ([]normalizedClaim, int) {
	out, rejected := []normalizedClaim{}, 0
	seen := map[string]bool{}
	r := redact.Redactor{}
	for _, c := range claims {
		subject, subjectErr := factText(c.Subject, 256)
		object, objectErr := factText(c.Object, 1024)
		quote := strings.TrimSpace(c.Quote)
		if subjectErr != nil || objectErr != nil || !predicatePattern.MatchString(c.Predicate) || !extract.KnownPredicate(c.Predicate) ||
			quote == "" || len(quote) > extract.MaxQuoteBytes || !utf8.ValidString(quote) || strings.ContainsRune(quote, 0) ||
			r.Text(quote) != quote || !strings.Contains(content, quote) {
			rejected++
			continue
		}
		n := normalizedClaim{Claim: extract.Claim{Subject: subject, Predicate: c.Predicate, Object: object, Quote: quote, Exclusive: extract.Exclusive(subject, c.Predicate)}}
		n.key = claimKey(scope, subject, c.Predicate, object, quote)
		if !seen[n.key] {
			seen[n.key] = true
			out = append(out, n)
		}
	}
	return out, rejected
}

// RecordExtractionSkip marks a memory handled at the extractor's version when
// no claims could be derived (input over a learned extractor's bound, invalid
// or truncated model output). Like ApplyExtraction it first re-checks that the
// memory still holds the selected content, leaving a changed memory pending;
// claims of this extractor whose quote is no longer in the memory are
// retracted, and the others are kept.
func RecordExtractionSkip(ctx context.Context, tx *sql.Tx, memory ExtractionMemory, id ExtractorID, at int64) (ExtractionReport, error) {
	report := ExtractionReport{Extractor: id.Kind, Version: id.Version}
	if !id.valid() || at < 0 {
		return report, fmt.Errorf("invalid extractor identity or time")
	}
	unchanged, err := memoryUnchanged(ctx, tx, memory)
	if err != nil || !unchanged {
		report.Changed = 1
		return report, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE facts SET retracted_at=max(@at,recorded_at) WHERE retracted_at IS NULL AND id IN (SELECT f.id FROM facts f
 JOIN fact_sources s ON s.fact_id=f.id JOIN memories m ON m.id=f.evidence_id WHERE `+claimsOfSQL+` AND instr(m.content,s.quote)=0)`,
		sql.Named("at", at), sql.Named("memory", memory.ID), sql.Named("kind", id.Kind), sql.Named("model", id.Model))
	if err != nil {
		return report, err
	}
	retracted, err := result.RowsAffected()
	if err != nil {
		return report, err
	}
	report.Retracted, report.Skipped = int(retracted), 1
	return report, recordState(ctx, tx, memory, id, 0, at)
}

// memoryUnchanged re-reads a memory inside the write transaction.
func memoryUnchanged(ctx context.Context, tx *sql.Tx, memory ExtractionMemory) (bool, error) {
	var current string
	err := tx.QueryRowContext(ctx, "SELECT substr(content,1,65537) FROM memories WHERE id=?", memory.ID).Scan(&current)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil && extractionHash((redact.Redactor{}).Text(current)) == memory.ContentHash, err
}

func recordState(ctx context.Context, tx *sql.Tx, memory ExtractionMemory, id ExtractorID, claims int, at int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO fact_extraction_state(memory_id,extractor,version,content_hash,claims,extracted_at) VALUES(?,?,?,?,?,?)
 ON CONFLICT(memory_id,extractor) DO UPDATE SET version=excluded.version,content_hash=excluded.content_hash,claims=excluded.claims,extracted_at=excluded.extracted_at`,
		memory.ID, id.family(), id.Version, memory.ContentHash, claims, at)
	return err
}

// ApplyExtraction reconciles one memory's claims for an extractor inside tx.
// Identical claims are kept, claims no longer supported are retracted (never
// deleted), and a claim once retracted is not restored. New claims are added
// unless they would require closing a manual claim or tie with a
// contradicting claim at the same instant. The memory must still hold the
// extracted content; otherwise it is left pending.
func ApplyExtraction(ctx context.Context, tx *sql.Tx, memory ExtractionMemory, id ExtractorID, claims []extract.Claim, at int64) (ExtractionReport, error) {
	report := ExtractionReport{Extractor: id.Kind, Version: id.Version}
	if !id.valid() || at < 0 {
		return report, fmt.Errorf("invalid extractor identity or time")
	}
	unchanged, err := memoryUnchanged(ctx, tx, memory)
	if err != nil || !unchanged {
		report.Changed = 1
		return report, err
	}
	desired, rejected := normalizeClaims(memory.Scope, memory.Content, claims)
	report.Rejected = rejected
	if memory.Scope == "" {
		report.Rejected += len(desired)
		desired = nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT f.id,f.scope,f.subject,f.predicate,f.object,s.quote,f.retracted_at IS NOT NULL,f.recorded_at FROM facts f
 JOIN fact_sources s ON s.fact_id=f.id WHERE `+claimsOfSQL, sql.Named("memory", memory.ID), sql.Named("kind", id.Kind), sql.Named("model", id.Model))
	if err != nil {
		return report, err
	}
	type existing struct {
		id        string
		retracted bool
		recorded  int64
	}
	history := map[string][]existing{}
	for rows.Next() {
		var e existing
		var scope, subject, predicate, object, quote string
		if err := rows.Scan(&e.id, &scope, &subject, &predicate, &object, &quote, &e.retracted, &e.recorded); err != nil {
			rows.Close()
			return report, err
		}
		key := claimKey(scope, subject, predicate, object, quote)
		history[key] = append(history[key], e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return report, err
	}
	wanted := map[string]bool{}
	for _, c := range desired {
		wanted[c.key] = true
	}
	for key, versions := range history {
		for _, e := range versions {
			if e.retracted || wanted[key] {
				continue
			}
			if _, err := tx.ExecContext(ctx, "UPDATE facts SET retracted_at=max(?,recorded_at) WHERE id=? AND retracted_at IS NULL", at, e.id); err != nil {
				return report, err
			}
			report.Retracted++
		}
	}
	var total int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM facts").Scan(&total); err != nil {
		return report, err
	}
	accepted := 0
	for _, c := range desired {
		if versions := history[c.key]; len(versions) > 0 {
			active := false
			for _, e := range versions {
				active = active || !e.retracted
			}
			if active {
				report.Unchanged++
				accepted++
			} else {
				report.Suppressed++
			}
			continue
		}
		if total >= extractionFactLimit {
			report.BudgetExhausted = true
			continue
		}
		blocked, err := claimBlocked(ctx, tx, memory.Scope, c.Claim, memory.ValidFrom, at)
		if err != nil {
			return report, err
		}
		if blocked {
			report.Conflicts++
			continue
		}
		covered, err := claimCovered(ctx, tx, memory.Scope, c.Claim, memory.ValidFrom, at)
		if err != nil {
			return report, err
		}
		if covered {
			report.Corroborated++
			accepted++
			continue
		}
		factID, err := newFactID()
		if err != nil {
			return report, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO facts VALUES(?,?,?,?,?,?,NULL,?,NULL,?,?,?)", factID, memory.Scope, c.Subject, c.Predicate, c.Object, memory.ValidFrom, at, memory.ID, quoteHash(c.Quote), c.Exclusive); err != nil {
			return report, fmt.Errorf("adding extracted claim: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO fact_sources(fact_id,extractor,version,quote) VALUES(?,?,?,?)", factID, id.Kind, id.Version, c.Quote); err != nil {
			return report, fmt.Errorf("recording claim evidence: %w", err)
		}
		total++
		accepted++
		report.Added++
	}
	report.Memories = 1
	if report.BudgetExhausted {
		// Stay pending so the remaining claims are added once capacity exists.
		return report, nil
	}
	return report, recordState(ctx, tx, memory, id, accepted, at)
}

// claimBlocked reports whether adding c from valid time from would require
// closing an active manual claim, or contradicts another claim starting at the
// identical instant. A new claim may close older extracted claims (derived at
// read time) but never a manual one. Each probe uses its own index.
func claimBlocked(ctx context.Context, tx *sql.Tx, scope string, c extract.Claim, from, at int64) (bool, error) {
	overlapping := ` AND y.valid_from<=@from AND y.retracted_at IS NULL AND y.recorded_at<=@at
 AND ((ys.fact_id IS NULL AND (y.valid_to IS NULL OR y.valid_to>@from)) OR (ys.fact_id IS NOT NULL AND y.valid_from=@from))`
	sameKey := "y.scope=@scope AND y.subject=@subject AND y.predicate=@predicate AND y.object<>@object"
	if !c.Exclusive {
		sameKey += " AND y.is_exclusive=1"
	}
	var blocked bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM facts y LEFT JOIN fact_sources ys ON ys.fact_id=y.id WHERE `+sameKey+overlapping+`)
 OR EXISTS(SELECT 1 FROM facts y LEFT JOIN fact_sources ys ON ys.fact_id=y.id WHERE y.scope=@scope AND y.subject=@subject AND y.predicate=@negation AND y.object=@object`+overlapping+`)`,
		sql.Named("scope", scope), sql.Named("subject", c.Subject), sql.Named("predicate", c.Predicate), sql.Named("object", c.Object),
		sql.Named("negation", extract.Negation(c.Predicate)), sql.Named("from", from), sql.Named("at", at)).Scan(&blocked)
	return blocked, err
}

// maxCoveringClaims bounds identical extracted claims that hold one value at
// the same time. A value restated by further memories while already held by
// this many active claims is not recorded again: it changes no validity, and
// unbounded duplicates would make every closure probe skip them.
const maxCoveringClaims = 3

// claimCovered reports whether maxCoveringClaims extracted claims with the
// same scope, subject, predicate and value are already valid at from. Older
// identical claims are visited newest first and the scan stops at the first
// one no longer valid, since earlier runs of the value were closed before it.
func claimCovered(ctx context.Context, tx *sql.Tx, scope string, c extract.Claim, from, at int64) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+effectiveEndSQL("f.valid_to", closureSQL("@at"))+` FROM facts f JOIN fact_sources s ON s.fact_id=f.id
 WHERE f.scope=@scope AND f.subject=@subject AND f.predicate=@predicate AND f.object=@object AND f.valid_from<=@from
 AND f.retracted_at IS NULL AND f.recorded_at<=@at ORDER BY f.valid_from DESC LIMIT @limit`,
		sql.Named("scope", scope), sql.Named("subject", c.Subject), sql.Named("predicate", c.Predicate), sql.Named("object", c.Object),
		sql.Named("from", from), sql.Named("at", at), sql.Named("limit", 2*maxCoveringClaims))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	covering := 0
	for rows.Next() {
		var end sql.NullInt64
		if err := rows.Scan(&end); err != nil {
			return false, err
		}
		if end.Valid && end.Int64 <= from {
			break
		}
		if covering++; covering == maxCoveringClaims {
			return true, nil
		}
	}
	return false, rows.Err()
}

// claimKey identifies an extracted claim of one memory across re-extraction:
// the same statement in the same scope is kept rather than recorded again.
func claimKey(scope, subject, predicate, object, quote string) string {
	return strings.Join([]string{scope, subject, predicate, object, quote}, "\x00")
}

func newFactID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("fact-%x", random[:]), nil
}

// RulesOptions bounds one deterministic extraction pass.
type RulesOptions struct {
	Limit  int           // memories per pass, 1–10,000
	Budget time.Duration // no new batch starts after this much time
	// At, when nonzero, fixes the recorded time (tests). Otherwise each batch
	// records the time at which its write transaction began.
	At int64
}

// ExtractRules runs the deterministic extractor over pending memories in
// bounded IMMEDIATE batches. Re-running without source changes is a no-op.
func ExtractRules(ctx context.Context, db *sql.DB, options RulesOptions) (ExtractionReport, error) {
	report := ExtractionReport{Extractor: ExtractorRules, Version: extract.RulesVersion}
	if options.Limit < 1 || options.Limit > 10000 || options.At < 0 {
		return report, fmt.Errorf("extraction limit must be 1–10000")
	}
	start := time.Now()
	for report.Memories+report.Changed < options.Limit && !report.BudgetExhausted {
		if options.Budget > 0 && time.Since(start) > options.Budget {
			break
		}
		batch, err := rulesBatch(ctx, db, min(32, options.Limit-report.Memories-report.Changed), options.At)
		if err != nil {
			return report, err
		}
		if batch.Memories+batch.Changed == 0 {
			break
		}
		report.Add(batch)
	}
	pending, err := CountPendingExtraction(ctx, db, RulesExtractor)
	report.Pending = pending
	return report, err
}

func rulesBatch(ctx context.Context, db *sql.DB, limit int, fixed int64) (ExtractionReport, error) {
	report := ExtractionReport{}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	at := fixed
	if at == 0 {
		at = time.Now().UnixMilli()
	}
	memories, err := PendingExtraction(ctx, tx, RulesExtractor, limit, 0)
	if err != nil {
		return report, err
	}
	for _, memory := range memories {
		claims := extract.Rules(extract.Input{Text: memory.Content, Reference: memory.Reference(), TurnFormat: memory.TurnFormat()})
		result, err := ApplyExtraction(ctx, tx, memory, RulesExtractor, claims, at)
		if err != nil {
			return report, err
		}
		report.Add(result)
		if result.BudgetExhausted {
			break
		}
	}
	return report, tx.Commit()
}

// MaxContextFacts bounds the claims a context lookup may include.
const MaxContextFacts = 20

// ContextFacts returns up to limit current, non-stale `project` claims whose
// subject, object or quote contains one of the lowercase words (any claim
// when words is empty), most recently recorded first. With sourceDB, like the
// context memory list, only claims whose evidence is a manual or MCP note or
// was imported from that Trace database are returned. It never migrates or
// writes; databases without claims return none.
func ContextFacts(ctx context.Context, db queryer, words []string, limit int, now int64, sourceDB string) ([]Fact, bool, error) {
	if limit < 1 || limit > MaxContextFacts || now < 0 || len(words) > 16 {
		return nil, false, fmt.Errorf("context facts limit must be 1–%d", MaxContextFacts)
	}
	schema, installed, err := readClaimSchema(ctx, db)
	if err != nil || !installed {
		return []Fact{}, false, err
	}
	where := "f.scope='project' AND f.valid_from<=@at AND f.recorded_at<=@known AND (f.retracted_at IS NULL OR f.retracted_at>@known)"
	args := []any{sql.Named("at", now), sql.Named("known", now)}
	matches := []string{}
	for i, word := range words {
		name := fmt.Sprintf("w%d", i)
		match := "instr(lower(f.subject),@" + name + ")>0 OR instr(lower(f.object),@" + name + ")>0"
		if schema.sources {
			match += " OR instr(lower(COALESCE(s.quote,'')),@" + name + ")>0"
		}
		matches = append(matches, match)
		args = append(args, sql.Named(name, strings.ToLower(word)))
	}
	if len(matches) > 0 {
		where += " AND (" + strings.Join(matches, " OR ") + ")"
	}
	if sourceDB != "" {
		where += " AND (COALESCE(m.source,'') IN ('manual','mcp') OR EXISTS(SELECT 1 FROM memory_imports i WHERE i.memory_id=m.id AND i.source_db=@source))"
		args = append(args, sql.Named("source", sourceDB))
	}
	rows, err := db.QueryContext(ctx, claimSelect(schema, where)+" WHERE effective_to IS NULL OR effective_to>@at ORDER BY recorded_at DESC,id LIMIT 201", args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	facts, truncated, bytes := []Fact{}, false, 0
	for rows.Next() {
		row, err := scanClaim(rows)
		if err != nil {
			return nil, false, err
		}
		if bytes += len(row.content.String); bytes > 8*1024*1024 {
			return facts, true, nil
		}
		if row.evaluate(now); row.fact.EvidenceStale {
			continue
		}
		if len(facts) == limit {
			truncated = true
			break
		}
		facts = append(facts, row.fact)
	}
	return facts, truncated, rows.Err()
}
