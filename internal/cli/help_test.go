package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestEveryCommandHasShortHelp(t *testing.T) {
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Short == "" {
			t.Errorf("%q has no Short help", cmd.CommandPath())
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(newRoot())
}
