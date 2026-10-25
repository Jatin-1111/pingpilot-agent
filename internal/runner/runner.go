// Package runner is the agent's loop: take a reading, send it, and decide how long to wait. A network that drops is bridged
// by keeping the last few readings; a token that is refused is not hammered; the API's own pace is followed.
package runner

import (
	"context"
	"errors"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/Jatin-1111/pingpilot-agent/internal/client"
	"github.com/Jatin-1111/pingpilot-agent/internal/snapshot"
)

const (
	DefaultInterval = 60 * time.Second
	MinInterval     = 30 * time.Second
	MaxInterval     = 5 * time.Minute

	// BufferSize and MaxAge: what is kept while the API cannot be reached. The API believes a snapshot's own time only up to
	// 15 minutes back, so anything older would be stored as if it were new.
	BufferSize = 10
	MaxAge     = 14 * time.Minute

	unauthorizedWait = 5 * time.Minute
	unauthorizedMax  = time.Hour
	forbiddenWait    = 15 * time.Minute
	rateLimitWait    = time.Minute
	rateLimitMax     = 5 * time.Minute
	retryFirst       = 5 * time.Second
)

type Collector interface {
	Collect(ctx context.Context) (snapshot.Snapshot, error)
}

type Sender interface {
	SendAll(ctx context.Context, host snapshot.Host, snaps []snapshot.Snapshot) (accepted int, last client.Response, err error)
}

type Runner struct {
	Collector Collector
	Sender    Sender
	Host      snapshot.Host
	Version   string
	Log       func(format string, args ...any)
	Now       func() time.Time
	Rand      func() float64 // [0,1), for jitter

	interval  time.Duration
	buffer    []snapshot.Snapshot
	failures  int  // consecutive failed sends: network and server trouble
	authFails int  // consecutive refusals of the token
	denied    bool // the account's plan does not include host metrics
	warnedMin bool
}

func New(c Collector, s Sender, host snapshot.Host, version string, log func(string, ...any)) *Runner {
	return &Runner{Collector: c, Sender: s, Host: host, Version: version, Log: log, Now: time.Now, Rand: rand.Float64, interval: DefaultInterval}
}

// Interval is how often the API has asked for readings.
func (r *Runner) Interval() time.Duration { return r.interval }

// Buffered is how many readings are waiting to be sent.
func (r *Runner) Buffered() int { return len(r.buffer) }

// Step does one round and returns how long to wait before the next.
func (r *Runner) Step(ctx context.Context) time.Duration {
	// A token that is refused does not get a new reading every minute: nothing could be sent, and each reading costs a
	// second of CPU sampling.
	if r.authFails > 0 || r.denied {
		return r.send(ctx)
	}

	if snap, err := r.Collector.Collect(ctx); err != nil {
		r.Log("could not read the host: %v", err)
	} else {
		r.keep(snap)
	}
	return r.send(ctx)
}

func (r *Runner) keep(s snapshot.Snapshot) {
	r.buffer = append(r.buffer, s)
	cutoff := r.Now().Add(-MaxAge)
	kept := r.buffer[:0]
	for _, b := range r.buffer {
		if at, err := time.Parse(time.RFC3339, b.Ts); err != nil || at.After(cutoff) {
			kept = append(kept, b)
		}
	}
	r.buffer = kept
	if len(r.buffer) > BufferSize {
		r.buffer = r.buffer[len(r.buffer)-BufferSize:]
	}
}

func (r *Runner) send(ctx context.Context) time.Duration {
	if len(r.buffer) == 0 {
		// Nothing to send, but a refused token still has to be tried again some day
		if r.authFails > 0 || r.denied {
			return r.probe(ctx)
		}
		return r.interval
	}

	accepted, last, err := r.Sender.SendAll(ctx, r.Host, r.buffer)
	if accepted > 0 {
		r.buffer = r.buffer[accepted:]
	}
	if err == nil {
		return r.succeeded(accepted, last)
	}
	if ctx.Err() != nil {
		return r.interval
	}
	return r.failed(err)
}

// probe tries a refused token again with a fresh reading, since the buffer was cleared when it was refused.
func (r *Runner) probe(ctx context.Context) time.Duration {
	snap, err := r.Collector.Collect(ctx)
	if err != nil {
		r.Log("could not read the host: %v", err)
		return r.interval
	}
	r.keep(snap)
	return r.send(ctx)
}

func (r *Runner) succeeded(accepted int, resp client.Response) time.Duration {
	if r.failures > 0 || r.authFails > 0 || r.denied {
		r.Log("connected again (%d reading%s sent)", accepted, plural(accepted))
	}
	r.failures, r.authFails, r.denied = 0, 0, false

	if resp.NextIntervalSec > 0 {
		r.interval = clamp(time.Duration(resp.NextIntervalSec)*time.Second, MinInterval, MaxInterval)
	}
	if !r.warnedMin && resp.MinAgentVersion != "" && olderThan(r.Version, resp.MinAgentVersion) {
		r.warnedMin = true
		r.Log("this agent (%s) is older than the %s that Ping Pilott asks for: please update it", r.Version, resp.MinAgentVersion)
	}
	return r.interval
}

func (r *Runner) failed(err error) time.Duration {
	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		return r.transient(err.Error())
	}
	switch apiErr.Kind {
	case client.Unauthorized:
		if r.authFails == 0 {
			r.Log("Ping Pilott does not accept this agent's token. It may have been replaced or the agent turned off. Copy a new token from the server's page and put it in the token file: the agent picks it up on its own. Trying again in a few minutes.")
		}
		r.authFails++
		r.buffer = nil
		return r.jitter(backoff(unauthorizedWait, unauthorizedMax, r.authFails))
	case client.Forbidden:
		if !r.denied {
			r.Log("host metrics are not part of this account's plan. Upgrade the plan or turn the agent off on the server's page. Trying again in 15 minutes.")
		}
		r.denied = true
		r.buffer = nil
		return r.jitter(forbiddenWait)
	case client.Rejected:
		r.Log("Ping Pilott did not accept a reading, so it was dropped: %s%s", apiErr.Message, issues(apiErr))
		r.buffer = nil
		return r.interval
	case client.RateLimited:
		wait := apiErr.RetryAfter
		if wait <= 0 {
			wait = rateLimitWait
		}
		r.Log("sending too fast; waiting %s", wait.Round(time.Second))
		return r.jitter(clamp(wait, time.Second, rateLimitMax))
	}
	return r.transient(apiErr.Error())
}

func (r *Runner) transient(reason string) time.Duration {
	r.failures++
	// Say it when it starts, and now and then while it lasts: not once a minute for a night
	if r.failures == 1 || r.failures%10 == 0 {
		r.Log("cannot send readings (%s). Keeping the last %d and trying again; failure %d", reason, BufferSize, r.failures)
	}
	return r.jitter(minDuration(backoff(retryFirst, r.interval, r.failures), r.interval))
}

// jitter spreads waits by 20% either way, so agents started together (a fleet restart, an outage that ends) do not all
// come back in the same second.
func (r *Runner) jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*r.Rand()))
}

// Run loops until the context ends. The first round waits a few random seconds for the same reason as jitter.
func (r *Runner) Run(ctx context.Context) {
	wait := time.Duration(r.Rand() * float64(5*time.Second))
	for {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
		wait = r.Step(ctx)
	}
}

func backoff(first, max time.Duration, attempt int) time.Duration {
	d := first
	for i := 1; i < attempt && d < max; i++ {
		d *= 2
	}
	return minDuration(d, max)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func clamp(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func issues(e *client.Error) string {
	if len(e.Issues) == 0 {
		return ""
	}
	return " (" + strings.Join(e.Issues, "; ") + ")"
}

// olderThan compares "1.2.3" style versions, field by field. A version it cannot read is never reported as old.
func olderThan(have, want string) bool {
	a, ok1 := parseVersion(have)
	b, ok2 := parseVersion(want)
	if !ok1 || !ok2 {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	var out [3]int
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
