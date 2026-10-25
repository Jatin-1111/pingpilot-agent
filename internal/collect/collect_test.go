package collect

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// fake is a machine whose readings the test chooses.
type fake struct {
	cpu      float64
	cpuErr   error
	load     [3]float64
	loadOK   bool
	mem      Usage
	memErr   error
	swap     Usage
	parts    []Partition
	usage    map[string]Usage
	net      NetCounters
	netErr   error
	uptime   uint64
	procs    []ProcSample
	procsErr error
}

func (f *fake) CPUPercent(context.Context, time.Duration) (float64, error) { return f.cpu, f.cpuErr }
func (f *fake) CPUCores() int                                              { return 4 }
func (f *fake) LoadAverage() ([3]float64, bool)                            { return f.load, f.loadOK }
func (f *fake) Memory() (Usage, error)                                     { return f.mem, f.memErr }
func (f *fake) Swap() (Usage, error)                                       { return f.swap, nil }
func (f *fake) Partitions() ([]Partition, error)                           { return f.parts, nil }
func (f *fake) DiskUsage(m string) (Usage, error) {
	u, ok := f.usage[m]
	if !ok {
		return Usage{}, errors.New("no such mount")
	}
	return u, nil
}
func (f *fake) Network() (NetCounters, error)  { return f.net, f.netErr }
func (f *fake) UptimeSeconds() (uint64, error) { return f.uptime, nil }
func (f *fake) Processes(context.Context, time.Duration) ([]ProcSample, error) {
	return f.procs, f.procsErr
}
func (f *fake) Hostname() string { return "web-1" }

func newFake() *fake {
	return &fake{
		cpu: 37.5, load: [3]float64{0.5, 0.6, 0.7}, loadOK: true,
		mem: Usage{Total: 8 << 30, Used: 3 << 30}, swap: Usage{Total: 2 << 30, Used: 1 << 20},
		parts:  []Partition{{Device: "/dev/sda1", Mount: "/", FSType: "ext4"}},
		usage:  map[string]Usage{"/": {Total: 100 << 30, Used: 40 << 30}},
		uptime: 12345,
		procs:  []ProcSample{{Name: "node", CPU: 12, Mem: 3}},
	}
}

func collector(f *fake, now *time.Time) *Collector {
	c := New(f)
	c.Window = 0
	c.Now = func() time.Time { return *now }
	return c
}

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func TestACollectionFillsEveryPartOfTheSnapshot(t *testing.T) {
	now := t0
	s, err := collector(newFake(), &now).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Ts != "2026-10-26T12:00:00Z" || s.CPU.Percent != 37.5 || s.CPU.Cores != 4 || s.CPU.Load == nil || s.CPU.Load[2] != 0.7 {
		t.Errorf("cpu %+v ts %s", s.CPU, s.Ts)
	}
	if s.Mem.Total != 8<<30 || s.Mem.Used != 3<<30 || s.Swap == nil || s.UptimeSec != 12345 {
		t.Errorf("memory %+v swap %+v uptime %d", s.Mem, s.Swap, s.UptimeSec)
	}
	if len(s.Disks) != 1 || s.Disks[0].Mount != "/" || len(s.Procs) != 1 || s.Procs[0].Name != "node" {
		t.Errorf("disks %+v procs %+v", s.Disks, s.Procs)
	}
	if s.Net != nil {
		t.Error("the first reading has no network rate: there is nothing to compare it with")
	}
}

func TestTheNetworkRateComesFromTwoReadings(t *testing.T) {
	now := t0
	f := newFake()
	c := collector(f, &now)
	f.net = NetCounters{Rx: 1000, Tx: 500}
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(10 * time.Second)
	f.net = NetCounters{Rx: 6000, Tx: 1500}
	s, _ := c.Collect(context.Background())
	if s.Net == nil || s.Net.RxBps != 500 || s.Net.TxBps != 100 {
		t.Errorf("%+v", s.Net)
	}
}

func TestACounterThatWentBackwardsGivesNoRateRatherThanAHugeOne(t *testing.T) {
	now := t0
	f := newFake()
	c := collector(f, &now)
	f.net = NetCounters{Rx: 9000, Tx: 9000}
	c.Collect(context.Background())
	now = t0.Add(time.Minute)
	f.net = NetCounters{Rx: 10, Tx: 10} // a reboot
	if s, _ := c.Collect(context.Background()); s.Net != nil {
		t.Errorf("no rate, got %+v", s.Net)
	}
	now = t0.Add(2 * time.Minute)
	f.net = NetCounters{Rx: 70, Tx: 130}
	if s, _ := c.Collect(context.Background()); s.Net == nil || s.Net.RxBps != 1 || s.Net.TxBps != 2 {
		t.Errorf("and the next one works again: %+v", s.Net)
	}
}

func TestWhatCannotBeReadIsLeftOutAndTheRestIsStillSent(t *testing.T) {
	now := t0
	f := newFake()
	f.loadOK = false
	f.swap = Usage{}
	f.netErr = errors.New("no counters")
	f.procsErr = errors.New("permission denied")
	f.parts = nil
	s, err := collector(f, &now).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.CPU.Load != nil || s.Swap != nil || s.Net != nil || s.Procs != nil || len(s.Disks) != 0 {
		t.Errorf("%+v", s)
	}
	if s.CPU.Percent != 37.5 || s.Mem.Total == 0 {
		t.Error("cpu and memory are still there")
	}
}

func TestWithoutCPUOrMemoryThereIsNothingToSend(t *testing.T) {
	now := t0
	f := newFake()
	f.cpuErr = errors.New("no cpu")
	if _, err := collector(f, &now).Collect(context.Background()); err == nil {
		t.Error("no cpu reading is an error")
	}
	f = newFake()
	f.memErr = errors.New("no memory")
	if _, err := collector(f, &now).Collect(context.Background()); err == nil {
		t.Error("no memory reading is an error")
	}
}

func TestOnlyRealDisksAreWatchedAndEachOnce(t *testing.T) {
	parts := []Partition{
		{"/dev/sda1", "/", "ext4"},
		{"/dev/sda1", "/var/lib/something", "ext4"}, // a bind mount of the same device
		{"/dev/sdb1", "/data", "xfs"},
		{"tmpfs", "/dev/shm", "tmpfs"},
		{"overlay", "/", "overlay"},
		{"/dev/loop3", "/snap/core/123", "squashfs"},
		{"/dev/sdc1", "/var/lib/docker/overlay2/x", "ext4"},
		{"proc", "/proc", "proc"},
		{"C:", "C:", "NTFS"},
		{"/dev/sdd1", "", "ext4"},
	}
	got := FilterPartitions(parts)
	var mounts []string
	for _, p := range got {
		mounts = append(mounts, p.Mount)
	}
	want := []string{"/", "/data", "C:"}
	if len(mounts) != len(want) {
		t.Fatalf("got %v, want %v", mounts, want)
	}
	for i := range want {
		if mounts[i] != want[i] {
			t.Fatalf("got %v, want %v", mounts, want)
		}
	}
}

func TestADeviceMountedTwiceIsListedUnderItsShortestMount(t *testing.T) {
	got := FilterPartitions([]Partition{{"/dev/sda1", "/mnt/backup/long", "ext4"}, {"/dev/sda1", "/mnt/b", "ext4"}})
	if len(got) != 1 || got[0].Mount != "/mnt/b" {
		t.Errorf("%+v", got)
	}
}

func TestADiskThatCannotBeReadIsSkippedNotFatal(t *testing.T) {
	now := t0
	f := newFake()
	f.parts = append(f.parts, Partition{"/dev/sdb1", "/gone", "ext4"}, Partition{"/dev/sdc1", "/empty", "ext4"})
	f.usage["/empty"] = Usage{}
	s, _ := collector(f, &now).Collect(context.Background())
	if len(s.Disks) != 1 || s.Disks[0].Mount != "/" {
		t.Errorf("%+v", s.Disks)
	}
}

func TestProcessesAreGroupedByNameAndOnlyTheBusiestAreKept(t *testing.T) {
	samples := []ProcSample{
		{"nginx", 5, 1}, {"nginx", 6, 1.5}, {"nginx", 4, 1}, // forty workers would be one line
		{"postgres", 30, 12}, {"node", 30, 3}, {"cron", 0, 0.1},
		{"a", 1, 1}, {"b", 2, 1}, {"c", 3, 1}, {"\x00", 99, 99}, {"", 99, 99},
	}
	top := TopProcesses(samples, 5)
	if len(top) != 5 {
		t.Fatalf("%d", len(top))
	}
	if top[0].Name != "postgres" || top[1].Name != "node" {
		t.Errorf("by cpu, then memory: %+v", top)
	}
	if top[2].Name != "nginx" || top[2].CPU != 15 || top[2].Mem != 3.5 {
		t.Errorf("nginx is one entry with the totals: %+v", top[2])
	}
	for _, p := range top {
		if p.Name == "" || p.Name == "\x00" {
			t.Errorf("nameless processes are dropped: %+v", top)
		}
	}
	if len(TopProcesses(nil, 5)) != 0 {
		t.Error("nothing in, nothing out")
	}
}

func TestRateNeedsTimeToHavePassed(t *testing.T) {
	if _, ok := Rate(NetCounters{}, NetCounters{Rx: 1}, t0, t0); ok {
		t.Error("no time has passed")
	}
	if _, ok := Rate(NetCounters{}, NetCounters{Rx: 1}, t0.Add(time.Second), t0); ok {
		t.Error("the clock went backward")
	}
	n, ok := Rate(NetCounters{}, NetCounters{Rx: 100, Tx: 50}, t0, t0.Add(10*time.Second))
	if !ok || n.RxBps != 10 || n.TxBps != 5 {
		t.Errorf("%+v %v", n, ok)
	}
}

func TestOnlyTheMachinesOwnInterfacesCount(t *testing.T) {
	for name, want := range map[string]bool{
		"eth0": true, "ens5": true, "enp0s3": true, "wlan0": true, "Ethernet": true, "Wi-Fi": true, "bond0": true, "long0": true, "local-net": true,
		"lo": false, "lo0": false, "docker0": false, "veth1234": false, "br-abcdef": false, "virbr0": false, "cni0": false,
		"flannel.1": false, "cali123": false, "Loopback Pseudo-Interface 1": false, "vEthernet (WSL)": false,
	} {
		if got := IncludeInterface(name); got != want {
			t.Errorf("IncludeInterface(%q) = %v, want %v", name, got, want)
		}
	}
}

// A real reading of the machine the tests run on. It checks only that the numbers are the kind a machine has.
func TestTheRealMachineGivesSaneNumbers(t *testing.T) {
	if testing.Short() {
		t.Skip("takes a second")
	}
	c := New(System{})
	c.Window = 500 * time.Millisecond
	s, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.CPU.Percent < 0 || s.CPU.Percent > 100 || s.CPU.Cores < 1 {
		t.Errorf("cpu %+v", s.CPU)
	}
	if s.Mem.Total == 0 || s.Mem.Used > s.Mem.Total {
		t.Errorf("memory %+v", s.Mem)
	}
	if s.UptimeSec == 0 {
		t.Error("uptime")
	}
	if len(s.Disks) == 0 {
		// A container's root is an overlay, which is deliberately not a disk: it sees only what is mounted into it
		if _, err := os.Stat("/.dockerenv"); err != nil {
			t.Error("a machine has a disk")
		} else {
			t.Log("no disks: this is a container, whose root is an overlay")
		}
	}
	for _, d := range s.Disks {
		if d.Used > d.Total || d.Mount == "" {
			t.Errorf("disk %+v", d)
		}
	}
	if len(s.Procs) == 0 {
		t.Log("no processes could be read (permissions?)")
	}
	t.Logf("cpu %.1f%% on %d cores, memory %d/%d MB, %d disk(s), top process %v", s.CPU.Percent, s.CPU.Cores, s.Mem.Used>>20, s.Mem.Total>>20, len(s.Disks), s.Procs)
}

func TestAHostMountIsFoundUnderTheMountedRoot(t *testing.T) {
	for _, tc := range []struct{ root, mount, want string }{
		{"", "/data", "/data"}, {"/host", "/", "/host/"}, {"/host", "/data", "/host/data"}, {"/host/", "/var/lib", "/host/var/lib"}, {"/host", "data", "/host/data"},
	} {
		if got := RootedPath(tc.root, tc.mount); got != tc.want {
			t.Errorf("RootedPath(%q, %q) = %q, want %q", tc.root, tc.mount, got, tc.want)
		}
	}
}
