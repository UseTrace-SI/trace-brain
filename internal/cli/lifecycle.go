package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/spf13/cobra"
)

func readMemoryDB() (*sql.DB, error) {
	return openReadOnlyMemory(dbPath)
}

func openReadOnlyMemory(databasePath string) (*sql.DB, error) {
	path, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("memory index is missing; run refresh or remember first: %w", err)
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(2000)"}
	db, err := sql.Open("sqlite", u.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

func getCmd() *cobra.Command {
	return &cobra.Command{Use: "get <id>", Short: "Read one memory with provenance as JSON", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		defer cancel()
		db, err := readMemoryDB()
		if err != nil {
			return err
		}
		defer db.Close()
		m, err := store.Get(ctx, db, args[0])
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(m)
	}}
}
func updateCmd() *cobra.Command {
	var scope string
	cmd := &cobra.Command{Use: "update <id> <content>", Short: "Revise a manual memory, preserving local version history", Args: cobra.MinimumNArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		if err := store.Update(cmd.Context(), db, args[0], scope, strings.Join(args[1:], " "), time.Now().UnixMilli()); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"id": args[0], "updated": true})
	}}
	cmd.Flags().StringVar(&scope, "scope", "", "new scope (empty preserves the existing scope)")
	return cmd
}
func forgetCmd() *cobra.Command {
	return &cobra.Command{Use: "forget <id>", Short: "Delete a memory and its local revisions, vectors and import links", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		if err := store.Forget(cmd.Context(), db, args[0]); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"id": args[0], "forgotten": true, "source_unchanged": true})
	}}
}
func historyCmd() *cobra.Command {
	return &cobra.Command{Use: "history <id>", Short: "Read up to 50 local memory revisions as JSON", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		items, err := store.History(ctx, db, args[0])
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"id": args[0], "revisions": items})
	}}
}
