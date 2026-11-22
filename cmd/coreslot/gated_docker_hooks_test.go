//go:build coreslot_testhooks && darwin

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
	"github.com/core-slot/coreslot/internal/testhooks"
)

// The fake engine's VM is 8 GiB (V 8192 MiB, a Docker reserve of 2048); its process, by the
// readings file, is pid vmPID holding vmNowMiB on the Mac. A label gated with docker_mem 2G then
// reserves min(8192 − 2048, 2 × 2048) = 4096 MiB of growth from its admission.
const (
	vmPID    = 4242
	vmNowMiB = 2048
)

// gatedNeeds gives label a Docker need of 2 GiB.
func gatedNeeds(label string) string { return "[needs.\"" + label + "\"]\ndocker_mem = \"2G\"\n" }

// vm sets the VM the readings file shows, on a calm machine.
func (b *fwBed) vm(state, pid, footprintMiB int64) {
	r := calm(0)
	r["DockerVMState"], r["DockerVMPID"], r["DockerVMFootprintMiB"] = state, pid, footprintMiB
	b.readings(r)
}

// gatedBed is a bed whose VM is identified, with the needs file needs.
func gatedBed(t *testing.T, needs string, opts ...dockertest.Option) *fwBed {
	t.Helper()
	b := newFwBed(t, needs, opts...)
	b.vm(1, vmPID, vmNowMiB)
	return b
}

// liveHold is the body of the one hold in the bed's state directory; nil when there is none.
func (b *fwBed) liveHold(t *testing.T) map[string]any {
	t.Helper()
	names := entries(t, b.dir, "holds")
	if len(names) != 1 {
		return nil
	}
	body, err := os.ReadFile(filepath.Join(b.dir, "holds", names[0]))
	if err != nil {
		return nil
	}
	var h map[string]any
	if json.Unmarshal(body, &h) != nil {
		return nil
	}
	return h
}

// start runs a labelled shell command in the background; its result comes on the channel.
func (b *fwBed) start(t *testing.T, label, script string, env ...string) <-chan result {
	t.Helper()
	done := make(chan result, 1)
	go func() { done <- b.run(t, label, script, env...) }()
	return done
}

// release lets a command waiting on $FWT/go end.
func (b *fwBed) release(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(b.work, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

const waitGo = `while [ ! -e "$FWT/go" ]; do sleep 0.1; done; `

// num is a number of a hold body, and whether it is there.
func num(h map[string]any, key string) (float64, bool) {
	v, ok := h[key].(float64)
	return v, ok
}

// A label's docker_mem gives a command with no other Docker interest a forwarder and a gate: the
// run reads the VM before it starts, and its hold carries the need, the growth it reserves on the
// Mac and the reading's stamp from its first body. Mutation: leaving WithDocker out of wait gives
// a hold without docker_need_mib and no reading.
func TestGatedRunByDockerMem(t *testing.T) {
	b := gatedBed(t, gatedNeeds("g"))
	r := b.run(t, "g", saveHost+saveHold)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if strings.TrimSpace(b.file(t, "host")) == "" {
		t.Error("a run with a Docker need got no forwarder")
	}
	h := b.hold(t)
	need, _ := num(h, "docker_need_mib")
	growth, _ := num(h, "docker_growth_mib")
	stamp, _ := num(h, "docker_sampled_ns")
	if need != 2048 || growth != 4096 || stamp <= 0 {
		t.Errorf("hold: docker_need_mib %v, docker_growth_mib %v, docker_sampled_ns %v; want 2048, 4096 and a stamp", need, growth, stamp)
	}
	if reqs := strings.Join(b.fake.Requests(), "\n"); !strings.Contains(reqs, "GET /info") {
		t.Errorf("no reading of the VM: %q", reqs)
	}
}

// On Docker VMM the reading that admits the run carries the backend, and the hold reserves
// min(8192 − 2048, 3/2 × 2048) = 3072 MiB. Mutation: newDockerSource leaving the backend out of
// the reading gives 4096.
func TestGatedRunOnVMMReservesThreeHalves(t *testing.T) {
	b := newFwBed(t, gatedNeeds("g"))
	r := calm(0)
	r["DockerVMState"], r["DockerVMPID"], r["DockerVMFootprintMiB"], r["DockerVMVMM"] = 1, vmPID, vmNowMiB, 1
	b.readings(r)
	if res := b.run(t, "g", saveHold); res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if growth, _ := num(b.hold(t), "docker_growth_mib"); growth != 3072 {
		t.Errorf("docker_growth_mib %v; want 3072", growth)
	}
}

// A docker_mem that is not a size above 0 makes the entry unreadable: no forwarder, no gate, the
// needs-entry note once. Mutation: accepting a docker_mem of 0 engages the container command.
func TestBadDockerMemNoForwarderNoGate(t *testing.T) {
	b := gatedBed(t, "[needs.\"bad\"]\ndocker_mem = \"0\"\n")
	command := filepath.Join(b.work, "docker")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"+saveHost+saveHold+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := cli(t, b.env, "run", "--restart=yes", "--label", "bad", "--mem", "64M", "--", command)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	h := b.hold(t)
	if _, gated := h["docker_need_mib"]; gated || h["docker_note"] != docker.NoteNeedsEntry || strings.TrimSpace(b.file(t, "host")) != "" {
		t.Errorf("hold %v, DOCKER_HOST %q; want the needs-entry note, no need and no forwarder", h, b.file(t, "host"))
	}
	if n := count(r.stderr, docker.NoteNeedsEntry); n != 1 {
		t.Errorf("%d notices of %q: %s", n, docker.NoteNeedsEntry, r.stderr)
	}
	if reqs := b.fake.Requests(); len(reqs) != 0 {
		t.Errorf("the engine was called: %q", reqs)
	}
}

// A Docker need on a run that cannot be engaged (here a DOCKER_HOST that is no unix socket) is
// ignored: the run starts, says why once and carries the note, and is never refused, also under
// --strict. Mutation: a Docker need left out of the run's interest says nothing and carries no note.
func TestNeedWithoutForwarderIgnored(t *testing.T) {
	b := gatedBed(t, gatedNeeds("n"))
	want := docker.OffNote(docker.OffRemoteHost)
	for _, strict := range []bool{false, true} {
		args := []string{"run", "--restart=yes", "--label", "n", "--mem", "64M"}
		if strict {
			args = append(args, "--strict")
		}
		r := cli(t, append(append([]string{}, b.env...), "DOCKER_HOST=tcp://192.0.2.10:2376"), append(args, "--", "sh", "-c", saveHold)...)
		if r.code != 0 {
			t.Fatalf("strict %v: exit %d: %s", strict, r.code, r.stderr)
		}
		if n := count(r.stderr, want); n != 1 {
			t.Errorf("strict %v: %d notices of %q: %s", strict, n, want, r.stderr)
		}
		if h := b.hold(t); h["docker_note"] != want || h["docker_need_mib"] != nil {
			t.Errorf("strict %v: hold %v; want the note %q and no Docker need", strict, h, want)
		}
	}
	if n := b.fake.Conns(); n != 0 {
		t.Errorf("%d connections to the engine", n)
	}
}

// At each sample of its containers a gated hold rewrites its growth, Growth(V, vm_now, need,
// footprint), and its stamp. Mutation: setDockerKeys not writing the sample's growth keeps the
// admission's 4096.
func TestGatedHoldRewritesGrowth(t *testing.T) {
	mine := dockertest.ID('a')
	b := gatedBed(t, gatedNeeds("g"), dockertest.WithContainer(dockertest.Container{ID: mine, Name: "mine", UsageBytes: 100 << 20, LimitBytes: 8 << 30}))
	touch := `curl -s -X POST --unix-socket "${DOCKER_HOST#unix://}" http://docker/containers/mine/restart; `
	done := b.start(t, "g", touch+waitGo)
	var admitted, first float64
	waitFor(t, 30*time.Second, "the first body", func() bool { admitted, _ = num(b.liveHold(t), "docker_sampled_ns"); return admitted > 0 })
	waitFor(t, 30*time.Second, "the first sample", func() bool { // it takes the touched container's baseline
		h := b.liveHold(t)
		first, _ = num(h, "docker_sampled_ns")
		growth, _ := num(h, "docker_growth_mib")
		return first > admitted && growth == 4096
	})
	b.fake.SetUsage(mine, (100+1024)<<20)
	b.vm(1, vmPID, 7168)
	// min(8192 − 7168, 2 × (2048 − 1024)) once a sample read both.
	waitFor(t, 30*time.Second, "a sample of the grown container and VM", func() bool {
		h := b.liveHold(t)
		fp, _ := num(h, "docker_footprint_mib")
		ns, _ := num(h, "docker_sampled_ns")
		growth, _ := num(h, "docker_growth_mib")
		return fp == 1024 && ns > first && growth == 1024
	})
	b.release(t)
	if r := <-done; r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
}

// A run that loses sight of its containers (its forwarder killed) drops its Docker need, growth and
// stamp from its hold: it no longer reserves for a gate it cannot keep. Mutation: setDockerKeys
// keeping the keys of a blind run.
func TestBlindDropsNeedAndGrowth(t *testing.T) {
	b := gatedBed(t, gatedNeeds("g"))
	done := b.start(t, "g", saveHost+waitGo)
	waitFor(t, 30*time.Second, "the command", func() bool { return b.file(t, "host") != "" })
	if _, ok := num(b.liveHold(t), "docker_need_mib"); !ok {
		t.Fatalf("hold %v is not gated", b.liveHold(t))
	}
	pid := forwarderPID(t, entryOf(t, b.file(t, "host")))
	if pid == 0 {
		t.Fatal("no forwarder")
	}
	if err := unix.Kill(pid, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the blind note", func() bool { return b.liveHold(t)["docker_note"] == docker.NoteBlind })
	h := b.liveHold(t)
	for _, key := range []string{"docker_need_mib", "docker_growth_mib", "docker_sampled_ns"} {
		if v, ok := h[key]; ok {
			t.Errorf("a blind hold carries %s %v", key, v)
		}
	}
	b.release(t)
	if r := <-done; r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
}

// A gated run whose forwarder does not start after its wait runs ungated: its hold drops the Docker
// keys it was published with and carries the start note. Mutation: setDockerKeys dropping them for a
// blind run only keeps them.
func TestForwarderStartFailureUngates(t *testing.T) {
	b := gatedBed(t, gatedNeeds("g"))
	r := b.run(t, "g", saveHost+saveHold, forwardSeamEnv+"=silent")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	h := b.hold(t)
	if h["docker_note"] != docker.NoteStart {
		t.Errorf("hold's docker_note %v, want %q", h["docker_note"], docker.NoteStart)
	}
	for _, key := range []string{"docker_need_mib", "docker_growth_mib", "docker_sampled_ns"} {
		if v, ok := h[key]; ok {
			t.Errorf("an ungated hold carries %s %v", key, v)
		}
	}
}

// A gated run that waits for room in Docker's VM says so with its Docker need, and with
// --on-timeout run starts at its timeout outside the budget, its Docker part ungated: no Docker key
// in its hold. Mutations: the waiting line without the VM reasons lacks "waits for room in Docker's
// VM"; a run not gated starts at once, with no timeout.
func TestTimeoutRunUngated(t *testing.T) {
	// 3 GiB used of 8: a room of 8192 − 3072 − 2048 = 3072 MiB, short of a 4 GiB need.
	b := gatedBed(t, "[needs.\"g\"]\ndocker_mem = \"4G\"\n",
		dockertest.WithContainer(dockertest.Container{ID: dockertest.ID('a'), Name: "big", UsageBytes: 3 << 30, LimitBytes: 8 << 30}))
	r := cli(t, b.env, "run", "--restart=yes", "--label", "g", "--mem", "64M", "--timeout", "3s", "--on-timeout", "run", "--", "sh", "-c", saveHold)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"docker_mem 4 GiB", "waits for room in Docker's VM", "--on-timeout run"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}
	for _, key := range []string{"docker_need_mib", "docker_growth_mib"} {
		if v, ok := b.hold(t)[key]; ok {
			t.Errorf("the timeout run's hold carries %s %v", key, v)
		}
	}
}

// A gated waiter short of room on the Mac never reads Docker: it opens no socket while it waits.
// Mutation: the VM test before the Mac test in the engine's attempt reads at every attempt.
func TestGatedWaiterReadsOnlyAtAttempts(t *testing.T) {
	b := gatedBed(t, gatedNeeds("g"))
	if r := b.run(t, "init", "true"); r.code != 0 { // the state directory's layout
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	holdUntil(t, b.dir, 1000) // of the 1024 MiB of headroom
	r := cli(t, b.env, "run", "--restart=yes", "--label", "g", "--mem", "512M", "--timeout", "3s", "--", "true")
	if r.code == 0 || !strings.Contains(r.stderr, "not admitted within") {
		t.Fatalf("exit %d, want a timeout: %s", r.code, r.stderr)
	}
	if n := b.fake.Conns(); n != 0 {
		t.Errorf("a waiter short of the Mac connected %d times", n)
	}
}

// readingsHook points the readings hook of this process at a file of the test's.
func readingsHook(t *testing.T) func(state, pid, footprintMiB int64) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CORESLOT_TESTHOOK_DIR", dir)
	return func(state, pid, footprintMiB int64) {
		body := []byte("DockerVMState " + itoa(state) + "\nDockerVMPID " + itoa(pid) + "\nDockerVMFootprintMiB " + itoa(footprintMiB) + "\n")
		path := filepath.Join(dir, testhooks.ReadingsFile)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		next := time.Now().Add(time.Duration(pid+state+footprintMiB) * time.Second) // a new mtime: the hook rereads
		if err := os.Chtimes(path, next, next); err != nil {
			t.Fatal(err)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// A sample that finds no VM, not running or not identified, keeps the hold's last growth, while
// its stamp moves: unknown is not zero. Mutation: a growth of 0 when no VM is found.
func TestGrowthKeptWhenVMMissing(t *testing.T) {
	set := readingsHook(t)
	set(1, vmPID, vmNowMiB)
	g := &dockerGate{needMiB: 2048, vMiB: 8192}
	g.sampled(0, 10)
	for _, state := range []int64{0, 2} {
		set(state, vmPID, vmNowMiB)
		g.sampled(512, 20+state)
		if growth, known, ns := g.last(); growth != 4096 || !known || ns != 20+state {
			t.Errorf("state %d: growth %d (known %v) at %d; want the last 4096 at %d", state, growth, known, ns, 20+state)
		}
	}
}

// After a Docker restart (another VM pid) the next sample finds the new VM and its footprint.
// Mutation: no lookup after a miss keeps the old VM's growth.
func TestHoldGrowthFollowsANewVM(t *testing.T) {
	set := readingsHook(t)
	set(1, vmPID, vmNowMiB)
	g := &dockerGate{needMiB: 2048, vMiB: 8192}
	g.sampled(0, 10)
	set(1, vmPID+1, 7168)
	g.sampled(0, 20)
	if growth, _, _ := g.last(); growth != 1024 { // min(8192 − 7168, 2 × 2048)
		t.Errorf("growth %d after the restart, want 1024 from the new VM", growth)
	}
	if g.vm.PID != vmPID+1 {
		t.Errorf("the gate follows pid %d, want %d", g.vm.PID, vmPID+1)
	}
}
