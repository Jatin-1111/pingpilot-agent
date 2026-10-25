// Package check is `pingpilot-agent check`: the one command to run when something does not work. It follows the same road a
// snapshot takes (token, network, sign-in, reading the host) and says at which step it stops and what to do about it.
package check

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Jatin-1111/pingpilot-agent/internal/client"
	"github.com/Jatin-1111/pingpilot-agent/internal/config"
	"github.com/Jatin-1111/pingpilot-agent/internal/runner"
	"github.com/Jatin-1111/pingpilot-agent/internal/snapshot"
)

// Deps are what a check needs from outside; tests give their own.
type Deps struct {
	Config    config.Config
	Version   string
	GOOS      string
	Host      snapshot.Host
	Collector runner.Collector
	Sender    runner.Sender
	// Health fetches the API's health address, returning the HTTP status. Any answer means the network works.
	Health func(ctx context.Context, url string) (status int, err error)
	Now    func() time.Time
	Stat   func(string) (os.FileInfo, error)
}

type report struct {
	w      io.Writer
	failed bool
}

func (r *report) ok(format string, args ...any)   { fmt.Fprintf(r.w, "  [ ok ] "+format+"\n", args...) }
func (r *report) warn(format string, args ...any) { fmt.Fprintf(r.w, "  [warn] "+format+"\n", args...) }
func (r *report) fail(format string, args ...any) {
	r.failed = true
	fmt.Fprintf(r.w, "  [FAIL] "+format+"\n", args...)
}
func (r *report) hint(format string, args ...any) { fmt.Fprintf(r.w, "         "+format+"\n", args...) }

// Run does the check, writes what it found, and returns the exit code: 0 when everything works, 1 when something does not.
func Run(ctx context.Context, w io.Writer, d Deps) int {
	if d.Now == nil {
		d.Now = time.Now
	}
	r := &report{w: w}
	fmt.Fprintf(w, "Ping Pilott agent %s (%s)\n\n", d.Version, d.Host.OS+"/"+d.Host.Arch)

	fmt.Fprintln(w, "Token")
	tokenOK := checkToken(r, d)

	fmt.Fprintln(w, "\nNetwork")
	reachable := checkNetwork(ctx, r, d)

	fmt.Fprintln(w, "\nSign-in")
	if !tokenOK || !reachable {
		r.warn("skipped: fix the step above first")
	} else {
		checkSignIn(ctx, r, d)
	}

	fmt.Fprintln(w)
	if r.failed {
		fmt.Fprintln(w, "Something needs fixing: see the steps marked FAIL above.")
		return 1
	}
	fmt.Fprintln(w, "Everything works. The agent will report once a minute.")
	return 0
}

func checkToken(r *report, d Deps) bool {
	if d.Config.Token == "" {
		r.fail("no token found")
		r.hint("Copy the token from the server's page in Ping Pilott (Host metrics), and put it in the token file.")
		return false
	}
	if err := config.CheckToken(d.Config.Token); err != nil {
		r.fail("%s", err)
		r.hint("It was read from %s.", d.Config.TokenFrom)
		return false
	}
	r.ok("found in %s (%s)", d.Config.TokenFrom, config.Redact(d.Config.Token))
	if d.Config.TokenFrom != "" && !strings.HasPrefix(d.Config.TokenFrom, "the ") {
		stat := d.Stat
		if stat == nil {
			stat = os.Stat
		}
		if info, err := stat(d.Config.TokenFrom); err == nil && config.LooseTokenFile(info, d.GOOS) {
			r.warn("other users on this machine can read the token file")
			r.hint("Run: chmod 600 %s", d.Config.TokenFrom)
		}
	}
	return true
}

func checkNetwork(ctx context.Context, r *report, d Deps) bool {
	start := d.Now()
	status, err := d.Health(ctx, d.Config.API+"/health")
	took := d.Now().Sub(start).Round(time.Millisecond)
	if err != nil {
		r.fail("cannot reach %s: %s", d.Config.API, config.RedactIn(err.Error(), d.Config.Token))
		r.hint("Check this machine can open https connections to the internet (a firewall or a proxy may be in the way),")
		r.hint("and that the address is right: it is set with PINGPILOT_API or the config file.")
		return false
	}
	if status >= 500 && status != 503 {
		r.warn("%s answered HTTP %d in %s", d.Config.API, status, took)
	} else {
		r.ok("%s reachable (HTTP %d in %s)", d.Config.API, status, took)
	}
	return true
}

func checkSignIn(ctx context.Context, r *report, d Deps) {
	snap, err := d.Collector.Collect(ctx)
	if err != nil {
		r.fail("cannot read this machine's CPU and memory: %v", err)
		r.hint("The agent needs permission to read /proc (Linux) or performance counters (Windows).")
		return
	}
	r.ok("host read: %d core%s, CPU %.0f%%, memory %s of %s, %d disk%s, %d process%s listed",
		snap.CPU.Cores, plural(snap.CPU.Cores), snap.CPU.Percent, size(snap.Mem.Used), size(snap.Mem.Total),
		len(snap.Disks), plural(len(snap.Disks)), len(snap.Procs), plural(len(snap.Procs), "es"))
	for _, disk := range snap.Disks {
		r.hint("disk %s: %s of %s", disk.Mount, size(disk.Used), size(disk.Total))
	}

	accepted, _, err := d.Sender.SendAll(ctx, d.Host, []snapshot.Snapshot{snap})
	if err == nil {
		r.ok("Ping Pilott accepted the reading (%d stored)", accepted)
		return
	}
	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		r.fail("%s", config.RedactIn(err.Error(), d.Config.Token))
		return
	}
	switch apiErr.Kind {
	case client.Unauthorized:
		r.fail("Ping Pilott does not accept this token")
		r.hint("It may have been replaced (Replace token), the agent may have been turned off, or the token was copied wrongly.")
		r.hint("Copy a fresh token from the server's page, put it in the token file, and run this again.")
	case client.Forbidden:
		r.fail("the token is fine, but this account's plan does not include host metrics")
		r.hint("Upgrade the plan, or turn the agent off on the server's page.")
	case client.Rejected:
		r.fail("Ping Pilott refused the reading: %s", apiErr.Message)
		for _, issue := range apiErr.Issues {
			r.hint("%s", issue)
		}
		r.hint("This is a bug in the agent. Please report it with the version above.")
	case client.RateLimited:
		r.warn("Ping Pilott says this is too fast; the agent will wait and retry on its own")
	default:
		r.fail("%s", config.RedactIn(apiErr.Error(), d.Config.Token))
	}
}

func plural(n int, forms ...string) string {
	if n == 1 {
		return ""
	}
	if len(forms) > 0 {
		return forms[0]
	}
	return "s"
}

func size(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
