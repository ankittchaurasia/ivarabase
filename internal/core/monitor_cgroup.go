package core

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Inside a container /proc shows the whole host. When the process runs under
// a cgroup memory or CPU limit (Docker --memory / --cpus, compose limits,
// Kubernetes), monitoring reports against that limit instead, so the charts
// and alerts describe what IvaraBase can actually use.

func readCG(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func cgStat(path, key string) (float64, bool) {
	s, ok := readCG(path)
	if !ok {
		return 0, false
	}
	for _, l := range strings.Split(s, "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == key {
			v, err := strconv.ParseFloat(f[1], 64)
			return v, err == nil
		}
	}
	return 0, false
}

// cgroupMem returns the container's memory limit and its usage without
// reclaimable page cache, when a limit is set.
func cgroupMem() (total, used float64, ok bool) {
	if runtime.GOOS != "linux" {
		return 0, 0, false
	}
	if lim, ok := readCG("/sys/fs/cgroup/memory.max"); ok { // cgroup v2
		if lim == "max" {
			return 0, 0, false
		}
		t, err := strconv.ParseFloat(lim, 64)
		cur, ok2 := readCG("/sys/fs/cgroup/memory.current")
		if err != nil || !ok2 {
			return 0, 0, false
		}
		u, _ := strconv.ParseFloat(cur, 64)
		if inactive, ok := cgStat("/sys/fs/cgroup/memory.stat", "inactive_file"); ok && inactive < u {
			u -= inactive
		}
		return t, u, t > 0
	}
	if lim, ok := readCG("/sys/fs/cgroup/memory/memory.limit_in_bytes"); ok { // v1
		t, err := strconv.ParseFloat(lim, 64)
		if err != nil || t <= 0 || t > 1<<60 {
			return 0, 0, false
		}
		cur, _ := readCG("/sys/fs/cgroup/memory/memory.usage_in_bytes")
		u, _ := strconv.ParseFloat(cur, 64)
		if inactive, ok := cgStat("/sys/fs/cgroup/memory/memory.stat", "total_inactive_file"); ok && inactive < u {
			u -= inactive
		}
		return t, u, true
	}
	return 0, 0, false
}

// cgroupCPUs is the CPU limit in cores (cpu.max quota/period), or 0.
func cgroupCPUs() float64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	if s, ok := readCG("/sys/fs/cgroup/cpu.max"); ok {
		f := strings.Fields(s)
		if len(f) == 2 && f[0] != "max" {
			q, err1 := strconv.ParseFloat(f[0], 64)
			p, err2 := strconv.ParseFloat(f[1], 64)
			if err1 == nil && err2 == nil && p > 0 {
				return q / p
			}
		}
		return 0
	}
	q, ok1 := readCG("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	p, ok2 := readCG("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if ok1 && ok2 {
		qv, _ := strconv.ParseFloat(q, 64)
		pv, _ := strconv.ParseFloat(p, 64)
		if qv > 0 && pv > 0 {
			return qv / pv
		}
	}
	return 0
}

// cgroupCPUPercent: usage of the container's CPU limit since the last call.
func (s *sampler) cgroupCPUPercent() (float64, bool) {
	cores := cgroupCPUs()
	if cores <= 0 {
		return 0, false
	}
	var usage float64 // microseconds
	if v, ok := cgStat("/sys/fs/cgroup/cpu.stat", "usage_usec"); ok {
		usage = v
	} else if raw, ok := readCG("/sys/fs/cgroup/cpuacct/cpuacct.usage"); ok { // v1, nanoseconds
		n, _ := strconv.ParseFloat(raw, 64)
		usage = n / 1000
	} else {
		return 0, false
	}
	now := float64(time.Now().UnixMicro())
	prev := s.prevCG
	s.prevCG = [2]float64{usage, now}
	if prev[1] == 0 || now <= prev[1] {
		return 0, false
	}
	pct := 100 * (usage - prev[0]) / ((now - prev[1]) * cores)
	if pct < 0 {
		return 0, false
	}
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

// effectiveCPUs is the CPU count to divide load by.
func effectiveCPUs() float64 {
	if c := cgroupCPUs(); c > 0 {
		return c
	}
	return float64(runtime.NumCPU())
}
