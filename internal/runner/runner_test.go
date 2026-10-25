package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Jatin-1111/pingpilot-agent/internal/client"
	"github.com/Jatin-1111/pingpilot-agent/internal/snapshot"
)

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

type fakeCollector struct {
	clock *clock
	n     int
	err   error
}

func (f *fakeCollector) Collect(context.Context) (snapshot.Snapshot, error) {
	if f.err != nil {
		return snapshot.Snapshot{}, f.err
	}
	f.n++
	return snapshot.Snapshot{Ts: f.clock.now.Format(time.RFC3339), UptimeSec: uint64(f.n), CPU: snapshot.CPU{Cores: 1}}, nil
}

type fakeSender struct {
	results []sendResult // one per call; the last repeats
	calls   [][]snapshot.Snapshot
}

type sendResult struct {
	resp client.Response
	err  error
}

func (f *fakeSender) SendAll(_ context.Context, _ snapshot.Host, snaps []snapshot.Snapshot) (int, client.Response, error) {
	f.calls = append(f.calls, append([]snapshot.Snapshot(nil), snaps...))
	i := len(f.calls) - 1
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	res := f.results[i]
	if res.err != nil {
		return 0, client.Response{}, res.err
	}
	return len(snaps), res.resp, nil
}

type logs struct{ lines []string }

func (l *logs) log(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}
func (l *logs) text() string { return strings.Join(l.lines, "\n") }

func setup(results ...sendResult) (*Runner, *fakeCollector, *fakeSender, *logs, *clock) {
	cl := &clock{now: t0}
	col := &fakeCollector{clock: cl}
	snd := &fakeSender{results: results}
	l := &logs{}
	r := New(col, snd, snapshot.Host{Name: "web-1"}, "0.1.0", l.log)
	r.Now = cl.Now
	r.Rand = func() float64 { return 0.5 } // jitter of exactly 1.0×
	return r, col, snd, l, cl
}

func ok(next int) sendResult {
	return sendResult{resp: client.Response{Accepted: 1, NextIntervalSec: next, MinAgentVersion: "0.1.0"}}
}
func fail(kind client.Kind, status int, msg string) sendResult {
	return sendResult{err: &client.Error{Kind: kind, Status: status, Message: msg}}
}

func TestEachRoundTakesOneReadingSendsItAndWaitsTheInterval(t *testing.T) {
	r, col, snd, _, _ := setup(ok(60))
	wait := r.Step(context.Background())
	if col.n != 1 || len(snd.calls) != 1 || len(snd.calls[0]) != 1 {
		t.Fatalf("%d readings, %d sends", col.n, len(snd.calls))
	}
	if wait != 60*time.Second || r.Buffered() != 0 {
		t.Errorf("wait %s, buffered %d", wait, r.Buffered())
	}
}

func TestTheServerSetsThePaceWithinLimits(t *testing.T) {
	for _, tc := range []struct {
		asked int
		want  time.Duration
	}{{45, 45 * time.Second}, {1, MinInterval}, {99999, MaxInterval}, {0, DefaultInterval}} {
		r, _, _, _, _ := setup(ok(tc.asked))
		if got := r.Step(context.Background()); got != tc.want {
			t.Errorf("asked %d: waited %s, want %s", tc.asked, got, tc.want)
		}
	}
}

func TestReadingsKeptThroughAnOutageAreSentTogetherInOrderWhenTheNetworkIsBack(t *testing.T) {
	r, _, snd, l, cl := setup(
		fail(client.Unavailable, 0, "could not reach the API: connection refused"),
		fail(client.Unavailable, 0, "could not reach the API: connection refused"),
		ok(60),
	)
	for i := 0; i < 3; i++ {
		r.Step(context.Background())
		cl.now = cl.now.Add(time.Minute)
	}
	if len(snd.calls) != 3 || len(snd.calls[2]) != 3 {
		t.Fatalf("the last send carried %d", len(snd.calls[len(snd.calls)-1]))
	}
	for i, s := range snd.calls[2] {
		if int(s.UptimeSec) != i+1 {
			t.Errorf("out of order: %v", snd.calls[2])
		}
	}
	if r.Buffered() != 0 {
		t.Error("the buffer is empty once they are sent")
	}
	if !strings.Contains(l.text(), "cannot send readings") || !strings.Contains(l.text(), "connected again (3 readings sent)") {
		t.Errorf("it says when it starts and when it ends:\n%s", l.text())
	}
}

func TestOnlyTheLastTenReadingsAreKept(t *testing.T) {
	r, _, snd, _, cl := setup(fail(client.Unavailable, 0, "down"))
	for i := 0; i < 25; i++ {
		r.Step(context.Background())
		cl.now = cl.now.Add(time.Second) // quick, so age is not what trims them
	}
	if r.Buffered() != BufferSize {
		t.Errorf("buffered %d", r.Buffered())
	}
	last := snd.calls[len(snd.calls)-1]
	if len(last) != BufferSize || last[0].UptimeSec != 16 || last[len(last)-1].UptimeSec != 25 {
		t.Errorf("the newest ten: %d..%d", last[0].UptimeSec, last[len(last)-1].UptimeSec)
	}
}

func TestReadingsOlderThanTheServerWouldBelieveAreDropped(t *testing.T) {
	r, _, snd, _, cl := setup(fail(client.Unavailable, 0, "down"))
	r.Step(context.Background()) // taken at t0
	cl.now = t0.Add(MaxAge + time.Minute)
	r.Step(context.Background())
	if r.Buffered() != 1 || snd.calls[1][0].UptimeSec != 2 {
		t.Errorf("only the new one is kept: %d buffered", r.Buffered())
	}
}

func TestRetriesAreQuickAtFirstThenSlowDownButNeverBeyondTheInterval(t *testing.T) {
	r, _, _, _, _ := setup(fail(client.Unavailable, 0, "down"))
	var waits []time.Duration
	for i := 0; i < 8; i++ {
		waits = append(waits, r.Step(context.Background()))
	}
	want := []time.Duration{5, 10, 20, 40, 60, 60, 60, 60}
	for i := range want {
		if waits[i] != want[i]*time.Second {
			t.Errorf("failure %d: waited %s, want %s", i+1, waits[i], want[i]*time.Second)
		}
	}
}

func TestAFailureIsReportedWhenItStartsAndNowAndThenNotEveryMinuteForANight(t *testing.T) {
	r, _, _, l, _ := setup(fail(client.Unavailable, 0, "down"))
	for i := 0; i < 25; i++ {
		r.Step(context.Background())
	}
	if len(l.lines) != 3 { // failure 1, 10 and 20
		t.Errorf("%d lines for 25 failures:\n%s", len(l.lines), l.text())
	}
}

func TestJitterSpreadsWaitsByTwentyPercentEitherWay(t *testing.T) {
	r, _, _, _, _ := setup(ok(60))
	for rnd, want := range map[float64]time.Duration{0: 48 * time.Second, 0.5: 60 * time.Second, 0.999: 71976 * time.Millisecond} {
		r.Rand = func() float64 { return rnd }
		if got := r.jitter(60 * time.Second); got < want-time.Millisecond || got > want+time.Millisecond {
			t.Errorf("rand %v: %s, want %s", rnd, got, want)
		}
	}
}

func TestARefusedTokenStopsTheReadingsAndIsTriedAgainSlowly(t *testing.T) {
	r, col, snd, l, _ := setup(fail(client.Unauthorized, 401, "Invalid agent token"), fail(client.Unauthorized, 401, "Invalid agent token"), fail(client.Unauthorized, 401, "x"), fail(client.Unauthorized, 401, "x"), fail(client.Unauthorized, 401, "x"), fail(client.Unauthorized, 401, "x"), fail(client.Unauthorized, 401, "x"), ok(60))
	var waits []time.Duration
	for i := 0; i < 7; i++ {
		waits = append(waits, r.Step(context.Background()))
	}
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour, time.Hour}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("refusal %d: waited %s, want %s", i+1, waits[i], want[i])
		}
	}
	if r.Buffered() != 0 {
		t.Error("what was waiting is dropped: it would be refused too")
	}
	if col.n != 7 || len(snd.calls) != 7 {
		t.Errorf("one probe reading per try, not more: %d readings %d sends", col.n, len(snd.calls))
	}
	if strings.Count(l.text(), "does not accept this agent's token") != 1 {
		t.Errorf("said once, not at every try:\n%s", l.text())
	}
	// and when the token is replaced, the next try works and it says so
	r.Step(context.Background())
	if !strings.Contains(l.text(), "connected again") {
		t.Errorf("%s", l.text())
	}
}

func TestAnAccountWithoutThePlanIsToldSoOnceAndAskedAgainInFifteenMinutes(t *testing.T) {
	r, _, _, l, _ := setup(fail(client.Forbidden, 403, "Host metrics are not part of this account's plan"))
	for i := 0; i < 3; i++ {
		if wait := r.Step(context.Background()); wait != forbiddenWait {
			t.Errorf("waited %s", wait)
		}
	}
	if strings.Count(l.text(), "not part of this account's plan") != 1 || r.Buffered() != 0 {
		t.Errorf("%s", l.text())
	}
}

func TestARejectedReadingIsDroppedSoOneBadReadingCannotBlockTheRest(t *testing.T) {
	r, _, snd, l, _ := setup(
		sendResult{err: &client.Error{Kind: client.Rejected, Status: 422, Message: "The snapshot is not in the expected format", Issues: []string{"snapshots.0.cpu.percent: Too big"}}},
		ok(60),
	)
	if wait := r.Step(context.Background()); wait != DefaultInterval || r.Buffered() != 0 {
		t.Errorf("wait %s buffered %d", wait, r.Buffered())
	}
	r.Step(context.Background())
	if len(snd.calls[1]) != 1 {
		t.Error("the next send is only the new reading")
	}
	if !strings.Contains(l.text(), "snapshots.0.cpu.percent: Too big") {
		t.Errorf("the reason is in the log:\n%s", l.text())
	}
}

func TestBeingSlowedDownIsObeyedAndNothingIsLost(t *testing.T) {
	err := &client.Error{Kind: client.RateLimited, Status: 429, Message: "slow down", RetryAfter: 90 * time.Second}
	r, _, _, _, _ := setup(sendResult{err: err}, ok(60))
	if wait := r.Step(context.Background()); wait != 90*time.Second || r.Buffered() != 1 {
		t.Errorf("wait %s buffered %d", wait, r.Buffered())
	}
}

func TestRateLimitWaitDefaultsAndIsCapped(t *testing.T) {
	r, _, _, _, _ := setup(sendResult{err: &client.Error{Kind: client.RateLimited, Status: 429}})
	if wait := r.Step(context.Background()); wait != rateLimitWait {
		t.Errorf("no Retry-After: %s", wait)
	}
	r, _, _, _, _ = setup(sendResult{err: &client.Error{Kind: client.RateLimited, Status: 429, RetryAfter: 10 * time.Hour}})
	if wait := r.Step(context.Background()); wait != rateLimitMax {
		t.Errorf("an absurd Retry-After: %s", wait)
	}
}

func TestAHostThatCannotBeReadIsReportedAndTheRoundGoesOn(t *testing.T) {
	r, col, snd, l, _ := setup(ok(60))
	col.err = errors.New("permission denied")
	if wait := r.Step(context.Background()); wait != DefaultInterval {
		t.Errorf("wait %s", wait)
	}
	if len(snd.calls) != 0 || !strings.Contains(l.text(), "could not read the host: permission denied") {
		t.Errorf("%d sends\n%s", len(snd.calls), l.text())
	}
}

func TestStoppingTheAgentDuringASendIsNotCountedAsTheServerFailing(t *testing.T) {
	r, _, _, l, _ := setup(sendResult{err: context.Canceled})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Step(ctx)
	if strings.Contains(l.text(), "cannot send") {
		t.Errorf("%s", l.text())
	}
}

func TestAnOlderAgentThanTheServerAsksForIsToldOnce(t *testing.T) {
	r, _, _, l, _ := setup(sendResult{resp: client.Response{NextIntervalSec: 60, MinAgentVersion: "0.2.0"}})
	r.Step(context.Background())
	r.Step(context.Background())
	if strings.Count(l.text(), "please update it") != 1 {
		t.Errorf("%s", l.text())
	}
	r2, _, _, l2, _ := setup(sendResult{resp: client.Response{NextIntervalSec: 60, MinAgentVersion: "0.1.0"}})
	r2.Step(context.Background())
	if len(l2.lines) != 0 {
		t.Errorf("the same version is fine: %s", l2.text())
	}
}

func TestVersionsAreComparedFieldByFieldAndUnknownOnesAreNeverOld(t *testing.T) {
	for _, tc := range []struct {
		have, want string
		old        bool
	}{
		{"0.1.0", "0.1.0", false}, {"0.1.0", "0.1.1", true}, {"0.2.0", "0.1.9", false}, {"0.9.0", "0.10.0", true}, {"1.0.0", "0.99.99", false},
		{"v0.1.0", "0.1.0", false}, {"0.1.0-dev", "0.1.0", false}, {"dev", "0.1.0", false}, {"0.1.0", "x", false}, {"", "", false}, {"1.2", "1.2.3", false},
	} {
		if got := olderThan(tc.have, tc.want); got != tc.old {
			t.Errorf("olderThan(%q, %q) = %v", tc.have, tc.want, got)
		}
	}
}

func TestTheFirstRoundWaitsAFewRandomSecondsSoAFleetDoesNotArriveTogether(t *testing.T) {
	r, _, snd, _, _ := setup(ok(60))
	r.Rand = func() float64 { return 0 }
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r.Run(ctx) // no wait at rand 0, so one round runs, then the 60 s wait is cut short by the context
	if len(snd.calls) != 1 {
		t.Errorf("%d sends", len(snd.calls))
	}
}
