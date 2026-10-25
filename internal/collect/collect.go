// Package collect measures the host. What is measured comes from a Source (gopsutil on a real machine, a fake in tests);
// what is made of it (which disks count, how processes are grouped, how a rate is worked out) is here, and is tested
// without a machine.
package collect

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jatin-1111/pingpilot-agent/internal/snapshot"
)

// Window is how long CPU is sampled for: long enough to be a reading, short enough not to hold the agent up.
const Window = time.Second

type Partition struct {
	Device string
	Mount  string
	FSType string
}

type Usage struct{ Total, Used uint64 }

type ProcSample struct {
	Name string
	CPU  float64 // percent of one core, so a busy multi-threaded process can pass 100
	Mem  float64 // percent of memory
}

type NetCounters struct{ Rx, Tx uint64 }

// Source is the machine. Every method may fail on its own: a host that will not say its load average still has a CPU.
type Source interface {
	CPUPercent(ctx context.Context, window time.Duration) (float64, error)
	CPUCores() int
	LoadAverage() (load [3]float64, ok bool)
	Memory() (Usage, error)
	Swap() (Usage, error)
	Partitions() ([]Partition, error)
	DiskUsage(mount string) (Usage, error)
	Network() (NetCounters, error)
	UptimeSeconds() (uint64, error)
	// Processes samples every process over the window and returns what each used in it.
	Processes(ctx context.Context, window time.Duration) ([]ProcSample, error)
	Hostname() string
}

// Collector turns a Source into snapshots. It remembers the network counters of the last one, to work out a rate.
type Collector struct {
	Src    Source
	Now    func() time.Time
	Window time.Duration

	prevNet  NetCounters
	prevAt   time.Time
	havePrev bool
}

func New(src Source) *Collector {
	return &Collector{Src: src, Now: time.Now, Window: Window}
}

// Collect takes one reading. It fails only when there is no CPU or memory reading at all, because without those the
// snapshot says nothing; everything else that cannot be read is left out.
func (c *Collector) Collect(ctx context.Context) (snapshot.Snapshot, error) {
	var (
		wg       sync.WaitGroup
		cpu      float64
		cpuErr   error
		procs    []ProcSample
		procsErr error
	)
	// CPU and the per-process sampling share the same window, so a collection takes one window, not two
	wg.Add(2)
	go func() { defer wg.Done(); cpu, cpuErr = c.Src.CPUPercent(ctx, c.Window) }()
	go func() { defer wg.Done(); procs, procsErr = c.Src.Processes(ctx, c.Window) }()
	wg.Wait()
	if cpuErr != nil {
		return snapshot.Snapshot{}, cpuErr
	}

	mem, err := c.Src.Memory()
	if err != nil {
		return snapshot.Snapshot{}, err
	}

	now := c.Now()
	s := snapshot.Snapshot{
		Ts:  now.UTC().Format(time.RFC3339),
		CPU: snapshot.CPU{Percent: cpu, Cores: c.Src.CPUCores()},
		Mem: snapshot.Memory{Total: mem.Total, Used: mem.Used},
	}
	if load, ok := c.Src.LoadAverage(); ok {
		s.CPU.Load = &load
	}
	if swap, err := c.Src.Swap(); err == nil && swap.Total > 0 {
		s.Swap = &snapshot.Memory{Total: swap.Total, Used: swap.Used}
	}
	s.Disks = c.disks()
	if up, err := c.Src.UptimeSeconds(); err == nil {
		s.UptimeSec = up
	}
	if procsErr == nil {
		s.Procs = TopProcesses(procs, snapshot.MaxProcesses)
	}
	if counters, err := c.Src.Network(); err == nil {
		if c.havePrev {
			if n, ok := Rate(c.prevNet, counters, c.prevAt, now); ok {
				s.Net = &n
			}
		}
		c.prevNet, c.prevAt, c.havePrev = counters, now, true
	}
	return snapshot.Sanitize(s), nil
}

func (c *Collector) disks() []snapshot.Disk {
	parts, err := c.Src.Partitions()
	if err != nil {
		return nil
	}
	var disks []snapshot.Disk
	for _, p := range FilterPartitions(parts) {
		u, err := c.Src.DiskUsage(p.Mount)
		if err != nil || u.Total == 0 {
			continue
		}
		disks = append(disks, snapshot.Disk{Mount: p.Mount, Total: u.Total, Used: u.Used})
	}
	return disks
}

// Real filesystems on a disk. Everything else (tmpfs, overlay for containers, squashfs for snaps, proc, sys, devices) is
// either not a disk or always full by design, and would raise alerts that mean nothing.
var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true, "jfs": true,
	"reiserfs": true, "ntfs": true, "ntfs3": true, "refs": true, "exfat": true, "vfat": true, "fat32": true, "fat": true,
	"apfs": true, "hfs": true, "hfs+": true, "ufs": true, "zfs_member": true,
}

var skipMountPrefixes = []string{"/snap/", "/var/lib/docker/", "/var/lib/containers/", "/var/lib/kubelet/", "/run/", "/sys/", "/proc/", "/dev/", "/System/Volumes/"}

// FilterPartitions keeps the real disks, once each: a device mounted in several places (a bind mount) is one disk, listed
// under its shortest mount point.
func FilterPartitions(parts []Partition) []Partition {
	byDevice := map[string]Partition{}
	var order []string
	for _, p := range parts {
		if !realFS[strings.ToLower(p.FSType)] || p.Mount == "" {
			continue
		}
		skip := false
		for _, prefix := range skipMountPrefixes {
			if strings.HasPrefix(p.Mount, prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		key := p.Device
		if key == "" {
			key = p.Mount
		}
		seen, ok := byDevice[key]
		if !ok {
			order = append(order, key)
			byDevice[key] = p
		} else if len(p.Mount) < len(seen.Mount) {
			byDevice[key] = p
		}
	}
	out := make([]Partition, 0, len(order))
	for _, key := range order {
		out = append(out, byDevice[key])
	}
	return out
}

// TopProcesses groups processes by name (a web server with forty workers is one entry) and returns the busiest n by CPU,
// then memory. Only names are kept: never the command line, which is where passwords and keys end up.
func TopProcesses(samples []ProcSample, n int) []snapshot.Proc {
	byName := map[string]*snapshot.Proc{}
	var names []string
	for _, s := range samples {
		name := snapshot.CleanName(s.Name)
		if name == "" {
			continue
		}
		p, ok := byName[name]
		if !ok {
			p = &snapshot.Proc{Name: name}
			byName[name] = p
			names = append(names, name)
		}
		p.CPU += s.CPU
		p.Mem += s.Mem
	}
	procs := make([]snapshot.Proc, 0, len(names))
	for _, name := range names {
		procs = append(procs, *byName[name])
	}
	sort.SliceStable(procs, func(i, j int) bool {
		if procs[i].CPU != procs[j].CPU {
			return procs[i].CPU > procs[j].CPU
		}
		return procs[i].Mem > procs[j].Mem
	})
	if len(procs) > n {
		procs = procs[:n]
	}
	return procs
}

// Rate is bytes per second between two readings of the counters. It says "not known" when the time did not move forward
// or a counter went backwards (a reboot, or an interface that came and went), rather than report a huge wrong number.
func Rate(prev, cur NetCounters, prevAt, curAt time.Time) (snapshot.Net, bool) {
	seconds := curAt.Sub(prevAt).Seconds()
	if seconds <= 0 || cur.Rx < prev.Rx || cur.Tx < prev.Tx {
		return snapshot.Net{}, false
	}
	return snapshot.Net{
		RxBps: uint64(float64(cur.Rx-prev.Rx) / seconds),
		TxBps: uint64(float64(cur.Tx-prev.Tx) / seconds),
	}, true
}

// IncludeInterface says whether a network interface is one of the machine's own. Loopback and the virtual interfaces that
// containers and bridges create would count the same bytes again.
func IncludeInterface(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range []string{"lo", "docker", "veth", "br-", "virbr", "cni", "flannel", "cali", "vxlan", "tun", "tap", "loopback", "isatap", "teredo", "vethernet", "vmnet"} {
		if strings.HasPrefix(lower, prefix) {
			// "lo" must be the whole name or lo0: "long-name0" is a real interface
			if prefix == "lo" && lower != "lo" && !strings.HasPrefix(lower, "lo0") {
				continue
			}
			return false
		}
	}
	return true
}
