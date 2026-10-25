// Package snapshot is what the agent sends: one reading of the host, in the exact shape the Ping Pilott API accepts.
//
// The API is strict: unknown fields are refused, every number has a range, every list a length, every string a length
// and no control characters. Sanitize makes whatever was measured fit those rules, so a strange host (a mount with a very
// long name, a process called "\x1b[31m") is reported as best it can be rather than refused.
package snapshot

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits the API enforces (utils/hostMetrics.js in the backend). They are repeated here on purpose: a limit changed on
// one side only shows up as a rejected snapshot, and the tests on both sides name the same numbers.
const (
	MaxDisks     = 16
	MaxProcesses = 5
	MaxNameRunes = 64
)

type CPU struct {
	Percent float64     `json:"percent"`
	Cores   int         `json:"cores"`
	Load    *[3]float64 `json:"load,omitempty"`
}

type Memory struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

type Disk struct {
	Mount string `json:"mount"`
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

type Net struct {
	RxBps uint64 `json:"rxBps"`
	TxBps uint64 `json:"txBps"`
}

// Proc is a process by name only, summed over every process with that name. Never a command line: those hold passwords.
type Proc struct {
	Name string  `json:"name"`
	CPU  float64 `json:"cpu"`
	Mem  float64 `json:"mem"`
}

type Snapshot struct {
	// Ts is when it was measured, RFC 3339 in UTC. The server believes it only when it is close to its own clock.
	Ts        string  `json:"ts,omitempty"`
	CPU       CPU     `json:"cpu"`
	Mem       Memory  `json:"mem"`
	Swap      *Memory `json:"swap,omitempty"`
	Disks     []Disk  `json:"disks"`
	Net       *Net    `json:"net,omitempty"`
	UptimeSec uint64  `json:"uptimeSec"`
	Procs     []Proc  `json:"procs,omitempty"`
}

type Host struct {
	Name         string `json:"name,omitempty"`
	OS           string `json:"os,omitempty"`
	Arch         string `json:"arch,omitempty"`
	AgentVersion string `json:"agentVersion,omitempty"`
}

// Payload is one request: the host, and one to ten snapshots (more than one after an outage).
type Payload struct {
	V         int        `json:"v"`
	Host      Host       `json:"host"`
	Snapshots []Snapshot `json:"snapshots"`
}

// CleanName makes a name acceptable: control characters removed, at most 64 characters, and "" when nothing is left.
func CleanName(name string) string {
	if !utf8.ValidString(name) {
		name = strings.ToValidUTF8(name, "")
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > MaxNameRunes {
		name = string([]rune(name)[:MaxNameRunes])
	}
	return name
}

func clampPercent(v float64) float64 {
	switch {
	case v != v: // NaN
		return 0
	case v < 0:
		return 0
	case v > 100:
		return 100
	}
	return v
}

func clampUsed(used, total uint64) uint64 {
	if used > total {
		return total
	}
	return used
}

// Sanitize returns the snapshot adjusted to the API's rules. It does not modify its argument.
func Sanitize(s Snapshot) Snapshot {
	out := s
	out.CPU.Percent = clampPercent(s.CPU.Percent)
	if out.CPU.Cores < 1 {
		out.CPU.Cores = 1
	}
	if s.CPU.Load != nil {
		load := *s.CPU.Load
		for i, v := range load {
			if v != v || v < 0 {
				load[i] = 0
			}
		}
		out.CPU.Load = &load
	}

	out.Mem.Used = clampUsed(s.Mem.Used, s.Mem.Total)
	if s.Swap != nil {
		swap := Memory{Total: s.Swap.Total, Used: clampUsed(s.Swap.Used, s.Swap.Total)}
		out.Swap = &swap
	}

	disks := make([]Disk, 0, len(s.Disks))
	for _, d := range s.Disks {
		mount := CleanName(d.Mount)
		if mount == "" {
			continue
		}
		disks = append(disks, Disk{Mount: mount, Total: d.Total, Used: clampUsed(d.Used, d.Total)})
	}
	// When there are more than the API takes, the biggest are the ones worth watching
	sort.SliceStable(disks, func(i, j int) bool { return disks[i].Total > disks[j].Total })
	if len(disks) > MaxDisks {
		disks = disks[:MaxDisks]
	}
	out.Disks = disks // never nil: the API wants a list, and JSON null is not one

	var procs []Proc
	for _, p := range s.Procs {
		name := CleanName(p.Name)
		if name == "" {
			continue
		}
		cpu := p.CPU
		if cpu != cpu || cpu < 0 {
			cpu = 0
		}
		if cpu > 409600 {
			cpu = 409600
		}
		procs = append(procs, Proc{Name: name, CPU: cpu, Mem: clampPercent(p.Mem)})
	}
	if len(procs) > MaxProcesses {
		procs = procs[:MaxProcesses]
	}
	out.Procs = procs
	return out
}

// SanitizeHost cleans the names the host reports about itself.
func SanitizeHost(h Host) Host {
	return Host{
		Name:         CleanName(h.Name),
		OS:           truncate(CleanName(h.OS), 32),
		Arch:         truncate(CleanName(h.Arch), 16),
		AgentVersion: truncate(CleanName(h.AgentVersion), 24),
	}
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
