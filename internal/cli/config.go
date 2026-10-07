package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/nenych/chcli/internal/auth"
)

func (a *app) configCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the configuration",
		Args:  noArgs,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "show",
			Short: "Print the effective configuration after merging profile, environment and flags",
			Long: `Print the effective connection configuration: built-in defaults, overlaid by
the selected profile, then environment variables, then command-line flags.
Secrets are redacted.`,
			Example: "  chcli config show --profile production --database analytics",
			Args:    noArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				_, resolved, err := a.resolve(cmd)
				if err != nil {
					return err
				}
				// config.Secret marshals as "***", so nothing sensitive can be printed here.
				enc := yaml.NewEncoder(a.stdout)
				enc.SetIndent(2)
				if err := enc.Encode(resolved); err != nil {
					return err
				}
				return enc.Close()
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List the configured connection profiles",
			Args:  noArgs,
			RunE: func(*cobra.Command, []string) error {
				file, err := a.loadConfig()
				if err != nil {
					return err
				}
				names := file.ProfileNames()
				if len(names) == 0 {
					fmt.Fprintf(a.stderr, "No profiles configured in %s\n", a.configFile())
					return nil
				}
				tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "PROFILE\tHOST\tAUTH")
				for _, name := range names {
					p := file.Connections[name]
					authType := p.Auth.Type
					if authType == "" {
						authType = "password"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\n", name, p.Host, auth.Label(authType))
				}
				return tw.Flush()
			},
		},
		&cobra.Command{
			Use:   "path",
			Short: "Print the location of the configuration file",
			Args:  noArgs,
			Run:   func(*cobra.Command, []string) { fmt.Fprintln(a.stdout, a.configFile()) },
		},
	)
	return cmd
}
