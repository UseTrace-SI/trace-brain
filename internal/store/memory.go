package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

const (
	maxMemoryContentBytes = 65536
	maxMemoryScopeBytes   = 1024
)

// redactMemoryText enforces the size budget after redaction. Redaction markers
// expand the serialized bytes, so a raw payload that fits can overflow once a
// secret is replaced. Oversized redacted text would be silently truncated by
// Get/History (substr) and rejected downstream by fact evidence, embedding and
// semantic cache checks. This mirrors factText in facts.go.
func redactMemoryText(content, scope string) (string, string, error) {
	if strings.TrimSpace(content) == "" || len(content) > maxMemoryContentBytes || len(scope) > maxMemoryScopeBytes {
		return "", "", fmt.Errorf("memory must contain 1–65536 bytes and scope at most 1024 bytes")
	}
	r := redact.Redactor{}
	content, scope = r.Text(content), r.Text(scope)
	if len(content) > maxMemoryContentBytes || len(scope) > maxMemoryScopeBytes || !utf8.ValidString(content) || !utf8.ValidString(scope) {
		return "", "", fmt.Errorf("redacted memory exceeds the 64 KiB content or 1 KiB scope budget")
	}
	return content, scope, nil
}

// RedactImportedContent applies the same content policy to memories assembled
// outside Remember (session/turn and agent-extraction imports). Those paths
// build their own INSERT and would otherwise bypass the size budget that
// reads (substr), embeddings, fact evidence and semantic caching rely on.
func RedactImportedContent(content string) (string, error) {
	if strings.TrimSpace(content) == "" || len(content) > maxMemoryContentBytes {
		return "", fmt.Errorf("imported memory must contain 1–65536 bytes")
	}
	content = (redact.Redactor{}).Text(content)
	if len(content) > maxMemoryContentBytes || !utf8.ValidString(content) {
		return "", fmt.Errorf("redacted imported memory exceeds the 64 KiB content budget")
	}
	return content, nil
}

// importOmissionReserve bounds the omission notice inserted by
// BoundImportedContent; the notice itself is always shorter.
const importOmissionReserve = 128

// importRawWindow bounds the raw text redacted from each end of an oversized
// import. It is far larger than the parts kept, so any secret overlapping the
// kept text lies wholly inside a window and is redacted before the final cut.
const importRawWindow = 256 << 10

// BoundImportedContent prepares Trace history text for a derived memory. Trace
// appends every assistant response, thought and output to a turn without a
// size limit, so rejecting an oversized turn (or one with invalid UTF-8) would
// fail every later import of the same history. Instead the complete text is
// redacted first, so a cut can never split a secret into an unrecognised
// fragment, and an oversized result keeps its beginning (the prompt) and its
// end (the latest outcome) around an explicit omission notice. Cuts never fall
// inside a redaction marker or a UTF-8 sequence, and the stored text remains a
// redaction fixed point.
func BoundImportedContent(content string) (string, error) {
	content = strings.ToValidUTF8(content, "�")
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("imported memory must not be empty")
	}
	// Redaction is linear but still runs on every refresh to fingerprint each
	// turn, so only bounded windows of a very large turn are redacted.
	omitted := 0
	if len(content) > 2*importRawWindow {
		head := cutAtLine(content, importRawWindow, false)
		tail := cutAtLine(content, len(content)-importRawWindow, true)
		omitted = tail - head
		content = content[:head] + "\n" + content[tail:]
	}
	r := redact.Redactor{}
	redacted := r.Text(content)
	if omitted == 0 && len(redacted) <= maxMemoryContentBytes {
		return redacted, nil
	}
	budget := maxMemoryContentBytes
	for attempt := 0; attempt < 4 && budget > importOmissionReserve; attempt++ {
		bounded := r.Text(omitMiddle(redacted, budget, omitted))
		if len(bounded) <= maxMemoryContentBytes {
			return bounded, nil
		}
		budget -= len(bounded) - maxMemoryContentBytes + importOmissionReserve
	}
	return "", fmt.Errorf("imported memory could not be bounded to the 64 KiB content budget")
}

// omitMiddle keeps about three quarters of budget from the start of text and
// the rest from its end, joined by a notice naming the omitted byte count
// (including alreadyOmitted bytes dropped before redaction).
func omitMiddle(text string, budget, alreadyOmitted int) string {
	available := budget - importOmissionReserve
	head := cutBefore(text, available*3/4)
	tail := cutAfter(text, len(text)-(available-available*3/4))
	if tail < head {
		tail = head
	}
	return text[:head] + fmt.Sprintf("\n[trace-brain: %d bytes omitted to fit the 64 KiB memory budget]\n", tail-head+alreadyOmitted) + text[tail:]
}

// cutAtLine moves offset n to a nearby line start (forward when after is set,
// backward otherwise), or else to a rune boundary.
func cutAtLine(text string, n int, after bool) int {
	if after {
		if i := strings.IndexByte(text[n:min(len(text), n+4096)], '\n'); i >= 0 {
			return n + i + 1
		}
		for n < len(text) && !utf8.RuneStart(text[n]) {
			n++
		}
		return n
	}
	if i := strings.LastIndexByte(text[max(0, n-4096):n], '\n'); i >= 0 {
		return max(0, n-4096) + i + 1
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return n
}

const redactionMarkerOpen, redactionMarkerClose = "⟨redacted:", "⟩"

// cutBefore returns an end offset at or before n that is a rune boundary and
// not inside a redaction marker.
func cutBefore(text string, n int) int {
	if n >= len(text) {
		return len(text)
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	window := max(0, n-64)
	if open := strings.LastIndex(text[window:n], redactionMarkerOpen); open >= 0 && !strings.Contains(text[window+open:n], redactionMarkerClose) {
		n = window + open
	}
	return n
}

// cutAfter returns a start offset at or after n that is a rune boundary and
// not inside a redaction marker.
func cutAfter(text string, n int) int {
	if n <= 0 {
		return 0
	}
	for n < len(text) && !utf8.RuneStart(text[n]) {
		n++
	}
	window := max(0, n-64)
	if open := strings.LastIndex(text[window:n], redactionMarkerOpen); open >= 0 && !strings.Contains(text[window+open:n], redactionMarkerClose) {
		if end := strings.Index(text[n:], redactionMarkerClose); end >= 0 {
			n += end + len(redactionMarkerClose)
		}
	}
	return n
}

// NewMemoryID allocates a collision-resistant memory ID from crypto/rand,
// checking the database and retrying so an existing ID is never reused. Time
// stamps alone can collide within one nanosecond across concurrent writers.
func NewMemoryID(ctx context.Context, db *sql.DB) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			continue
		}
		id := "mem-" + hex.EncodeToString(random[:])
		var exists int
		err := db.QueryRowContext(ctx, "SELECT 1 FROM memories WHERE id=?", id).Scan(&exists)
		if err == sql.ErrNoRows {
			return id, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate a unique memory ID")
}

func Remember(ctx context.Context, db *sql.DB, id, scope, content, source string, at int64) error {
	var err error
	if content, scope, err = redactMemoryText(content, scope); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO memories(id,scope,content,source,created_at) VALUES(?,?,?,?,?)", id, scope, content, source, at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO embeddings(memory_id,dim,vector) VALUES(?,?,?)", id, embed.Dim, embed.Encode(embed.Embed(content))); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO memory_revisions(memory_id,revision,scope,content,recorded_at) VALUES(?,1,?,?,?)", id, scope, content, at); err != nil {
		return err
	}
	return tx.Commit()
}
