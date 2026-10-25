package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jatin-1111/pingpilot-agent/internal/snapshot"
)

const token = "ppa_0123456789abcdef01234567_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnop_"

func snap(n int) snapshot.Snapshot {
	return snapshot.Sanitize(snapshot.Snapshot{
		Ts: time.Date(2026, 10, 26, 12, n, 0, 0, time.UTC).Format(time.RFC3339), CPU: snapshot.CPU{Percent: float64(n), Cores: 2},
		Mem: snapshot.Memory{Total: 100, Used: 50}, UptimeSec: uint64(n),
	})
}

func payload(n int) snapshot.Payload {
	p := snapshot.Payload{V: 1, Host: snapshot.Host{Name: "web-1"}}
	for i := 0; i < n; i++ {
		p.Snapshots = append(p.Snapshots, snap(i))
	}
	return p
}

type seen struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []snapshot.Payload
}

func (s *seen) record(r *http.Request) {
	var p snapshot.Payload
	_ = json.NewDecoder(r.Body).Decode(&p)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r)
	s.bodies = append(s.bodies, p)
}

func server(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*Client, *seen) {
	t.Helper()
	s := &seen{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		handler(w, r)
	}))
	t.Cleanup(ts.Close)
	return New(ts.URL, token, "pingpilot-agent/test"), s
}

func reply(w http.ResponseWriter, status int, body string, headers ...string) {
	for i := 0; i+1 < len(headers); i += 2 {
		w.Header().Set(headers[i], headers[i+1])
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func asError(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("expected a client.Error, got %v", err)
	}
	return e
}

func TestASnapshotGoesToTheRightPlaceWithTheTokenInTheHeaderOnly(t *testing.T) {
	c, s := server(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `{"status":"success","data":{"accepted":1,"nextIntervalSec":45,"minAgentVersion":"0.1.0"}}`)
	})
	resp, err := c.Send(context.Background(), payload(1))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Accepted != 1 || resp.NextIntervalSec != 45 || resp.MinAgentVersion != "0.1.0" {
		t.Errorf("%+v", resp)
	}
	r := s.requests[0]
	if r.Method != "POST" || r.URL.Path != "/api/agent/v1/metrics" || r.URL.RawQuery != "" {
		t.Errorf("%s %s", r.Method, r.URL)
	}
	if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("User-Agent") != "pingpilot-agent/test" || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", r.Header)
	}
	if strings.Contains(r.URL.String(), "ppa_") {
		t.Error("the token is never in the address")
	}
	if len(s.bodies[0].Snapshots) != 1 || s.bodies[0].V != 1 || s.bodies[0].Host.Name != "web-1" {
		t.Errorf("%+v", s.bodies[0])
	}
}

func TestEachAnswerTheAPIGivesBecomesTheRightKindOfFailure(t *testing.T) {
	cases := []struct {
		status  int
		body    string
		headers []string
		kind    Kind
		message string
	}{
		{401, `{"status":"error","message":"Invalid agent token","code":"INVALID_AGENT_TOKEN"}`, nil, Unauthorized, "Invalid agent token"},
		{403, `{"status":"error","message":"Host metrics are not part of this account's plan","code":"PLAN_REQUIRED"}`, nil, Forbidden, "plan"},
		{422, `{"status":"error","message":"The snapshot is not in the expected format","code":"INVALID_PAYLOAD","issues":[{"path":"snapshots.0.cpu.percent","message":"Too big"}]}`, nil, Rejected, "expected format"},
		{413, `{"status":"error","message":"request entity too large"}`, nil, Rejected, "too large"},
		{400, `not json at all`, nil, Rejected, "Bad Request"},
		{429, `{"status":"error","message":"Too many snapshots","code":"RATE_LIMITED"}`, []string{"Retry-After", "42"}, RateLimited, "Too many"},
		{404, `{"status":"error","message":"Not found","code":"NOT_FOUND"}`, nil, Unavailable, "not accepting host metrics"},
		{500, `<html>oops</html>`, nil, Unavailable, "Internal Server Error"},
		{502, ``, nil, Unavailable, "Bad Gateway"},
		{503, `{"status":"error","message":"busy"}`, nil, Unavailable, "busy"},
	}
	for _, tc := range cases {
		c, _ := server(t, func(w http.ResponseWriter, r *http.Request) { reply(w, tc.status, tc.body, tc.headers...) })
		_, err := c.Send(context.Background(), payload(1))
		e := asError(t, err)
		if e.Kind != tc.kind || e.Status != tc.status || !strings.Contains(e.Message, tc.message) {
			t.Errorf("HTTP %d: got %+v, want kind %d containing %q", tc.status, e, tc.kind, tc.message)
		}
	}
}

func TestTheIssuesTheAPIListsAreKeptForTheLog(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 422, `{"message":"bad","issues":[{"path":"snapshots.0.cpu.percent","message":"Too big"},{"path":"v","message":"Invalid"}]}`)
	})
	_, err := c.Send(context.Background(), payload(1))
	e := asError(t, err)
	if len(e.Issues) != 2 || e.Issues[0] != "snapshots.0.cpu.percent: Too big" {
		t.Errorf("%v", e.Issues)
	}
}

func TestRetryAfterIsReadAsSecondsOrADate(t *testing.T) {
	if retryAfter("30") != 30*time.Second || retryAfter(" 5 ") != 5*time.Second || retryAfter("") != 0 || retryAfter("soon") != 0 || retryAfter("-4") != 0 {
		t.Error("seconds")
	}
	if d := retryAfter(time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)); d < 80*time.Second || d > 91*time.Second {
		t.Errorf("a date: %s", d)
	}
	if retryAfter(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)) != 0 {
		t.Error("a date in the past")
	}
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 429, `{}`, "Retry-After", "42") })
	_, err := c.Send(context.Background(), payload(1))
	if asError(t, err).RetryAfter != 42*time.Second {
		t.Error("the wait is carried to the caller")
	}
}

func TestAnUnreachableServerIsUnavailableAndTheTokenIsNotInTheMessage(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close() // nothing is listening any more
	_, err := New(url, token, "ua").Send(context.Background(), payload(1))
	e := asError(t, err)
	if e.Kind != Unavailable || e.Status != 0 || strings.Contains(e.Error(), token) || !strings.Contains(e.Message, "could not reach") {
		t.Errorf("%+v", e)
	}
}

func TestASlowServerTimesOutAsUnavailable(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond); reply(w, 200, `{}`) })
	c.HTTP.Timeout = 50 * time.Millisecond
	_, err := c.Send(context.Background(), payload(1))
	if e := asError(t, err); e.Kind != Unavailable || !strings.Contains(e.Message, "timed out") {
		t.Errorf("%+v", e)
	}
}

func TestACancelledContextIsNotAFailureOfTheAPI(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond); reply(w, 200, `{}`) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Send(ctx, payload(1)); !errors.Is(err, context.Canceled) {
		t.Errorf("%v", err)
	}
}

func TestARedirectIsNeverFollowedSoTheTokenCannotBeHandedToAnotherAddress(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect target was called with %q", r.Header.Get("Authorization"))
	}))
	defer other.Close()
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusMovedPermanently)
	})
	_, err := c.Send(context.Background(), payload(1))
	e := asError(t, err)
	if e.Kind != Unavailable || !strings.Contains(e.Message, "redirects") {
		t.Errorf("%+v", e)
	}
}

func TestAnErrorMessageFromTheServerCannotLeakTheToken(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 500, `{"message":"bad header: `+r.Header.Get("Authorization")+`"}`)
	})
	_, err := c.Send(context.Background(), payload(1))
	if strings.Contains(err.Error(), "ABCDEFGHIJKLMNOP") {
		t.Errorf("%v", err)
	}
}

// ── splitting a batch by size ──

func bigSnap(n int) snapshot.Snapshot {
	s := snap(n)
	for i := 0; i < snapshot.MaxDisks; i++ {
		s.Disks = append(s.Disks, snapshot.Disk{Mount: "/mnt/a-fairly-long-mount-point-name-" + strings.Repeat("x", 20) + string(rune('a'+i)), Total: 1 << 40, Used: 1 << 39})
	}
	for i := 0; i < snapshot.MaxProcesses; i++ {
		s.Procs = append(s.Procs, snapshot.Proc{Name: "a-process-with-a-long-name-" + strings.Repeat("y", 30), CPU: 1.5, Mem: 2.5})
	}
	return s
}

func TestABatchThatIsTooBigForOneRequestIsSentInPartsInOrder(t *testing.T) {
	c, s := server(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `{"data":{"accepted":1,"nextIntervalSec":60}}`)
	})
	var snaps []snapshot.Snapshot
	for i := 0; i < 10; i++ {
		snaps = append(snaps, bigSnap(i))
	}
	accepted, _, err := c.SendAll(context.Background(), snapshot.Host{Name: "h"}, snaps)
	if err != nil {
		t.Fatal(err)
	}
	if accepted != 10 || len(s.bodies) < 2 {
		t.Fatalf("accepted %d in %d requests: a full batch of big snapshots needs more than one", accepted, len(s.bodies))
	}
	total, last := 0, -1
	for _, body := range s.bodies {
		raw, _ := json.Marshal(body)
		if len(raw) > MaxBody+200 { // a snapshot is never split, so one alone may be a little over
			t.Errorf("a request of %d bytes", len(raw))
		}
		for _, sn := range body.Snapshots {
			if int(sn.UptimeSec) != last+1 {
				t.Errorf("out of order: %d after %d", sn.UptimeSec, last)
			}
			last = int(sn.UptimeSec)
			total++
		}
	}
	if total != 10 {
		t.Errorf("%d snapshots sent", total)
	}
}

func TestSmallSnapshotsGoInOneRequest(t *testing.T) {
	c, s := server(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, `{"data":{"accepted":5}}`) })
	var snaps []snapshot.Snapshot
	for i := 0; i < 5; i++ {
		snaps = append(snaps, snap(i))
	}
	accepted, _, err := c.SendAll(context.Background(), snapshot.Host{}, snaps)
	if err != nil || accepted != 5 || len(s.bodies) != 1 {
		t.Errorf("accepted %d in %d requests, %v", accepted, len(s.bodies), err)
	}
}

func TestWhenALaterPartFailsTheEarlierPartsAreStillCountedAsSent(t *testing.T) {
	calls := 0
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			reply(w, 200, `{"data":{"accepted":3}}`)
			return
		}
		reply(w, 503, `{"message":"busy"}`)
	})
	var snaps []snapshot.Snapshot
	for i := 0; i < 10; i++ {
		snaps = append(snaps, bigSnap(i))
	}
	accepted, _, err := c.SendAll(context.Background(), snapshot.Host{}, snaps)
	if err == nil || accepted == 0 || accepted >= 10 {
		t.Errorf("accepted %d, err %v", accepted, err)
	}
	if asError(t, err).Kind != Unavailable {
		t.Error("the failure of the part that failed")
	}
}

func TestNothingToSendSendsNothing(t *testing.T) {
	c, s := server(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, `{}`) })
	accepted, _, err := c.SendAll(context.Background(), snapshot.Host{}, nil)
	if err != nil || accepted != 0 || len(s.requests) != 0 {
		t.Errorf("%d %v %d", accepted, err, len(s.requests))
	}
}

func TestATokenReplacedInItsFileIsUsedFromTheNextRequestOn(t *testing.T) {
	c, s := server(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, `{"data":{"accepted":1}}`) })
	current := token
	c.TokenFunc = func() string { return current }
	if _, err := c.Send(context.Background(), payload(1)); err != nil {
		t.Fatal(err)
	}
	current = "ppa_a_new_one"
	if _, err := c.Send(context.Background(), payload(1)); err != nil {
		t.Fatal(err)
	}
	if got := s.requests[0].Header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("first: %s", got)
	}
	if got := s.requests[1].Header.Get("Authorization"); got != "Bearer ppa_a_new_one" {
		t.Errorf("second: %s", got)
	}
}

func TestWhenTheTokenCannotBeReadAgainTheLastGoodOneIsKept(t *testing.T) {
	c, s := server(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, `{"data":{"accepted":1}}`) })
	c.TokenFunc = func() string { return "" } // the file is being rewritten, or gone for a moment
	if _, err := c.Send(context.Background(), payload(1)); err != nil {
		t.Fatal(err)
	}
	if got := s.requests[0].Header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("%s", got)
	}
}
