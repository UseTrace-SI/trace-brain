package cli

import (
	"fmt"

	brainmcp "github.com/UseTrace-SI/trace-brain/internal/mcp"
	"github.com/UseTrace-SI/trace-brain/internal/semantic"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

func mcpCmd(factory modelFactory) *cobra.Command {
	var model, endpoint string
	cmd := &cobra.Command{Use: "mcp", Short: "Serve memory tools over MCP stdio; learned search requires explicit local model configuration", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if model == "" && cmd.Flags().Changed("semantic-endpoint") {
			return fmt.Errorf("--semantic-endpoint requires --semantic-model")
		}
		// Validate before opening/creating a database, without starting a model,
		// resolving DNS, or contacting the configured service.
		if model != "" {
			if err := semantic.ValidateModel(model); err != nil {
				return err
			}
			probe, err := semantic.NewOllama(endpoint)
			if err != nil {
				return err
			}
			probe.Close()
		}
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		server := brainmcp.NewServer(db)
		if model != "" {
			server, err = brainmcp.NewServerWithSemantic(db, model, endpoint, factory)
			if err != nil {
				return err
			}
		}
		return server.Run(cmd.Context(), &mcp.StdioTransport{})
	}}
	cmd.Flags().StringVar(&model, "semantic-model", "", "enable brain_semantic with this exact installed local model (disabled by default)")
	cmd.Flags().StringVar(&endpoint, "semantic-endpoint", "http://127.0.0.1:11434", "loopback Ollama origin pinned for all semantic tool calls")
	return cmd
}
