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
	"dshgo/host/webhost"
	"dshgo/host/webserver"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "dsh: %v\n", err)
		os.Exit(1)
	}
}

// defaultWebHost and defaultWebPort mirror the official web profile's
// deployment fallbacks (dsh-web-app bundle: 127.0.0.1:3080).
const (
	defaultWebHost = "127.0.0.1"
	defaultWebPort = "3080"
)

// webAlias reports whether the invocation names the web surface: the
// official `dsh web` hard alias for `--profile web`, or an explicit
// `--profile web`.
func webAlias(args []string) (profile string, web bool) {
	for i, arg := range args {
		if arg == "web" && i == 0 {
			return "web", true
		}
		if arg == "--profile" && i+1 < len(args) && args[i+1] == "web" {
			return "web", true
		}
	}
	return "", false
}

// normalizeWebAlias rewrites a leading positional `web` into the explicit
// profile flag so the remaining launcher flags still parse: Go's flag
// package stops at the first non-flag argument, so `dsh web --port 0`
// would otherwise drop every flag after the alias.
func normalizeWebAlias(args []string) []string {
	if len(args) > 0 && args[0] == "web" {
		return append([]string{"--profile", "web"}, args[1:]...)
	}
	return args
}

func run() error {
	profile := flag.String("profile", "headless", "profile name under the harness home")
	home := flag.String("home", "", "harness home (defaults to the platform DSH home)")
	anchor := flag.String("anchor", "", "installation anchor for bundle resolution (defaults to the executable directory)")
	list := flag.Bool("list", false, "print the composed services and exit")
	host := flag.String("host", defaultWebHost, "web bind host (0.0.0.0 reaches it from another machine)")
	port := flag.String("port", defaultWebPort, "web listen port (0 lets the OS pick a free one)")
	noOpen := flag.Bool("no-open", false, "web: do not open the Web UI in the default browser")
	os.Args = append([]string{os.Args[0]}, normalizeWebAlias(os.Args[1:])...)
	flag.Parse()

	args := flag.Args()
	// The web-startup row parses --no-open from the inner arguments; Go's
	// flag package consumes the launcher-level declaration above, so it is
	// re-injected into the inner args for the composition to see.
	if *noOpen {
		args = append(args, "--no-open")
	}
	if aliasProfile, web := webAlias(args); web {
		*profile = aliasProfile
	}
	logger := cordis.StdLogger{}
	anchorPath := *anchor
	if anchorPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve executable: %w", err)
		}
		anchorPath = filepath.Join(filepath.Dir(exe), "package.json")
	}

	var app *boot.App
	var warnings []string
	var err error
	if *profile == "web" {
		// The web profile's web-startup row parses its own flag family
		// (--host/--port/--dev/--trusted-host) from the launcher's inner
		// arguments, so they reach the composition via cmdlineArgs.
		app, warnings, err = boot.AssembleProfileWithCmdline("dsh", *profile, anchorPath, *home, args, boot.CatalogDeps{
			Logger: logger,
			Home:   *home,
			Anchor: anchorPath,
		})
	} else {
		app, warnings, err = boot.AssembleProfile("dsh", *profile, anchorPath, *home, boot.CatalogDeps{
			Logger: logger,
			Home:   *home,
			Anchor: anchorPath,
		})
	}
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
			boot.ServiceWebhookRuntime, boot.ServiceWeb,
		} {
			present := app.Root().Get(service) != nil
			fmt.Printf("  %-20s %v\n", service, present)
		}
		return app.Shutdown()
	}

	if *profile == "web" {
		return serveWeb(app, anchorPath, *host, *port, logger)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	fmt.Println("dsh: shutting down")
	return app.Shutdown()
}

// serveWeb mounts the frontend dist over the composed webserver registry and
// serves until interrupted: the official `dsh web` serve loop (web-startup
// flags over the dsh-web-app bundle defaults).
func serveWeb(app *boot.App, anchor string, host string, port string, logger cordis.Logger) error {
	registryValue := app.Root().Get(boot.ServiceWebServer)
	if registryValue == nil {
		return fmt.Errorf("web: webServer service absent from profile %q", "web")
	}
	record, ok := registryValue.(map[string]any)
	if !ok {
		return fmt.Errorf("web: webServer service has type %T", registryValue)
	}
	registry, ok := record["registry"].(*webserver.Registry)
	if !ok || registry == nil {
		return fmt.Errorf("web: webServer registry missing from service record")
	}
	dist, err := webhost.ResolveFrontendDist(anchor)
	if err != nil {
		return err
	}
	web, err := webhost.Mount(registry, app.Root(), dist, logger)
	if err != nil {
		return err
	}
	if err := web.Listen(host, port); err != nil {
		return err
	}
	fmt.Printf("dsh web: http://%s\n", web.Addr().String())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	fmt.Println("dsh: shutting down")
	if err := web.Close(); err != nil {
		logger.Warn(fmt.Sprintf("web: close: %v", err))
	}
	return app.Shutdown()
}
