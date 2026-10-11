//go:build unix

// Command waiters is the waiter benchmark, the release gate: a thousand
// waiters, each its own process on the public library, wait against a held budget while the
// harness drives bursts of arrivals, admissions, cancellations and stall recoveries. It reports
// the waiters' aggregate CPU per second of wall time, as a percentage of one core, for each run
// and the spread over the runs, and exits 1 when the median exceeds the gate.
//
// CPU accounting: each waiter's CPU comes from the kernel's rusage of that child (wait4(2):
// ru_utime + ru_stime), minus, for a waiter that already existed when the measured window
// opened, its own getrusage(RUSAGE_SELF) taken on the harness's signal at the window's start.
// A waiter that arrives during the window is counted whole, start-up and enqueue included. The
// harness's getrusage(RUSAGE_CHILDREN) delta over the run is printed as a cross-check. The
// processes that publish cancel requests (`coreslot cancel`) are counted separately, as control.
//
// Usage:
//
//	go build -o /tmp/b/coreslot ./cmd/coreslot && go build -o /tmp/b/waiters ./bench/waiters
//	/tmp/b/waiters -coreslot /tmp/b/coreslot -dir /tmp/b/work
//
// The same binary is the waiter (`waiters waiter ...`) and the process-group leader
// (`waiters leader`): every child joins the leader's group and reads the harness's pipe on
// stdin, so the group is killed on every exit path and a child whose harness died exits.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"
)

const gatePercent = 2.0 // of one core

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "waiter":
			os.Exit(waiterMain(os.Args[2:]))
		case "leader":
			os.Exit(leaderMain())
		}
	}
	os.Exit(benchMain(os.Args[1:]))
}

func benchMain(args []string) int {
	var p params
	fs := flag.NewFlagSet("waiters", flag.ContinueOnError)
	fs.IntVar(&p.N, "n", 1000, "waiters kept queued")
	fs.DurationVar(&p.Duration, "duration", 120*time.Second, "measured window per run")
	fs.IntVar(&p.Runs, "runs", 5, "runs")
	fs.DurationVar(&p.BurstEvery, "burst-every", 10*time.Second, "interval between bursts")
	fs.IntVar(&p.Size, "size", 8, "cpu budget size; the harness holds all of it between bursts")
	fs.IntVar(&p.Release, "release", 4, "tokens the harness releases at each burst (admissions)")
	fs.DurationVar(&p.AdmitWindow, "admit-window", 3*time.Second, "how long released tokens stay free")
	fs.IntVar(&p.Cancel, "cancel", 5, "cancel requests (`coreslot cancel <id>`) per burst")
	fs.IntVar(&p.Withdraw, "withdraw", 5, "withdrawals (SIGTERM, the waiter's Wait context ends) per burst")
	fs.IntVar(&p.Stall, "stall", 3, "waiters stopped (SIGSTOP) per burst, the queue head first")
	fs.DurationVar(&p.StallFor, "stall-for", 12*time.Second, "how long a stopped waiter stays stopped")
	fs.DurationVar(&p.Settle, "settle", 5*time.Second, "wait after the last enqueue before the window opens")
	fs.StringVar(&p.coreslot, "coreslot", "", "path to the coreslot binary (publishes cancel requests)")
	fs.StringVar(&p.dir, "dir", "", "work directory (a state directory per run is created under it)")
	fs.StringVar(&p.profile, "profile", "", "write a CPU profile of one waiter (the middle of the initial queue) to this path; such a run is not representative")
	fs.IntVar(&p.profileHz, "profile-hz", 500, "sampling rate of -profile")
	jsonOut := fs.String("json", "", "also write the results as JSON to this path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if p.coreslot == "" || p.dir == "" {
		fmt.Fprintln(os.Stderr, "waiters: -coreslot and -dir are required")
		return 2
	}
	var err error
	if p.coreslot, err = filepath.Abs(p.coreslot); err != nil {
		fmt.Fprintln(os.Stderr, "waiters:", err)
		return 2
	}
	if p.dir, err = filepath.Abs(p.dir); err != nil {
		fmt.Fprintln(os.Stderr, "waiters:", err)
		return 2
	}
	if p.self, err = os.Executable(); err != nil {
		fmt.Fprintln(os.Stderr, "waiters:", err)
		return 2
	}

	killGroupOnSignal()
	h := host()
	fmt.Printf("host: %s; %d logical CPUs; %s; %s %s/%s; waiter GOMAXPROCS default\n",
		h.Model, h.CPUs, h.OS, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Printf("setup: %d waiters (cpu=1 each), budget cpu=%d held by the harness, window %s per run, "+
		"a burst every %s: refill arrivals, %d tokens released for %s, %d cancel requests, %d withdrawals, "+
		"%d stopped for %s (head first)\n",
		p.N, p.Size, p.Duration, p.BurstEvery, p.Release, p.AdmitWindow, p.Cancel, p.Withdraw, p.Stall, p.StallFor)
	fmt.Println("cpu accounting: per-waiter wait4 rusage (user+sys) minus each pre-window waiter's own " +
		"getrusage(RUSAGE_SELF) at the window's start; cross-check: harness getrusage(RUSAGE_CHILDREN)")

	var results []result
	for i := 1; i <= p.Runs; i++ {
		r, err := runOnce(p, i)
		if err != nil {
			fmt.Fprintf(os.Stderr, "waiters: run %d: %v\n", i, err)
			return 2
		}
		results = append(results, r)
		fmt.Println(r.line())
	}
	pcts := make([]float64, len(results))
	for i, r := range results {
		pcts[i] = r.Percent
	}
	slices.Sort(pcts)
	median := pcts[len(pcts)/2]
	if len(pcts)%2 == 0 {
		median = (pcts[len(pcts)/2-1] + pcts[len(pcts)/2]) / 2
	}
	verdict := "PASS"
	if median > gatePercent {
		verdict = "FAIL"
	}
	fmt.Printf("summary: %d runs; %% of one core: min %.3f, median %.3f, max %.3f; gate median <= %.1f %%: %s\n",
		len(pcts), pcts[0], median, pcts[len(pcts)-1], gatePercent, verdict)
	if *jsonOut != "" {
		out := struct {
			Host    hostInfo `json:"host"`
			Go      string   `json:"go"`
			Params  params   `json:"params"`
			Runs    []result `json:"runs"`
			Min     float64  `json:"min_percent"`
			Median  float64  `json:"median_percent"`
			Max     float64  `json:"max_percent"`
			Verdict string   `json:"verdict"`
		}{h, runtime.Version(), p, results, pcts[0], median, pcts[len(pcts)-1], verdict}
		b, _ := json.MarshalIndent(out, "", "  ")
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "waiters:", err)
			return 2
		}
	}
	if verdict == "FAIL" {
		return 1
	}
	return 0
}

// params are the benchmark's settings.
type params struct {
	N           int           `json:"n"`
	Duration    time.Duration `json:"window_ns"`
	Runs        int           `json:"runs"`
	BurstEvery  time.Duration `json:"burst_every_ns"`
	Size        int           `json:"size"`
	Release     int           `json:"release"`
	AdmitWindow time.Duration `json:"admit_window_ns"`
	Cancel      int           `json:"cancel"`
	Withdraw    int           `json:"withdraw"`
	Stall       int           `json:"stall"`
	StallFor    time.Duration `json:"stall_for_ns"`
	Settle      time.Duration `json:"settle_ns"`

	coreslot, dir, self, profile string
	profileHz                    int
}

// hostInfo describes the machine.
type hostInfo struct {
	Model string `json:"model"`
	CPUs  int    `json:"logical_cpus"`
	OS    string `json:"os"`
}
