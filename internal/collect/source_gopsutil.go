package collect

import (
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

// maxProcesses bounds the work on a machine with a runaway number of them
const maxProcesses = 4000

// System is the real machine, read with gopsutil.
//
// Root is for running in a container that has the host's filesystem mounted somewhere (usually /host): disk sizes are read
// under it, since the mount points gopsutil lists are the host's own paths. Empty on an ordinary machine.
type System struct{ Root string }

// RootedPath is where a host mount point is found when the host's filesystem is mounted at root.
func RootedPath(root, mount string) string {
	if root == "" {
		return mount
	}
	return strings.TrimRight(root, "/") + "/" + strings.TrimLeft(mount, "/")
}

func (System) CPUPercent(ctx context.Context, window time.Duration) (float64, error) {
	values, err := cpu.PercentWithContext(ctx, window, false)
	if err != nil {
		return 0, err
	}
	if len(values) == 0 {
		return 0, nil
	}
	return values[0], nil
}

func (System) CPUCores() int {
	n, err := cpu.Counts(true)
	if err != nil || n < 1 {
		return runtime.NumCPU()
	}
	return n
}

func (System) LoadAverage() ([3]float64, bool) {
	// Windows has no load average; gopsutil would invent one from CPU use
	if runtime.GOOS == "windows" {
		return [3]float64{}, false
	}
	avg, err := load.Avg()
	if err != nil {
		return [3]float64{}, false
	}
	return [3]float64{avg.Load1, avg.Load5, avg.Load15}, true
}

func (System) Memory() (Usage, error) {
	v, err := mem.VirtualMemory()
	if err != nil {
		return Usage{}, err
	}
	// What is not available to programs, which is what `free` calls used: cache is not "used" memory
	used := v.Total - v.Available
	if v.Available > v.Total {
		used = v.Used
	}
	return Usage{Total: v.Total, Used: used}, nil
}

func (System) Swap() (Usage, error) {
	v, err := mem.SwapMemory()
	if err != nil {
		return Usage{}, err
	}
	return Usage{Total: v.Total, Used: v.Used}, nil
}

func (System) Partitions() ([]Partition, error) {
	parts, err := disk.Partitions(false)
	if err != nil {
		return nil, err
	}
	out := make([]Partition, 0, len(parts))
	for _, p := range parts {
		out = append(out, Partition{Device: p.Device, Mount: p.Mountpoint, FSType: p.Fstype})
	}
	return out, nil
}

func (s System) DiskUsage(mount string) (Usage, error) {
	u, err := disk.Usage(RootedPath(s.Root, mount))
	if err != nil {
		return Usage{}, err
	}
	return Usage{Total: u.Total, Used: u.Used}, nil
}

func (System) Network() (NetCounters, error) {
	all, err := net.IOCounters(true)
	if err != nil {
		return NetCounters{}, err
	}
	var total NetCounters
	for _, c := range all {
		if IncludeInterface(c.Name) {
			total.Rx += c.BytesRecv
			total.Tx += c.BytesSent
		}
	}
	return total, nil
}

func (System) UptimeSeconds() (uint64, error) { return host.Uptime() }

func (System) Hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

// Processes primes every process's CPU counter, waits the window, and reads what each used in it. Only the name and the
// two figures are read; the command line is never asked for.
func (System) Processes(ctx context.Context, window time.Duration) ([]ProcSample, error) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	if len(procs) > maxProcesses {
		procs = procs[:maxProcesses]
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, p := range procs {
		wg.Add(1)
		sem <- struct{}{}
		go func(p *process.Process) {
			defer wg.Done()
			defer func() { <-sem }()
			_, _ = p.PercentWithContext(ctx, 0) // the first reading is the baseline
		}(p)
	}
	wg.Wait()

	select {
	case <-time.After(window):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var mu sync.Mutex
	out := make([]ProcSample, 0, len(procs))
	for _, p := range procs {
		wg.Add(1)
		sem <- struct{}{}
		go func(p *process.Process) {
			defer wg.Done()
			defer func() { <-sem }()
			name, err := p.NameWithContext(ctx)
			if err != nil || name == "" {
				return // a process that went away, or that we may not look at
			}
			cpuPct, err := p.PercentWithContext(ctx, 0)
			if err != nil {
				return
			}
			memPct, _ := p.MemoryPercentWithContext(ctx)
			mu.Lock()
			out = append(out, ProcSample{Name: name, CPU: cpuPct, Mem: float64(memPct)})
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return out, nil
}
