// Command logpick finds, reads and pulls logs off remote hosts.
package main

import (
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is the release version, set via -ldflags at build time. When it is
// empty the version is read from the embedded build info instead.
var version string

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// cobra has already printed the error.
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "logpick",
		Short:         "Find, read and pull logs off remote hosts",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       buildVersion(),
	}
	root.SetVersionTemplate("logpick {{.Version}}\n")
	return root
}

// buildVersion reports the ldflags version, falling back to the module version
// recorded in the binary and finally to "dev".
func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}
