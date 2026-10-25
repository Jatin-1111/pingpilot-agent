package snapshot

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestCleanNameRemovesControlCharactersAndCutsToTheLimit(t *testing.T) {
	cases := map[string]string{
		"nginx":                   "nginx",
		"  node  ":                "node",
		"bad\x1b[31mname":         "bad[31mname",
		"line\nbreak":             "linebreak",
		"\x00\x01":                "",
		"":                        "",
		"C:\\Program Files":       "C:\\Program Files",
		"caf\u00e9":               "caf\u00e9",
		string([]byte{0xff, 'a'}): "a",
	}
	for in, want := range cases {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("\u00e9", 100)
	if got := CleanName(long); len([]rune(got)) != MaxNameRunes {
		t.Errorf("a long name is cut to %d characters, got %d", MaxNameRunes, len([]rune(got)))
	}
}

func TestSanitizeClampsWhatTheAPIWouldRefuse(t *testing.T) {
	load := [3]float64{-1, math.NaN(), 2}
	s := Sanitize(Snapshot{
		CPU:   CPU{Percent: 140, Cores: 0, Load: &load},
		Mem:   Memory{Total: 100, Used: 150},
		Swap:  &Memory{Total: 0, Used: 5},
		Disks: []Disk{{Mount: "/", Total: 10, Used: 20}},
	})
	if s.CPU.Percent != 100 || s.CPU.Cores != 1 {
		t.Errorf("cpu %+v", s.CPU)
	}
	if s.CPU.Load[0] != 0 || s.CPU.Load[1] != 0 || s.CPU.Load[2] != 2 {
		t.Errorf("load %v", *s.CPU.Load)
	}
	if s.Mem.Used != 100 || s.Swap.Used != 0 || s.Disks[0].Used != 10 {
		t.Errorf("used is never more than total: %+v %+v %+v", s.Mem, s.Swap, s.Disks)
	}
	if load[0] != -1 {
		t.Error("the argument is not modified")
	}
	if got := Sanitize(Snapshot{CPU: CPU{Percent: math.NaN(), Cores: 2}}).CPU.Percent; got != 0 {
		t.Errorf("NaN becomes 0, got %v", got)
	}
}

func TestSanitizeKeepsTheBiggestDisksAndTheBusiestProcessesWithinTheLimits(t *testing.T) {
	var disks []Disk
	for i := 0; i < MaxDisks+4; i++ {
		disks = append(disks, Disk{Mount: "/m" + string(rune('a'+i)), Total: uint64(i + 1), Used: 0})
	}
	disks = append(disks, Disk{Mount: "\x00", Total: 999, Used: 0}) // no usable name: dropped
	var procs []Proc
	for i := 0; i < MaxProcesses+3; i++ {
		procs = append(procs, Proc{Name: "p" + string(rune('a'+i)), CPU: float64(i)})
	}
	procs = append(procs, Proc{Name: "\n", CPU: 1}, Proc{Name: "huge", CPU: 1e9, Mem: 500})

	s := Sanitize(Snapshot{Disks: disks, Procs: procs})
	if len(s.Disks) != MaxDisks {
		t.Fatalf("disks %d", len(s.Disks))
	}
	if s.Disks[0].Total != uint64(MaxDisks+4) {
		t.Errorf("the biggest come first, got %d", s.Disks[0].Total)
	}
	if len(s.Procs) != MaxProcesses {
		t.Errorf("processes %d", len(s.Procs))
	}
	big := Sanitize(Snapshot{Procs: []Proc{{Name: "huge", CPU: 1e9, Mem: 500}}}).Procs[0]
	if big.CPU != 409600 || big.Mem != 100 {
		t.Errorf("process figures are clamped: %+v", big)
	}
}

func TestDisksAreAListNeverNull(t *testing.T) {
	raw, err := json.Marshal(Sanitize(Snapshot{CPU: CPU{Cores: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"disks":[]`) {
		t.Errorf("the API wants a list, got %s", raw)
	}
}

// The API refuses unknown fields and wants these exact names, so the shape is pinned here. The backend's own tests pin
// the same names; a rename on either side fails one of them.
func TestThePayloadHasExactlyTheFieldsTheAPIAccepts(t *testing.T) {
	load := [3]float64{0.5, 0.75, 1}
	p := Payload{
		V:    1,
		Host: SanitizeHost(Host{Name: "web-1", OS: "linux", Arch: "amd64", AgentVersion: "0.1.0"}),
		Snapshots: []Snapshot{Sanitize(Snapshot{
			Ts:        "2026-10-26T12:00:00Z",
			CPU:       CPU{Percent: 12.5, Cores: 4, Load: &load},
			Mem:       Memory{Total: 8, Used: 4},
			Swap:      &Memory{Total: 2, Used: 1},
			Disks:     []Disk{{Mount: "/", Total: 100, Used: 40}},
			Net:       &Net{RxBps: 10, TxBps: 20},
			UptimeSec: 99,
			Procs:     []Proc{{Name: "node", CPU: 3, Mem: 1}},
		})},
	}
	raw, _ := json.Marshal(p)
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	keys := func(m map[string]any) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		return out
	}
	want := map[string][]string{
		"":         {"v", "host", "snapshots"},
		"host":     {"name", "os", "arch", "agentVersion"},
		"snapshot": {"ts", "cpu", "mem", "swap", "disks", "net", "uptimeSec", "procs"},
		"cpu":      {"percent", "cores", "load"},
		"mem":      {"total", "used"},
		"disk":     {"mount", "total", "used"},
		"net":      {"rxBps", "txBps"},
		"proc":     {"name", "cpu", "mem"},
	}
	snap := generic["snapshots"].([]any)[0].(map[string]any)
	got := map[string]map[string]any{
		"": generic, "host": generic["host"].(map[string]any), "snapshot": snap, "cpu": snap["cpu"].(map[string]any),
		"mem": snap["mem"].(map[string]any), "disk": snap["disks"].([]any)[0].(map[string]any),
		"net": snap["net"].(map[string]any), "proc": snap["procs"].([]any)[0].(map[string]any),
	}
	for name, fields := range want {
		have := map[string]bool{}
		for _, k := range keys(got[name]) {
			have[k] = true
		}
		if len(have) != len(fields) {
			t.Errorf("%s: has %v, want exactly %v", name, keys(got[name]), fields)
		}
		for _, f := range fields {
			if !have[f] {
				t.Errorf("%s: missing %q", name, f)
			}
		}
	}
}

func TestFieldsThatDoNotApplyAreLeftOut(t *testing.T) {
	raw, _ := json.Marshal(Sanitize(Snapshot{CPU: CPU{Percent: 1, Cores: 1}, Mem: Memory{Total: 1}}))
	for _, field := range []string{`"swap"`, `"net"`, `"procs"`, `"load"`, `"ts"`} {
		if strings.Contains(string(raw), field) {
			t.Errorf("%s should be left out: %s", field, raw)
		}
	}
	host, _ := json.Marshal(SanitizeHost(Host{AgentVersion: "0.1.0"}))
	if string(host) != `{"agentVersion":"0.1.0"}` {
		t.Errorf("an empty host name is not sent: %s", host)
	}
}

func TestHostNamesAreCleanedAndCut(t *testing.T) {
	h := SanitizeHost(Host{Name: "web\n-1", OS: strings.Repeat("x", 80), Arch: strings.Repeat("y", 40), AgentVersion: strings.Repeat("z", 40)})
	if h.Name != "web-1" || len(h.OS) != 32 || len(h.Arch) != 16 || len(h.AgentVersion) != 24 {
		t.Errorf("%+v", h)
	}
}
