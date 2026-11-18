//go:build coreslot_testhooks && darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/docker/dockertest"
	"github.com/core-slot/coreslot/internal/wire"
)

const mibBytes = 1 << 20

// dockerEnv is the environment of a status run against the fake engine (nil for no hook), with the
// VM the readings file names: state 0 none, 1 identified, 2 not identified.
func dockerEnv(t *testing.T, dir string, f *dockertest.Fake, vmState, footprintMiB int, extra ...string) []string {
	t.Helper()
	hookDir := t.TempDir()
	readings := fmt.Sprintf("DockerVMState %d\nDockerVMFootprintMiB %d\n", vmState, footprintMiB)
	if err := os.WriteFile(filepath.Join(hookDir, "readings"), []byte(readings), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(baseEnv(dir), "CORESLOT_TESTHOOK_DIR="+hookDir)
	if f != nil {
		env = append(env, "CORESLOT_TESTHOOK_DOCKER_SOCKET="+f.Sock)
	}
	return append(env, extra...)
}

func dockerFake(t *testing.T, opts ...dockertest.Option) *dockertest.Fake {
	t.Helper()
	return dockertest.Engine(t, append([]dockertest.Option{
		dockertest.WithContainer(dockertest.Container{ID: dockertest.LongID, Name: "db", Image: "postgres:16", Project: "shop",
			UsageBytes: 300 * mibBytes, LimitBytes: 2048 * mibBytes}),
	}, opts...)...)
}

// Plain status opens no socket and says how to ask; --docker does, which proves the count sees
// the child's connections. Mutations: reading the engine without the flag leaves Conns above 0
// there; a count that misses the child leaves it 0 with the flag.
func TestStatusWithoutDockerOpensNoSocket(t *testing.T) {
	dir := budgetDir(t, 512)
	f := dockerFake(t)
	env := dockerEnv(t, dir, f, 1, 4096)
	text := cli(t, env, "status")
	if text.code != 0 || !strings.Contains(text.stdout, "--docker") {
		t.Errorf("status (exit %d) lacks the line naming --docker:\n%s", text.code, text.stdout)
	}
	cli(t, env, "status", "--json") // its docker key's absence is TestStatusDockerOffWithoutHook's
	if n := f.Conns(); n != 0 {
		t.Fatalf("plain status opened %d connections to the engine, want 0", n)
	}
	cli(t, env, "status", "--docker")
	if f.Conns() == 0 {
		t.Errorf("status --docker opened no connection: the count does not see the child")
	}
}

// The text and the JSON of the ok state: the VM's size and footprint, free_est as V less the usage, and
// every container with its keys. Mutations: free_est without the usage subtracted; the footprint
// shown as V.
func TestStatusDockerShowsVMAndContainers(t *testing.T) {
	dir := budgetDir(t, 512)
	env := dockerEnv(t, dir, dockerFake(t), 1, 4096)

	r := cli(t, env, "status", "--docker")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"docker:", "V 8 GiB", "footprint 4 GiB", "free_est 7.7 GiB", "estimate",
		"db", "postgres:16", "shop", "300 MiB", "2 GiB", dockertest.LongID[:12]} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("text lacks %q:\n%s", want, r.stdout)
		}
	}

	d := dockerObject(t, env)
	if got := keysOf(d); got != dockerKeys {
		t.Errorf("docker keys %s, want %s", got, dockerKeys)
	}
	vm := d["vm"].(map[string]any)
	if d["state"] != "ok" || d["reason"] != "" || d["free_est_mib"] != float64(8192-300) ||
		vm["mem_mib"] != float64(8192) || vm["identified"] != true || vm["footprint_mib"] != float64(4096) {
		t.Errorf("docker = %v", d)
	}
	cs := d["containers"].([]any)
	if len(cs) != 1 {
		t.Fatalf("containers = %v", cs)
	}
	c := cs[0].(map[string]any)
	if got := keysOf(c); got != "id image limit_mib name project usage_mib" {
		t.Errorf("container keys %s", got)
	}
	if c["id"] != dockertest.LongID || c["name"] != "db" || c["image"] != "postgres:16" || c["project"] != "shop" ||
		c["usage_mib"] != float64(300) || c["limit_mib"] != float64(2048) {
		t.Errorf("container = %v", c)
	}
}

// No VM process: not_running with every key present, nothing connected to, and the text names the
// hypervisor and the engines that are not read. Mutation: the old "no Docker Desktop VM was found"
// text lacks the first two words checked.
func TestStatusDockerNoVMOpensNoSocket(t *testing.T) {
	dir := budgetDir(t, 512)
	f := dockerFake(t)
	env := dockerEnv(t, dir, f, 0, 0)
	checkEmpty(t, dockerObject(t, env), "not_running", "")
	if r := cli(t, env, "status", "--docker"); !strings.Contains(r.stdout, "Apple hypervisor") || !strings.Contains(r.stdout, "Docker VMM, OrbStack and Colima are not read") {
		t.Errorf("text does not say what is read:\n%s", r.stdout)
	}
	if n := f.Conns(); n != 0 {
		t.Errorf("opened %d connections with no VM, want 0", n)
	}
}

// A VM that cannot be identified still lists the containers, with no footprint. Mutation: taking
// "not identified" for "no VM" answers not_running.
func TestStatusDockerVMNotIdentifiedStillLists(t *testing.T) {
	dir := budgetDir(t, 512)
	env := dockerEnv(t, dir, dockerFake(t), 2, 0)
	d := dockerObject(t, env)
	vm := d["vm"].(map[string]any)
	if d["state"] != "ok" || vm["identified"] != false || vm["footprint_mib"] != nil || vm["mem_mib"] != float64(8192) ||
		len(d["containers"].([]any)) != 1 {
		t.Errorf("docker = %v, want ok, not identified, no footprint, one container", d)
	}
	if r := cli(t, env, "status", "--docker"); !strings.Contains(r.stdout, "VM not identified") {
		t.Errorf("text does not say the VM is not identified:\n%s", r.stdout)
	}
}

// Another engine is its own state with no figures. Mutation: dropping the other-engine case reads
// its containers as Docker Desktop's and answers ok.
func TestStatusDockerOtherEngine(t *testing.T) {
	dir := budgetDir(t, 512)
	f := dockerFake(t, dockertest.WithInfo("Ubuntu 24.04", 8<<30))
	checkEmpty(t, dockerObject(t, dockerEnv(t, dir, f, 1, 4096)), "other_engine", "")
}

// Each failed call is no reading with its code, in the text and the JSON. Mutations: dropping the
// code from the view leaves the deadline row with an empty reason; printing the state with no code
// leaves the text without the exact line.
func TestStatusDockerNoReading(t *testing.T) {
	dir := budgetDir(t, 512)
	checkEmpty(t, dockerObject(t, dockerEnv(t, dir, dockerFake(t, dockertest.Hang()), 1, 4096)), "no_reading", "deadline")
	env := dockerEnv(t, dir, dockerFake(t, dockertest.StatsStatus(dockertest.LongID, 500)), 1, 4096)
	if r := cli(t, env, "status", "--docker"); !strings.Contains(r.stdout, "\ndocker: no reading (http)\n") {
		t.Errorf("text lacks the line \"docker: no reading (http)\":\n%s", r.stdout)
	}
}

// Each way the features are off, with no connection made. Every row but remote-host names the fake
// in DOCKER_HOST, so a reading made first connects to it. Mutations: reading first, from the sockets
// DOCKER_HOST names, reddens those rows; passing Features no inherited context reddens both nested
// rows (a live ancestor's hold is the one a run inside `coreslot run` carries).
func TestStatusDockerOffReasons(t *testing.T) {
	cases := []struct {
		name, state, reason string
		cfg                 string
		hook, hold          bool
		extra               []string
	}{
		{"config", "off", "config", "[docker]\nenabled = false\n", true, false, nil},
		{"nested unbudgeted", "off", "nested", "", true, false, []string{"CORESLOT_UNBUDGETED=parent"}},
		{"nested hold", "off", "nested", "", true, true, nil},
		{"state-dir", "off", "state-dir", "", false, false, nil},
		{"remote-host", "off", "remote-host", "", true, false, []string{"DOCKER_HOST=tcp://127.0.0.1:1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := stateDir(t, "[memory]\nreserve = \"0\"\n"+c.cfg)
			budgetRoom.Store(dir, int64(512))
			f := dockerFake(t)
			extra := append([]string{"DOCKER_HOST=unix://" + f.Sock}, c.extra...)
			if c.hold {
				extra = append(extra, "CORESLOT_HOLD="+liveHold(t, dir, true))
			}
			var fake *dockertest.Fake
			if c.hook {
				fake = f
			}
			checkEmpty(t, dockerObject(t, dockerEnv(t, dir, fake, 1, 4096, extra...)), c.state, c.reason)
			if f.Conns() != 0 {
				t.Errorf("opened %d connections, want 0", f.Conns())
			}
		})
	}
}

// A VM that no socket answers for is no reading with no_socket, identified or not, and the text says
// the VM runs and where no socket was found: DOCKER_HOST when it is set, else the candidate paths.
// With no VM it is not_running, whatever the sockets. Mutations: no_socket mapped to not_running
// reddens the first three rows and the text; reading before looking for the VM reddens the last.
func TestStatusDockerNoSocket(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.sock")
	for _, c := range []struct {
		name         string
		vm           int
		host         string
		state, where string
	}{
		{"identified VM, candidate path", 1, "", "no_reading", absent},
		{"VM not identified, candidate path", 2, "", "no_reading", absent},
		{"identified VM, DOCKER_HOST", 1, "unix://" + absent, "no_reading", "DOCKER_HOST"},
		{"no VM", 0, "", "not_running", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := budgetDir(t, 512)
			extra := []string{"CORESLOT_TESTHOOK_DOCKER_SOCKET=" + absent}
			if c.host != "" {
				extra = append(extra, "DOCKER_HOST="+c.host)
			}
			env := dockerEnv(t, dir, nil, c.vm, 4096, extra...)
			reason := "no_socket"
			if c.state == "not_running" {
				reason = ""
			}
			checkEmpty(t, dockerObject(t, env), c.state, reason)
			text := cli(t, env, "status", "--docker").stdout
			if c.where != "" && (!strings.Contains(text, "Docker's VM runs but no Docker socket was found at "+c.where) || strings.Contains(text, "no Docker Desktop VM")) {
				t.Errorf("text does not say the VM runs and where no socket was found:\n%s", text)
			}
		})
	}
}

// Engine strings are shown as labels are: wire.EscapeHuman in the text, the labels' encoder in
// JSON, each cut to 128 bytes on a rune boundary. The fake sends the name's invalid byte raw, so the
// replacement is the client's, and the text is one exact container line, so a prefix does not pass.
// Mutations: no cut, or a cut at byte 128 splitting the rune, reddens the line and the JSON; skipping
// text() leaves the raw ESC in the text.
func TestStatusDockerEscapesEngineStrings(t *testing.T) {
	dir := budgetDir(t, 512)
	image := strings.Repeat("a", 127) + "é" + strings.Repeat("b", 170) // the rune spans bytes 127 and 128
	name := "x\x1b[31mred\u202e\xff" + strings.Repeat("n", 300)        // the invalid byte reaches the status as U+FFFD
	f := dockertest.Engine(t, dockertest.WithContainer(dockertest.Container{ID: dockertest.LongID, Name: name, Image: image,
		Project: "p\x1b[0m", UsageBytes: 10 * mibBytes, LimitBytes: 100 * mibBytes}))
	env := dockerEnv(t, dir, f, 1, 4096)

	wantImage := strings.Repeat("a", 127)
	wantName := ("x\x1b[31mred\u202e\ufffd" + strings.Repeat("n", 300))[:128]
	r := cli(t, env, "status", "--docker")
	line := fmt.Sprintf("  %s  %s  %s  project %s  usage 10 MiB of 100 MiB\n", dockertest.LongID[:12], wire.EscapeHuman(wantName), wire.EscapeHuman(wantImage), wire.EscapeHuman("p\x1b[0m"))
	if !strings.Contains(r.stdout, line) {
		t.Errorf("text lacks the line %q:\n%s", line, r.stdout)
	}
	if strings.ContainsAny(r.stdout, "\x1b\u202e") {
		t.Errorf("text carries a raw control character: %q", r.stdout)
	}

	js := cli(t, env, "status", "--docker", "--json")
	if strings.ContainsAny(js.stdout, "\x1b\u202e") {
		t.Errorf("JSON carries a raw control character")
	}
	var doc struct {
		Docker struct {
			Containers []struct{ ID, Name, Image, Project string }
		}
	}
	if err := json.Unmarshal([]byte(js.stdout), &doc); err != nil || len(doc.Docker.Containers) != 1 {
		t.Fatalf("%v\n%s", err, js.stdout)
	}
	if c := doc.Docker.Containers[0]; c.Image != wantImage || c.Name != wantName || c.Project != "p\x1b[0m" || c.ID != dockertest.LongID {
		t.Errorf("JSON container = %+v, want image %q and name %q", c, wantImage, wantName)
	}
}
