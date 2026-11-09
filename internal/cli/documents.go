package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/UseTrace-SI/trace-brain/internal/documents"
	"github.com/UseTrace-SI/trace-brain/internal/redact"
	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/spf13/cobra"
)

var documentSecret = regexp.MustCompile(`(?i)^(?:\.git|\.trace(?:-brain|-graph)?|\.ssh|\.aws|\.codex|\.agents|node_modules|vendor|\.env(?:\..*)?|credentials(?:\..*)?|secrets?(?:\..*)?|id_(?:rsa|ed25519)(?:\.pub)?)$`)

func documentRoot(path string) (string, error) {
	return documentRootPath(path, true)
}
func documentRootPath(path string, requirePresent bool) (string, error) {
	if path == "" {
		return "", fmt.Errorf("--root is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := canonicalDocumentPath(absolute, !requirePresent, 0)
	if err != nil {
		return "", fmt.Errorf("document source is unavailable")
	}
	absolute = resolved
	if (redact.Redactor{}).Text(absolute) != absolute {
		return "", fmt.Errorf("document root contains credentials")
	}
	for _, part := range strings.Split(filepath.ToSlash(absolute), "/") {
		if documentSecret.MatchString(part) {
			return "", fmt.Errorf("document root is a protected directory")
		}
	}
	return absolute, nil
}

// Resolve the existing prefix even after a source directory has disappeared.
// This preserves identities through macOS /var aliases and broken root links.
func canonicalDocumentPath(path string, allowMissing bool, depth int) (string, error) {
	if depth > 40 {
		return "", fmt.Errorf("document path resolution exceeds budget")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !allowMissing || !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	canonicalParent, parentErr := canonicalDocumentPath(parent, true, depth+1)
	if parentErr != nil {
		return "", parentErr
	}
	if info, linkErr := os.Lstat(path); linkErr == nil && info.Mode()&os.ModeSymlink != 0 {
		target, linkErr := os.Readlink(path)
		if linkErr != nil {
			return "", linkErr
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(canonicalParent, target)
		}
		return canonicalDocumentPath(target, true, depth+1)
	}
	return filepath.Join(canonicalParent, filepath.Base(path)), nil
}
func documentPath(path string) bool {
	if len(path) > 512 || (redact.Redactor{}).Text(path) != path || strings.ContainsAny(path, "\\:") || strings.ContainsFunc(path, func(r rune) bool { return r < 32 || r == 127 }) {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || documentSecret.MatchString(part) {
			return false
		}
	}
	return true
}
func openDocument(root *os.Root, path string) (*os.File, error) {
	parts := strings.Split(path, "/")
	current := root
	defer func() {
		if current != root {
			current.Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		before, err := current.Lstat(part)
		if err != nil || !before.IsDir() {
			return nil, fmt.Errorf("document directory changed or is a symlink")
		}
		next, err := current.OpenRoot(part)
		if err != nil {
			return nil, fmt.Errorf("opening document directory failed")
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			next.Close()
			return nil, fmt.Errorf("document directory changed while opening")
		}
		if current != root {
			current.Close()
		}
		current = next
	}
	leaf := parts[len(parts)-1]
	before, err := current.Lstat(leaf)
	if err != nil || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("document is not a regular file")
	}
	file, err := current.OpenFile(leaf, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("opening document failed")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		file.Close()
		return nil, fmt.Errorf("document changed while opening")
	}
	return file, nil
}

func ignoredDocuments(ctx context.Context, root string, paths []string) (map[string]bool, error) {
	ignored := map[string]bool{}
	if len(paths) == 0 {
		return ignored, nil
	}
	probe := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--is-inside-work-tree")
	probe.Env = append(os.Environ(), "LC_ALL=C")
	var diagnostics bytes.Buffer
	probe.Stderr = &diagnostics
	probe.Stdout = io.Discard
	if err := probe.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 128 && strings.Contains(diagnostics.String(), "not a git repository") {
			return ignored, nil
		}
		return nil, fmt.Errorf("checking document ignore policy failed; Git must be available")
	}
	input := strings.Join(paths, "\x00") + "\x00"
	command := exec.CommandContext(ctx, "git", "-C", root, "check-ignore", "--no-index", "-z", "--stdin")
	command.Env = append(os.Environ(), "LC_ALL=C")
	command.Stdin = strings.NewReader(input)
	command.Stderr = io.Discard
	output, err := command.Output()
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		return nil, fmt.Errorf("checking ignored documents failed")
	}
	if len(output) > 512*1024 {
		return nil, fmt.Errorf("document ignore response exceeds budget")
	}
	for _, path := range strings.Split(string(output), "\x00") {
		if path != "" {
			if !slices.Contains(paths, path) {
				return nil, fmt.Errorf("invalid document ignore response")
			}
			ignored[path] = true
		}
	}
	return ignored, nil
}

func collectDocuments(ctx context.Context, path string) ([]store.DocumentInput, int, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, 0, fmt.Errorf("opening document source failed")
	}
	defer root.Close()
	paths := []string{}
	visited, excluded := 0, 0
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("enumerating document source failed")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > 10000 {
			return fmt.Errorf("document scan exceeds 10000 entries")
		}
		if path == "." {
			return nil
		}
		if !documentPath(path) {
			excluded++
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if strings.Count(path, "/") > 16 {
			return fmt.Errorf("document source exceeds directory depth budget")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			excluded++
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".md", ".markdown", ".txt", ".rst", ".docx":
		default:
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".docx") && strings.HasPrefix(filepath.Base(path), "~$") {
			excluded++
			return nil
		}
		if !entry.Type().IsRegular() {
			excluded++
			return nil
		}
		paths = append(paths, path)
		if len(paths) > 512 {
			return fmt.Errorf("document source exceeds 512 candidate files")
		}
		return nil
	})
	if err != nil {
		return nil, excluded, err
	}
	gitContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ignored, err := ignoredDocuments(gitContext, path, paths)
	if err != nil {
		return nil, excluded, err
	}
	collected := []store.DocumentInput{}
	totalInput, totalText := 0, 0
	for _, path := range paths {
		if ignored[path] {
			excluded++
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, excluded, err
		}
		file, err := openDocument(root, path)
		if err != nil {
			return nil, excluded, err
		}
		before, statErr := file.Stat()
		limit := 128 * 1024
		isDOCX := strings.EqualFold(filepath.Ext(path), ".docx")
		if isDOCX {
			limit = documents.MaxDOCXBytes
		}
		data, readErr := io.ReadAll(io.LimitReader(file, int64(limit)+1))
		after, lastErr := file.Stat()
		file.Close()
		if statErr != nil || readErr != nil || lastErr != nil {
			return nil, excluded, fmt.Errorf("reading document failed")
		}
		if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
			return nil, excluded, fmt.Errorf("document changed during reading; retry sync")
		}
		if len(data) > limit {
			return nil, excluded, fmt.Errorf("document exceeds its input byte budget")
		}
		content := string(data)
		fingerprint := sha256.New()
		if isDOCX {
			content, err = documents.DOCX(ctx, data)
			if err != nil {
				return nil, excluded, err
			}
			fingerprint.Write([]byte(documents.DOCXVersion + "\x00"))
		} else if !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
			return nil, excluded, fmt.Errorf("documents must be UTF-8 text up to 128 KiB each")
		}
		totalInput += len(data)
		totalText += len(content)
		if totalInput > 16*1024*1024 || totalText > 16*1024*1024 {
			return nil, excluded, fmt.Errorf("document source exceeds 16 MiB of input or extracted text")
		}
		fingerprint.Write(data)
		collected = append(collected, store.DocumentInput{Path: path, Fingerprint: fmt.Sprintf("%x", fingerprint.Sum(nil)), Content: (redact.Redactor{}).Text(content)})
	}
	return collected, excluded, nil
}

func documentsCmd() *cobra.Command {
	parent := &cobra.Command{Use: "documents", Short: "Import explicit local text and DOCX body sources with freshness and deletion tracking"}
	var root, scope string
	var prune, dryRun bool
	sync := &cobra.Command{Use: "sync", Args: cobra.NoArgs, Short: "Refresh local text and DOCX body documents without modifying source files", RunE: func(cmd *cobra.Command, _ []string) error {
		canonical, err := documentRoot(root)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		documents, excluded, err := collectDocuments(ctx, canonical)
		if err != nil {
			return err
		}
		if dryRun {
			paths := []string{}
			for _, document := range documents {
				paths = append(paths, document.Path)
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"files": paths, "excluded": excluded, "submitted": false, "database_changed": false, "prune_requested": prune})
		}
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		result, err := store.SyncDocuments(ctx, db, canonical, scope, documents, prune, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"result": result, "excluded": excluded, "local_only": true, "source_files_changed": false})
	}}
	sync.Flags().StringVar(&root, "root", "", "explicit local source directory (required)")
	sync.Flags().StringVar(&scope, "scope", "documents", "local memory scope")
	sync.Flags().BoolVar(&prune, "prune", false, "delete local derived records for missing/excluded source documents")
	sync.Flags().BoolVar(&dryRun, "dry-run", false, "scan paths without reading or writing the memory database")
	var listRoot string
	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, Short: "List imported document fingerprints and last refresh time", RunE: func(cmd *cobra.Command, _ []string) error {
		canonical, err := documentRootPath(listRoot, false)
		if err != nil {
			return err
		}
		db, err := readMemoryDB()
		if err != nil {
			return err
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		defer cancel()
		records, err := store.ListDocuments(ctx, db, canonical)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(records)
	}}
	list.Flags().StringVar(&listRoot, "root", "", "explicit local source directory (required)")
	var forgetRoot string
	forget := &cobra.Command{Use: "forget-source", Args: cobra.NoArgs, Short: "Delete one source's derived document records without modifying source files", RunE: func(cmd *cobra.Command, _ []string) error {
		canonical, err := documentRootPath(forgetRoot, false)
		if err != nil {
			return err
		}
		if _, err := os.Stat(dbPath); err != nil {
			return fmt.Errorf("memory index is unavailable")
		}
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		result, err := store.SyncDocuments(ctx, db, canonical, "documents", nil, true, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	forget.Flags().StringVar(&forgetRoot, "root", "", "recorded local source directory (may no longer exist)")
	parent.AddCommand(sync, list, forget)
	return parent
}
