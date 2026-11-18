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

var fullID = strings.Repeat("0123456789abcdef", 4)

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
		dockertest.WithContainer(dockertest.Container{ID: fullID, Name: "db", Image: "postgres:16", Project: "shop",
			UsageBytes: 300 * mibBytes, LimitBytes: 2048 * mibBytes}),
	}, opts...)...)
}

// dockerObject runs status --docker --json and returns its docker object.
func dockerObject(t *testing.T, env []string) map[string]any {
	t.Helper()
	r := cli(t, env, "status", "--docker", "--json")
	if r.code != 0 {
		t.Fatalf("status --docker --json exited %d (stderr %q)", r.code, r.stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("status --docker --json: %v\n%s", err, r.stdout)
	}
	d, ok := doc["docker"].(map[string]any)
	if !ok {
		t.Fatalf("no docker object in %s", r.stdout)
	}
	return d
}

// Every key of the docker object and of its vm, always, whatever the state.
const (
	dockerKeys = "containers free_est_mib reason state vm"
	vmKeys     = "footprint_mib identified mem_mib"
)

// checkEmpty: the state, the reason and the null or empty cells of every state but ok.
func checkEmpty(t *testing.T, d map[string]any, state, reason string) {
	t.Helper()
	if got := keysOf(d); got != dockerKeys {
		t.Errorf("docker keys %s, want %s", got, dockerKeys)
	}
	vm, _ := d["vm"].(map[string]any)
	if got := keysOf(vm); got != vmKeys {
		t.Errorf("vm keys %s, want %s", got, vmKeys)
	}
	cs, isArray := d["containers"].([]any)
	if d["state"] != state || d["reason"] != reason || d["free_est_mib"] != nil || vm["mem_mib"] != nil ||
		vm["identified"] != false || vm["footprint_mib"] != nil || !isArray || len(cs) != 0 {
		t.Errorf("docker = %v, want state %q, reason %q and empty cells", d, state, reason)
	}
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
	js := cli(t, env, "status", "--json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(js.stdout), &doc); err != nil {
		t.Fatal(err)
	}
	if _, has := doc["docker"]; has {
		t.Errorf("status --json carries a docker key without --docker")
	}
	if n := f.Conns(); n != 0 {
		t.Fatalf("plain status opened %d connections to the engine, want 0", n)
	}
	cli(t, env, "status", "--docker")
	if f.Conns() == 0 {
		t.Errorf("status --docker opened no connection: the count does not see the child")
	}
}

func TestStatusDockerShowsVMAndContainers(t *testing.T) {
	dir := budgetDir(t, 512)
	env := dockerEnv(t, dir, dockerFake(t), 1, 4096)

	r := cli(t, env, "status", "--docker")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"docker:", "V 8 GiB", "footprint 4 GiB", "free_est 7.7 GiB", "estimate",
		"db", "postgres:16", "shop", "300 MiB", "2 GiB", fullID[:12]} {
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
	if c["id"] != fullID || c["name"] != "db" || c["image"] != "postgres:16" || c["project"] != "shop" ||
		c["usage_mib"] != float64(300) || c["limit_mib"] != float64(2048) {
		t.Errorf("container = %v", c)
	}
}

// No VM process: not_running with every key present, and nothing connected to.
func TestStatusDockerNoVMOpensNoSocket(t *testing.T) {
	dir := budgetDir(t, 512)
	f := dockerFake(t)
	checkEmpty(t, dockerObject(t, dockerEnv(t, dir, f, 0, 0)), "not_running", "")
	if n := f.Conns(); n != 0 {
		t.Errorf("opened %d connections with no VM, want 0", n)
	}
}

func TestStatusDockerVMNotIdentifiedStillLists(t *testing.T) {
	dir := budgetDir(t, 512)
	env := dockerEnv(t, dir, dockerFake(t), 2, 0)
	d := dockerObject(t, env)
	vm := d["vm"].(map[string]any)
	if d["state"] != "ok" || vm["identified"] != false || vm["footprint_mib"] != nil || vm["mem_mib"] != float64(8192) ||
		len(d["containers"].([]any)) != 1 {
		t.Errorf("docker = %v, want ok, not identified, no footprint, one container", d)
	}
	if r := cli(t, env, "status", "--docker"); !strings.Contains(r.stdout, "footprint not known") {
		t.Errorf("text does not say the footprint is not known:\n%s", r.stdout)
	}
}

func TestStatusDockerOtherEngine(t *testing.T) {
	dir := budgetDir(t, 512)
	f := dockerFake(t, dockertest.WithInfo("Ubuntu 24.04", 8<<30))
	checkEmpty(t, dockerObject(t, dockerEnv(t, dir, f, 1, 4096)), "other_engine", "")
}

func TestStatusDockerNoReading(t *testing.T) {
	dir := budgetDir(t, 512)
	checkEmpty(t, dockerObject(t, dockerEnv(t, dir, dockerFake(t, dockertest.Hang()), 1, 4096)), "no_reading", "deadline")
	env := dockerEnv(t, dir, dockerFake(t, dockertest.StatsStatus(fullID, 500)), 1, 4096)
	if r := cli(t, env, "status", "--docker"); !strings.Contains(r.stdout, "no reading") || !strings.Contains(r.stdout, "http") {
		t.Errorf("text does not say no reading and why:\n%s", r.stdout)
	}
}

// Each way the features are off, and the no-socket rule, with no connection made. Mutations:
// reading before the features are decided connects in every row; mapping no-socket to off turns
// its row red.
func TestStatusDockerOffReasons(t *testing.T) {
	cases := []struct {
		name, state, reason string
		cfg                 string
		hook                bool
		extra               []string
	}{
		{"config", "off", "config", "[docker]\nenabled = false\n", true, nil},
		{"nested", "off", "nested", "", true, []string{"CORESLOT_UNBUDGETED=parent"}},
		{"state-dir", "off", "state-dir", "", false, nil},
		{"remote-host", "off", "remote-host", "", true, []string{"DOCKER_HOST=tcp://127.0.0.1:1"}},
		{"no-socket", "not_running", "", "", true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := stateDir(t, "[memory]\nreserve = \"0\"\n"+c.cfg)
			budgetRoom.Store(dir, int64(512))
			f := dockerFake(t)
			var env []string
			switch {
			case c.name == "no-socket": // the hook names a path that is no socket
				env = dockerEnv(t, dir, nil, 1, 4096, "CORESLOT_TESTHOOK_DOCKER_SOCKET="+filepath.Join(t.TempDir(), "absent.sock"))
			case c.hook:
				env = dockerEnv(t, dir, f, 1, 4096, c.extra...)
			default:
				env = dockerEnv(t, dir, nil, 1, 4096, c.extra...)
			}
			checkEmpty(t, dockerObject(t, env), c.state, c.reason)
			if f.Conns() != 0 {
				t.Errorf("opened %d connections, want 0", f.Conns())
			}
		})
	}
}

// Engine strings are shown as labels are: wire.EscapeHuman in the text, the labels' encoder in
// JSON, each cut to 128 bytes on a rune boundary. Mutations: no cut leaves 300 bytes; a cut at byte
// 128 splits the rune; skipping text() leaves the raw ESC in the text.
func TestStatusDockerEscapesEngineStrings(t *testing.T) {
	dir := budgetDir(t, 512)
	image := strings.Repeat("a", 127) + "é" + strings.Repeat("b", 170) // the rune spans bytes 127 and 128
	name := "x\x1b[31mred\u202e\xff" + strings.Repeat("n", 300)        // the invalid byte reaches the status as U+FFFD
	f := dockertest.Engine(t, dockertest.WithContainer(dockertest.Container{ID: fullID, Name: name, Image: image,
		Project: "p\x1b[0m", UsageBytes: 10 * mibBytes, LimitBytes: 100 * mibBytes}))
	env := dockerEnv(t, dir, f, 1, 4096)

	wantImage := strings.Repeat("a", 127)
	wantName := ("x\x1b[31mred\u202e\ufffd" + strings.Repeat("n", 300))[:128]
	r := cli(t, env, "status", "--docker")
	for _, want := range []string{wire.EscapeHuman(wantImage), wire.EscapeHuman(wantName), wire.EscapeHuman("p\x1b[0m")} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("text lacks %q:\n%s", want, r.stdout)
		}
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
	if c := doc.Docker.Containers[0]; c.Image != wantImage || c.Name != wantName || c.Project != "p\x1b[0m" || c.ID != fullID {
		t.Errorf("JSON container = %+v, want image %q and name %q", c, wantImage, wantName)
	}
}
