package cli

import (
	"bytes"
	"testing"
)

func TestVersionReportsBundleVersion(t *testing.T) {
	original := Version
	defer func() { Version = original }()
	Version = "v1.2.3"
	var out bytes.Buffer
	cmd := newRoot()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil || out.String() != "trace-brain v1.2.3\n" {
		t.Fatalf("version output %q %v", out.String(), err)
	}
	if buildVersion() != "v1.2.3" {
		t.Fatal("MCP would not report the bundle version")
	}
}
