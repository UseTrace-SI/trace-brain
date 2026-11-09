package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/spf13/cobra"
)

type sourceReference struct {
	Database string `json:"database"`
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Status   string `json:"status"`
}
type verification struct {
	ID              string            `json:"id"`
	Status          string            `json:"status"`
	ContentVerified bool              `json:"content_verified"`
	Truncated       bool              `json:"truncated"`
	References      []sourceReference `json:"references"`
}

func verifyCmd() *cobra.Command {
	return &cobra.Command{Use: "verify <id>", Short: "Check source references; does not certify a memory's conclusions", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		defer cancel()
		db, err := readMemoryDB()
		if err != nil {
			return err
		}
		defer db.Close()
		if _, err := store.Get(ctx, db, args[0]); err != nil {
			return err
		}
		out := verification{ID: args[0], Status: "unlinked", References: []sourceReference{}}
		rows, err := db.QueryContext(ctx, "SELECT source_db,entity_kind,entity_id FROM memory_imports WHERE memory_id=? ORDER BY source_db,entity_kind,entity_id LIMIT 51", args[0])
		if err != nil {
			return err
		}
		for rows.Next() {
			var ref sourceReference
			if err := rows.Scan(&ref.Database, &ref.Kind, &ref.ID); err != nil {
				rows.Close()
				return err
			}
			if len(out.References) == 50 {
				out.Truncated = true
				break
			}
			out.References = append(out.References, ref)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(out.References) > 0 {
			out.Status = "references_present"
		}
		for i, ref := range out.References {
			status := referenceStatus(ctx, ref)
			out.References[i].Status = status
			if status != "present" {
				out.Status = "unverified_references"
			}
		}
		if out.Truncated {
			out.Status = "unverified_references"
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}}
}

func referenceStatus(ctx context.Context, ref sourceReference) string {
	if ref.Kind != "sessions" && ref.Kind != "turns" {
		return "unsupported_source"
	}
	if len(ref.Database) > 4096 || len(ref.ID) > 4096 || !filepath.IsAbs(ref.Database) {
		return "invalid_reference"
	}
	info, err := os.Stat(ref.Database)
	if err != nil || !info.Mode().IsRegular() {
		return "source_unavailable"
	}
	u := url.URL{Scheme: "file", Path: ref.Database, RawQuery: "mode=ro&_pragma=busy_timeout(100)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return "source_unavailable"
	}
	defer db.Close()
	var exists int
	// Table names come only from the explicit allowlist above.
	err = db.QueryRowContext(ctx, "SELECT 1 FROM "+ref.Kind+" WHERE id=? LIMIT 1", ref.ID).Scan(&exists)
	if err == sql.ErrNoRows {
		return "missing"
	}
	if err != nil {
		return "source_unavailable"
	}
	return "present"
}
