package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRegressionSemanticMCPInvalidConfigurationDoesNotCreateState(t *testing.T) {
	for _, args := range [][]string{
		{"--semantic-endpoint", "http://localhost"},
		{"--semantic-model", "test:1", "--semantic-endpoint", "https://example.com"},
		{"--semantic-model", "test\n1"},
	} {
		path := filepath.Join(t.TempDir(), "missing", "memory.db")
		command := newRootWithModelProvider(func(string) (modelProvider, error) { t.Fatal("validation created provider"); return nil, nil })
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(append([]string{"--db", path, "mcp"}, args...))
		if err := command.Execute(); err == nil {
			t.Fatal("accepted invalid MCP configuration")
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatal("invalid configuration created state", err)
		}
	}
}
