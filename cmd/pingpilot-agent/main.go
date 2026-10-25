// pingpilot-agent reports a server's CPU, memory, disk and network to Ping Pilott once a minute.
//
//	pingpilot-agent [run]   report until stopped (what the service runs)
//	pingpilot-agent check   test the token, the network and the reading, and say what to fix
//	pingpilot-agent print   take one reading and print what would be sent, without sending it
//	pingpilot-agent version
//
// It opens no port, reads only, and never sends a command line, a file or anything but the figures it prints.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/Jatin-1111/pingpilot-agent/internal/check"
	"github.com/Jatin-1111/pingpilot-agent/internal/client"
	"github.com/Jatin-1111/pingpilot-agent/internal/collect"
	"github.com/Jatin-1111/pingpilot-agent/internal/config"
	"github.com/Jatin-1111/pingpilot-agent/internal/runner"
	"github.com/Jatin-1111/pingpilot-agent/internal/snapshot"
)

// version is set by the release build (-ldflags "-X main.version=1.2.3").
var version = "0.1.0"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	command := "run"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		command, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("pingpilot-agent "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	api := fs.String("api", "", "the Ping Pilott address (default "+config.DefaultAPI+", or PINGPILOT_API)")
	configFile := fs.String("config", "", "a config file (default "+config.DefaultDir(runtime.GOOS)+"/agent.json)")
	tokenFile := fs.String("token-file", "", "the file holding the token (default "+config.DefaultDir(runtime.GOOS)+"/token). The token is never taken from a flag: flags show in the process list")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	logger := log.New(stderr, "pingpilot-agent: ", log.LstdFlags)
	// In a container the host's disks are read through a mounted root (PINGPILOT_HOST_ROOT), and the machine's name can be
	// given when the container does not share it (PINGPILOT_HOSTNAME)
	system := collect.System{Root: os.Getenv("PINGPILOT_HOST_ROOT")}
	name := os.Getenv("PINGPILOT_HOSTNAME")
	if name == "" {
		name = system.Hostname()
	}
	host := snapshot.SanitizeHost(snapshot.Host{Name: name, OS: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: version})

	switch command {
	case "version", "--version":
		fmt.Fprintf(stdout, "pingpilot-agent %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return 0
	case "help":
		fmt.Fprintln(stdout, "usage: pingpilot-agent [run|check|print|version] [--api URL] [--config FILE] [--token-file FILE]")
		return 0
	case "print":
		return printOnce(stdout, stderr, host, system)
	case "run", "check":
	default:
		fmt.Fprintf(stderr, "unknown command %q (try: run, check, print, version)\n", command)
		return 2
	}

	inputs := config.Inputs{APIFlag: *api, ConfigFlag: *configFile, TokenFileFlag: *tokenFile}
	cfg, err := config.Load(inputs)
	userAgent := fmt.Sprintf("pingpilot-agent/%s (%s; %s)", version, runtime.GOOS, runtime.GOARCH)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	collector := collect.New(system)
	sender := client.New(cfg.API, cfg.Token, userAgent)
	// A token replaced in its file (or the environment) is read again for each request, so no restart is needed
	sender.TokenFunc = func() string {
		reloaded, loadErr := config.Load(inputs)
		if loadErr != nil {
			return ""
		}
		return reloaded.Token
	}

	if command == "check" {
		// A missing token is one of the steps the check reports; a broken config file or API address stops it here
		if err != nil && !(cfg.Token == "" && cfg.API != "" && config.ValidateAPI(cfg.API) == nil) {
			fmt.Fprintf(stdout, "Ping Pilott agent %s\n\n  [FAIL] %v\n", version, config.RedactIn(err.Error(), cfg.Token))
			return 1
		}
		return check.Run(ctx, stdout, check.Deps{
			Config: cfg, Version: version, GOOS: runtime.GOOS, Host: host, Collector: collector, Sender: sender,
			Health: func(ctx context.Context, url string) (int, error) {
				req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				if reqErr != nil {
					return 0, reqErr
				}
				req.Header.Set("User-Agent", userAgent)
				res, doErr := (&http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
				if doErr != nil {
					return 0, doErr
				}
				res.Body.Close()
				return res.StatusCode, nil
			},
		})
	}

	if err != nil {
		logger.Printf("%s", config.RedactIn(err.Error(), cfg.Token))
		return 1
	}
	if err := config.CheckToken(cfg.Token); err != nil {
		logger.Printf("%v (read from %s)", err, cfg.TokenFrom)
		return 1
	}
	logger.Printf("started %s: reporting to %s as %s (token %s)", version, cfg.API, host.Name, config.Redact(cfg.Token))
	r := runner.New(collector, sender, host, version, logger.Printf)
	r.Run(ctx)
	logger.Printf("stopped")
	return 0
}

func printOnce(stdout, stderr io.Writer, host snapshot.Host, system collect.System) int {
	c := collect.New(system)
	snap, err := c.Collect(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "cannot read the host: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snapshot.Payload{V: 1, Host: host, Snapshots: []snapshot.Snapshot{snap}}); err != nil {
		fmt.Fprintf(stderr, "cannot encode: %v\n", err)
		return 1
	}
	return 0
}
