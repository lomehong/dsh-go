// The dsh launcher: the repo's runnable entry point. It composes the
// selected profile through the plugin catalog and serves until interrupted.
// Subcommand surfaces (agent chat, plugin management) land with their
// rounds; today the launcher owns the compose-and-serve core.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"dshgo/boot"
	"dshgo/cordis"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "dsh: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	profile := flag.String("profile", "headless", "profile name under the harness home")
	home := flag.String("home", "", "harness home (defaults to the platform DSH home)")
	anchor := flag.String("anchor", "", "installation anchor for bundle resolution (defaults to the executable directory)")
	list := flag.Bool("list", false, "print the composed services and exit")
	flag.Parse()

	logger := cordis.StdLogger{}
	anchorPath := *anchor
	if anchorPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve executable: %w", err)
		}
		anchorPath = filepath.Join(filepath.Dir(exe), "package.json")
	}

	app, warnings, err := boot.AssembleProfile("dsh", *profile, anchorPath, *home, boot.CatalogDeps{
		Logger: logger,
		Home:   *home,
	})
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		logger.Warn(warning)
	}
	fmt.Printf("dsh: profile %q composed (%d warnings)\n", *profile, len(warnings))

	if *list {
		for _, service := range []string{
			boot.ServiceTools, boot.ServiceCommands, boot.ServiceSettings,
			boot.ServiceCredential, boot.ServiceWebServer, boot.ServiceSessions,
			boot.ServiceProjections, boot.ServiceAgents, boot.ServiceLlm,
			boot.ServiceSessionPersist, boot.ServiceUserQuestions,
			boot.ServiceUserApproval, boot.ServicePermissionPresets,
			boot.ServiceSystemPrompt, boot.ServiceAgentLoop, boot.ServiceSubagentRuntime,
			boot.ServiceSessionTitle, boot.ServiceWorkspace, boot.ServiceAgentPresets,
			boot.ServiceWebhookRuntime,
		} {
			present := app.Root().Get(service) != nil
			fmt.Printf("  %-20s %v\n", service, present)
		}
		return app.Shutdown()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	fmt.Println("dsh: shutting down")
	return app.Shutdown()
}
