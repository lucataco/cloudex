package node

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// HostFacts describes the host from what the OS says. Docker's own view
// (GET /info: its CPUs and memory, Sysbox, its data root's free disk) and the
// firewall self-test come with the Docker runtime (M4); until then the
// runtime is docker and the firewall unchecked.
func HostFacts(unisolated bool) Facts {
	f := Facts{
		Arch:      runtime.GOARCH,
		Runtime:   "docker",
		Isolation: Isolation{Firewall: "unchecked", Unisolated: unisolated},
	}
	if n := runtime.NumCPU(); n > 0 {
		f.CPUs = &n
	}
	if mb := memTotalMB("/proc/meminfo"); mb > 0 {
		f.MemoryMB = &mb
	}
	return f
}

// memTotalMB reads MemTotal from /proc/meminfo; 0 when it can't.
func memTotalMB(path string) int {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0
		}
		kb, err := strconv.Atoi(fields[0])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}
