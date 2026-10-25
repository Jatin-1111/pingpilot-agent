package check

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Jatin-1111/pingpilot-agent/internal/client"
	"github.com/Jatin-1111/pingpilot-agent/internal/config"
	"github.com/Jatin-1111/pingpilot-agent/internal/snapshot"
)

const tok = "ppa_0123456789abcdef01234567_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnop_"

type collector struct{ err error }

func (c collector) Collect(context.Context) (snapshot.Snapshot, error) {
	if c.err != nil {
		return snapshot.Snapshot{}, c.err
	}
	return snapshot.Snapshot{
		CPU: snapshot.CPU{Percent: 12, Cores: 4}, Mem: snapshot.Memory{Total: 8 << 30, Used: 3 << 30},
		Disks: []snapshot.Disk{{Mount: "/", Total: 100 << 30, Used: 40 << 30}}, Procs: []snapshot.Proc{{Name: "node"}},
	}, nil
}

type sender struct{ err error }

func (s sender) SendAll(_ context.Context, _ snapshot.Host, snaps []snapshot.Snapshot) (int, client.Response, error) {
	if s.err != nil {
		return 0, client.Response{}, s.err
	}
	return len(snaps), client.Response{}, nil
}

type info struct{ perm fs.FileMode }

func (i info) Name() string       { return "token" }
func (i info) Size() int64        { return 1 }
func (i info) Mode() fs.FileMode  { return i.perm }
func (i info) ModTime() time.Time { return time.Time{} }
func (i info) IsDir() bool        { return false }
func (i info) Sys() any           { return nil }

func deps() Deps {
	return Deps{
		Config:    config.Config{API: "https://pp.example", Token: tok, TokenFrom: "/etc/pingpilot-agent/token"},
		Version:   "0.1.0",
		GOOS:      "linux",
		Host:      snapshot.Host{OS: "linux", Arch: "amd64"},
		Collector: collector{},
		Sender:    sender{},
		Health:    func(context.Context, string) (int, error) { return 200, nil },
		Stat:      func(string) (os.FileInfo, error) { return info{0o600}, nil },
	}
}

func run(d Deps) (int, string) {
	var out bytes.Buffer
	code := Run(context.Background(), &out, d)
	return code, out.String()
}

func TestWhenEverythingWorksItSaysSoAndExitsZero(t *testing.T) {
	code, out := run(deps())
	if code != 0 || !strings.Contains(out, "Everything works") || strings.Contains(out, "FAIL") || strings.Contains(out, "warn") {
		t.Errorf("code %d\n%s", code, out)
	}
	for _, want := range []string{"ppa_0123...", "4 cores", "disk /: 40.0 GiB of 100.0 GiB", "accepted the reading", "linux/amd64"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "ABCDEFGH") {
		t.Errorf("the token is never printed in full\n%s", out)
	}
}

func TestAMissingTokenIsOneStepThatFailsAndTheRestIsSkipped(t *testing.T) {
	d := deps()
	d.Config.Token = ""
	code, out := run(d)
	if code != 1 || !strings.Contains(out, "no token found") || !strings.Contains(out, "skipped") {
		t.Errorf("code %d\n%s", code, out)
	}
	if !strings.Contains(out, "reachable") {
		t.Errorf("the network is still tested, so the person learns that too\n%s", out)
	}
}

func TestATokenThatIsNotShapedLikeOneIsCaughtBeforeAnythingIsSent(t *testing.T) {
	d := deps()
	d.Config.Token = "ppa_oops"
	d.Sender = sender{err: errors.New("must not be called")}
	code, out := run(d)
	if code != 1 || !strings.Contains(out, "does not look like a Ping Pilott agent token") || strings.Contains(out, "must not be called") {
		t.Errorf("code %d\n%s", code, out)
	}
}

func TestAnUnreachableServerSaysWhatToLookAt(t *testing.T) {
	d := deps()
	d.Health = func(context.Context, string) (int, error) {
		return 0, errors.New("dial tcp: lookup pp.example: no such host")
	}
	code, out := run(d)
	if code != 1 || !strings.Contains(out, "cannot reach https://pp.example") || !strings.Contains(out, "firewall or a proxy") || !strings.Contains(out, "skipped") {
		t.Errorf("code %d\n%s", code, out)
	}
}

func TestEachWayTheSignInCanFailHasItsOwnAdvice(t *testing.T) {
	cases := []struct {
		err   error
		wants []string
	}{
		{&client.Error{Kind: client.Unauthorized, Status: 401}, []string{"does not accept this token", "Replace token", "fresh token"}},
		{&client.Error{Kind: client.Forbidden, Status: 403}, []string{"plan does not include host metrics", "Upgrade the plan"}},
		{&client.Error{Kind: client.Rejected, Status: 422, Message: "bad", Issues: []string{"snapshots.0.cpu: Too big"}}, []string{"refused the reading: bad", "snapshots.0.cpu: Too big", "bug in the agent"}},
		{&client.Error{Kind: client.Unavailable, Status: 503, Message: "busy"}, []string{"busy (HTTP 503)"}},
		{errors.New("something odd"), []string{"something odd"}},
	}
	for _, tc := range cases {
		d := deps()
		d.Sender = sender{err: tc.err}
		code, out := run(d)
		if code != 1 {
			t.Errorf("%v: code %d", tc.err, code)
		}
		for _, want := range tc.wants {
			if !strings.Contains(out, want) {
				t.Errorf("%v: missing %q\n%s", tc.err, want, out)
			}
		}
	}
}

func TestBeingSlowedDownIsAWarningNotAFailure(t *testing.T) {
	d := deps()
	d.Sender = sender{err: &client.Error{Kind: client.RateLimited, Status: 429}}
	code, out := run(d)
	if code != 0 || !strings.Contains(out, "[warn]") || strings.Contains(out, "FAIL") {
		t.Errorf("code %d\n%s", code, out)
	}
}

func TestAHostThatCannotBeReadSaysWhatPermissionIsNeeded(t *testing.T) {
	d := deps()
	d.Collector = collector{err: errors.New("open /proc/stat: permission denied")}
	code, out := run(d)
	if code != 1 || !strings.Contains(out, "cannot read this machine's CPU and memory") || !strings.Contains(out, "/proc") {
		t.Errorf("code %d\n%s", code, out)
	}
}

func TestATokenFileOthersCanReadIsWarnedAboutWithTheFix(t *testing.T) {
	d := deps()
	d.Stat = func(string) (os.FileInfo, error) { return info{0o644}, nil }
	code, out := run(d)
	if code != 0 || !strings.Contains(out, "other users on this machine can read the token file") || !strings.Contains(out, "chmod 600 /etc/pingpilot-agent/token") {
		t.Errorf("a warning, not a failure: code %d\n%s", code, out)
	}
	d.Config.TokenFrom = "the PINGPILOT_TOKEN environment variable"
	if _, out := run(d); strings.Contains(out, "chmod") {
		t.Error("a token from the environment has no file to protect")
	}
}

func TestAnErrorThatContainsTheTokenIsPrintedWithoutIt(t *testing.T) {
	d := deps()
	d.Health = func(context.Context, string) (int, error) { return 0, errors.New("bad header Bearer " + tok) }
	_, out := run(d)
	if strings.Contains(out, "ABCDEFGH") {
		t.Errorf("%s", out)
	}
}

func TestSizesAreWrittenForPeople(t *testing.T) {
	for in, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 3 << 30: "3.0 GiB", 5 << 40: "5.0 TiB"} {
		if got := size(in); got != want {
			t.Errorf("size(%d) = %s, want %s", in, got, want)
		}
	}
}
