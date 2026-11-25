package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

const (
	// DefaultSearchLimit and MaxSearchLimit bound every keyword read.
	DefaultSearchLimit = 20
	MaxSearchLimit     = 50
	// DefaultSnippetChars matches the historical substr(content,1,2048) cap.
	DefaultSnippetChars = 2048
	// MaxQueryBytes and maxQueryTerms bound the literal query.
	MaxQueryBytes = 4096
	maxQueryTerms = 16
	// searchOverfetch widens the BM25 candidate window so recency, the manual
	// boost and near-duplicate removal can reorder it without starving limit.
	searchOverfetch = 5
	// dedupTailChars is the trailing content compared, besides the snippet,
	// when collapsing near-duplicates of long memories.
	dedupTailChars = 256
)

// Ranking weights. Relevance dominates; recency decays with a 30-day
// half-life; user-authored (manual/MCP) notes get a fixed boost.
const (
	relevanceWeight = 0.75
	recencyWeight   = 0.15
	manualWeight    = 0.10
	recencyHalfLife = 30 * 24 * time.Hour
)

// SearchOpts selects memories for keyword retrieval. Query text is always
// treated as literal words; FTS operators and punctuation are never parsed.
type SearchOpts struct {
	Query string
	// Scope, when set, is an exact scope filter.
	Scope string
	// SourceDB, when set, limits results to manual/MCP notes and memories
	// imported from that canonical Trace database path.
	SourceDB string
	// Since, when positive, excludes memories created before this Unix ms.
	Since int64
	// Limit is clamped to 1–MaxSearchLimit (0 selects DefaultSearchLimit).
	Limit int
	// SnippetChars caps returned content (0 selects DefaultSnippetChars).
	SnippetChars int
	// RecentWhenEmpty lists the most recent memories for a blank query;
	// otherwise a query without words returns nothing.
	RecentWhenEmpty bool
	// Now is the recency reference (zero means time.Now()).
	Now time.Time
}

// SearchResult is one redacted memory with provenance.
type SearchResult struct {
	ID               string
	Scope            string
	Content          string
	Source           string
	SessionID        string
	TurnID           string
	CreatedAt        int64
	Score            float64
	ContentTruncated bool
}

// stopwords are dropped from queries: they match nearly every memory, so an
// any-word search for "the database" would otherwise return "the cat sat".
// Negations (no, not, never) are kept because they carry meaning.
var stopwords = map[string]bool{}

func init() {
	// Closed-class function words only; words that can carry meaning in a
	// developer note (down, up, out, over, only, more, ...) are kept.
	for _, word := range strings.Fields(`a about after all also am an and any are as at be because been before being
between both but by can could did do does doing during each for from had has have having he her here hers herself
him himself his how i if in into is it its itself just me my myself of on or our ours ourselves she should so some
such than that the their theirs them themselves then there these they this those through to until was we were what
when where which while who whom why will with would you your yours yourself yourselves`) {
		stopwords[word] = true
	}
}

// QueryTerms splits text into at most 16 distinct literal words (letters,
// numbers and underscores), dropping stopwords unless the query has nothing
// else. Every other character separates words, so FTS syntax such as quotes,
// operators, column filters and prefixes cannot be injected.
func QueryTerms(text string) ([]string, error) {
	if len(text) > MaxQueryBytes {
		return nil, fmt.Errorf("query exceeds %d bytes", MaxQueryBytes)
	}
	words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' })
	terms, common := []string{}, []string{}
	seen := map[string]bool{}
	for _, word := range words {
		folded := strings.ToLower(word)
		if seen[folded] {
			continue
		}
		seen[folded] = true
		if stopwords[folded] {
			common = append(common, word)
		} else {
			terms = append(terms, word)
		}
	}
	if len(terms) == 0 {
		terms = common
	}
	if len(terms) > maxQueryTerms {
		terms = terms[:maxQueryTerms]
	}
	return terms, nil
}

func quoteTerms(terms []string) []string {
	quoted := make([]string, len(terms))
	for i, term := range terms {
		// Terms never contain quotes (see QueryTerms); quoting makes FTS
		// keywords such as AND, OR, NOT and NEAR literal.
		quoted[i] = `"` + term + `"`
	}
	return quoted
}

func anyTermQuery(terms []string) string {
	if len(terms) == 0 {
		return `""`
	}
	return strings.Join(quoteTerms(terms), " OR ")
}

// Search is the single keyword retrieval path for CLI, MCP and context reads.
// Memories containing every query word rank above partial matches; within a tier
// it blends BM25 relevance (normalized over an overfetched candidate window)
// with recency and a manual-note boost, removes near-duplicates and applies read-time
// redaction to the returned scope and content.
func Search(ctx context.Context, db *sql.DB, opts SearchOpts) ([]SearchResult, error) {
	terms, err := QueryTerms(opts.Query)
	if err != nil {
		return nil, err
	}
	if opts.Limit <= 0 {
		opts.Limit = DefaultSearchLimit
	}
	opts.Limit = min(opts.Limit, MaxSearchLimit)
	if opts.SnippetChars <= 0 {
		opts.SnippetChars = DefaultSnippetChars
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if len(terms) == 0 && (!opts.RecentWhenEmpty || strings.TrimSpace(opts.Query) != "") {
		return []SearchResult{}, nil
	}

	filter, params := "", []any{}
	if opts.Scope != "" {
		filter += " AND m.scope=?"
		params = append(params, opts.Scope)
	}
	if opts.Since > 0 {
		filter += " AND m.created_at>=?"
		params = append(params, opts.Since)
	}
	if opts.SourceDB != "" {
		filter += " AND (m.source IN ('manual','mcp') OR EXISTS(SELECT 1 FROM memory_imports i WHERE i.memory_id=m.id AND i.source_db=?))"
		params = append(params, opts.SourceDB)
	}
	fetch := opts.Limit * searchOverfetch
	var found []searchCandidate
	if len(terms) == 0 {
		found, err = searchCandidates(ctx, db, "", filter, params, nil, fetch, 0, opts.SnippetChars)
	} else {
		// Memories containing every word form the first tier. Only when that
		// tier cannot fill the window are any-word matches added, excluding
		// the rows already selected, so the cost stays near one OR query.
		found, err = searchCandidates(ctx, db, strings.Join(quoteTerms(terms), " "), filter, params, nil, fetch, 1, opts.SnippetChars)
		if err == nil && len(terms) > 1 && len(found) < fetch {
			exclude := make([]any, len(found))
			for i, c := range found {
				exclude[i] = c.rowid
			}
			var partial []searchCandidate
			partial, err = searchCandidates(ctx, db, anyTermQuery(terms), filter, params, exclude, fetch-len(found), 0, opts.SnippetChars)
			found = append(found, partial...)
		}
	}
	if err != nil {
		return nil, err
	}
	best := 0.0
	for _, c := range found {
		best = math.Max(best, c.relevance)
	}
	now := opts.Now.UnixMilli()
	for i := range found {
		c := &found[i]
		relevance := 1.0
		if len(terms) > 0 {
			relevance = 0
			if best > 0 {
				relevance = c.relevance / best
			}
		}
		age := float64(max(0, now-c.CreatedAt))
		recency := math.Exp(-math.Ln2 * age / float64(recencyHalfLife.Milliseconds()))
		manual := 0.0
		if c.Source == "manual" || c.Source == "mcp" {
			manual = 1
		}
		c.Score = relevanceWeight*relevance + recencyWeight*recency + manualWeight*manual
	}
	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if len(terms) == 0 {
			// Recent listings keep strict recency order.
			if a.CreatedAt != b.CreatedAt {
				return a.CreatedAt > b.CreatedAt
			}
			return a.ID < b.ID
		}
		if a.allTerms != b.allTerms {
			return a.allTerms > b.allTerms
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt > b.CreatedAt
		}
		return a.ID < b.ID
	})
	results := []SearchResult{}
	seen := map[[32]byte]bool{}
	r := redact.Redactor{}
	for _, c := range found {
		key := dedupKey(c.Scope, c.Content, c.tail, c.ContentTruncated)
		if seen[key] {
			continue
		}
		seen[key] = true
		c.Scope, c.Content = r.Text(c.Scope), r.Text(c.Content)
		results = append(results, c.SearchResult)
		if len(results) == opts.Limit {
			break
		}
	}
	return results, ctx.Err()
}

type searchCandidate struct {
	SearchResult
	rowid     int64
	relevance float64 // -bm25(), larger is more relevant
	allTerms  int     // 1 when every query word matched
	tail      string
}

// searchCandidates selects up to limit memories matching the FTS expression
// match (or the most recent ones when match is empty). Candidates are ranked
// on index columns first; content is read only for the selected rows.
func searchCandidates(ctx context.Context, db *sql.DB, match, filter string, params, exclude []any, limit, allTerms, snippetChars int) ([]searchCandidate, error) {
	var candidates string
	var args []any
	if len(exclude) > 0 {
		filter += " AND m.rowid NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(exclude)), ",") + ")"
		params = append(append([]any{}, params...), exclude...)
	}
	if match == "" {
		candidates = `SELECT m.rowid AS rid, 0.0 AS rank FROM memories m WHERE 1=1` + filter + ` ORDER BY m.created_at DESC, m.id LIMIT ?`
		args = append(append(args, params...), limit)
	} else {
		candidates = `SELECT m.rowid AS rid, bm25(memory_fts) AS rank FROM memory_fts JOIN memories m ON m.rowid=memory_fts.rowid
			WHERE memory_fts MATCH ?` + filter + ` ORDER BY rank, m.created_at DESC, m.id LIMIT ?`
		args = append(append(append(args, match), params...), limit)
	}
	query := `WITH c AS (` + candidates + `)
		SELECT c.rid, c.rank, m.id, m.scope, substr(m.content,1,?), length(m.content)>?, substr(m.content,-?),
		COALESCE(m.source,''), COALESCE(m.session_id,''), COALESCE(m.turn_id,''), m.created_at
		FROM c JOIN memories m ON m.rowid=c.rid`
	args = append(args, snippetChars, snippetChars, dedupTailChars)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := []searchCandidate{}
	for rows.Next() {
		c := searchCandidate{allTerms: allTerms}
		var rank float64
		if err := rows.Scan(&c.rowid, &rank, &c.ID, &c.Scope, &c.Content, &c.ContentTruncated, &c.tail, &c.Source, &c.SessionID, &c.TurnID, &c.CreatedAt); err != nil {
			return nil, err
		}
		// SQLite's bm25() is negative; larger magnitude is more relevant.
		c.relevance = math.Max(0, -rank)
		found = append(found, c)
	}
	return found, rows.Err()
}

// dedupKey identifies near-duplicate memories: the same scope and the same
// text ignoring letter case and runs of whitespace, in the snippet and (for
// longer memories) the trailing text. Punctuation and symbols are kept, so
// "x > y" and "x < y", "-1" and "1" or "C++" and "C" stay distinct.
func dedupKey(scope, snippet, tail string, truncated bool) [32]byte {
	text := scope + "\x00" + normalizeForDedup(snippet)
	if truncated {
		text += "\x00" + normalizeForDedup(tail)
	}
	return sha256.Sum256([]byte(text))
}

func normalizeForDedup(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}
