package main

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

func host() hostInfo {
	model := "unknown " + runtime.GOARCH + " CPU"
	if f, err := os.Open("/proc/cpuinfo"); err == nil {
		s := bufio.NewScanner(f)
		for s.Scan() {
			if k, v, ok := strings.Cut(s.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
				model = strings.TrimSpace(v)
				break
			}
		}
		f.Close()
	}
	osName := "Linux"
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
				osName = strings.Trim(v, `"`)
			}
		}
	}
	var u unix.Utsname
	_ = unix.Uname(&u)
	return hostInfo{Model: model, CPUs: runtime.NumCPU(),
		OS: fmt.Sprintf("%s, kernel %s", osName, unix.ByteSliceToString(u.Release[:]))}
}

func loadAvg() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unavailable"
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return "unavailable"
	}
	return strings.Join(f[:3], " ")
}
