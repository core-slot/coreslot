//go:build coreslot_testhooks && darwin

package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
	"github.com/core-slot/coreslot/internal/store"
)

// The Docker traces run gated runs through the real CLI against the fake engine as Docker Desktop's
// socket, in a short state directory. Docker's VM is the fake's (8 GiB unless a trace says, so a
// Docker reserve of 2 GiB; 16 GiB gives 3 GiB), its process on the Mac comes from the readings file,
// and a run is gated by its label's docker_mem in the needs file. A container that no run touched
// fills the VM: its usage is what the VM's room lacks.

const vmPID = 4242 // the VM's process by the readings file; another pid is a Docker restart

// otherID is the container no run touched.
var otherID = dockertest.ID('e')

// other is that container, using usageMiB of the VM.
func other(usageMiB int64) dockertest.Option {
	return dockertest.WithContainer(dockertest.Container{ID: otherID, Name: "other", UsageBytes: uint64(usageMiB) << 20, LimitBytes: 64 << 30})
}

// vm16 is a VM of 16 GiB.
var vm16 = dockertest.WithInfo(docker.DesktopOS, 16<<30)

// withVM is the readings r of a machine whose Docker VM is identified and holds footprintMiB on the
// Mac; vm those of a calm one; noVM those of a calm one whose VM does not run, as when Docker is
// quit: no reading succeeds.
func withVM(r map[string]int64, footprintMiB int64) map[string]int64 {
	r["DockerVMState"], r["DockerVMPID"], r["DockerVMFootprintMiB"] = 1, vmPID, footprintMiB
	return r
}

func vm(footprintMiB int64) map[string]int64 { return withVM(calm(0), footprintMiB) }

func noVM() map[string]int64 {
	r := calm(0)
	r["DockerVMState"] = 0
	return r
}

// dockerEnv is a short state directory with the configuration cfg, a needs file giving each label of
// dockerMem its Docker need, the readings r and the fake engine of opts.
func dockerEnv(t *testing.T, cfg string, dockerMem map[string]string, r map[string]int64, opts ...dockertest.Option) (Env, *dockertest.Fake) {
	t.Helper()
	e := DockerState(t)
	e.WriteConfig(cfg)
	var needs strings.Builder
	for label, size := range dockerMem {
		needs.WriteString("[needs.\"" + label + "\"]\ndocker_mem = \"" + size + "\"\n")
	}
	if err := os.WriteFile(filepath.Join(e.Dir, "needs.toml"), []byte(needs.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := e.SetEngine(opts...)
	e.SetReadings(r)
	return e, fake
}

// waitNoted waits until the ticket labelled label has its note: it fits on the Mac and waits for
// Docker's VM.
func (e Env) waitNoted(label string) {
	e.t.Helper()
	e.Eventually(startWait, label+" has its note", func() bool { tk, ok := e.Ticket(label); return ok && tk.Noted })
}

// waiting fails the test unless the run labelled label still waits: its ticket queued, no hold.
func waiting(t *testing.T, e Env, label, why string) {
	t.Helper()
	noHold(t, e, label, why)
	if _, ok := e.Ticket(label); !ok {
		t.Fatalf("%s's ticket is gone: %s", label, why)
	}
}

// waitUngated waits until the run labelled label holds with its Docker part ungated after its
// failed readings: its hold carries the note and no Docker need.
func (e Env) waitUngated(label string) {
	e.t.Helper()
	e.Eventually(startWait, label+" holds with its Docker part ungated", func() bool {
		h, ok := e.Hold(label)
		return ok && h.DockerNote == "ungated: no-reading" && h.DockerNeedMiB == 0
	})
}

// finish lets each run's command exit 0 and waits for its run.
func (e Env) finish(runs ...*Proc) {
	e.t.Helper()
	for _, p := range runs {
		e.ReleaseChild(p.name)
	}
	for _, p := range runs {
		p.ExitCode(0)
	}
}

// With no hold live, a gated head whose VM test fails publishes its note, and the host run behind it
// starts by the idle rule: its re-election passes over the noted ticket. The head starts once the
// VM has room. Mutation: the re-election not passing over a noted ticket keeps H behind G.
func TestTraceGatedHeadNoteIdleRule(t *testing.T) {
	// The VM's room: 8192 − 3072 − 2048 = 3072 MiB, short of G's 4 GiB.
	e, fake := dockerEnv(t, roomConfig(1024), map[string]string{"G": "4G"}, vm(8192), other(3072))
	g := e.Run("G", "--mem", "128M")
	e.waitNoted("G")
	h := e.Run("H", "--mem", "128M")
	e.WaitStarted("H")
	waiting(t, e, "G", "G was admitted short of VM room")
	fake.SetUsage(otherID, 0)
	e.WaitStarted("G")
	e.finish(g, h)
}

// Docker quit (no VM, so no reading) with no hold live: the gated head counts its failed readings
// with its note, the host run behind it starts at once, and after eight failed readings the head
// starts with its Docker part ungated. Mutation: a failed reading publishing no note keeps H
// behind G until G goes ungated.
func TestTraceQuitEngineCounts(t *testing.T) {
	e, _ := dockerEnv(t, roomConfig(1024), map[string]string{"G": "2G"}, noVM())
	g := e.Run("G", "--mem", "128M")
	e.waitNoted("G")
	h := e.Run("H", "--mem", "128M")
	e.WaitStarted("H")
	waiting(t, e, "G", "G started before H, behind it, did")
	e.waitUngated("G")
	e.finish(g, h)
	if !strings.Contains(g.Stderr(), "ungated: no-reading") {
		t.Errorf("G did not say it starts ungated:\n%s", g.Stderr())
	}
}

// A gated container request starts ahead of a host head that waits for room on the Mac when it
// passes its VM test, on a fresh VM (its hold reserves twice its need) and on a grown one (it
// reserves nothing), and when its first reading fails it notes and reads again instead of waiting
// behind the head. Mutation: a gated container request taking the re-election keeps D behind B.
func TestTraceGatedContainerAheadOfHostHead(t *testing.T) {
	for _, c := range []struct {
		name      string
		first     map[string]int64 // the readings D first sees
		growthMiB int64            // what D's hold reserves: min(8192 − footprint, 2 × 2048)
	}{
		{"fresh VM", vm(2048), 4096},
		{"grown VM", vm(8192), 0},
		{"first reading fails", noVM(), 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _ := dockerEnv(t, roomConfig(1024), map[string]string{"D": "2G"}, c.first)
			a := e.Run("A", "--mem", "768M")
			e.WaitStarted("A")
			b := e.Run("B", "--mem", "512M")
			e.WaitTicket("B")
			d := e.runDocker("D")
			if c.first["DockerVMState"] == 0 {
				e.waitNoted("D")
				e.SetReadings(vm(8192))
			}
			e.WaitStarted("D")
			waiting(t, e, "B", "B was admitted while A holds its room")
			if h, _ := e.Hold("D"); h.DockerNeedMiB != 2048 || h.DockerGrowthMiB != c.growthMiB {
				t.Errorf("D's hold: docker_need_mib %d, docker_growth_mib %d; want 2048 and %d", h.DockerNeedMiB, h.DockerGrowthMiB, c.growthMiB)
			}
			e.finish(a, d)
			e.WaitStarted("B")
			e.finish(b)
		})
	}
}

// Docker quit while a hold is live: the gated head and the gated run behind it both note and count
// their failed readings at once, the host run behind them starts at once, and both start ungated.
// Mutation: a gated run not passing over a noted ticket in its election never notes while G counts.
func TestTraceQuitWhileHoldLive(t *testing.T) {
	e, _ := dockerEnv(t, roomConfig(1024), map[string]string{"G": "2G", "G2": "2G"}, noVM())
	a := e.Run("A", "--mem", "128M")
	e.WaitStarted("A")
	g := e.Run("G", "--mem", "128M")
	e.waitNoted("G")
	g2 := e.Run("G2", "--mem", "128M")
	e.waitNoted("G2")
	waiting(t, e, "G", "G started before G2 noted: they did not count at once")
	h := e.Run("H", "--mem", "128M")
	e.WaitStarted("H")
	waiting(t, e, "G", "G started before H, behind it, did")
	e.waitUngated("G")
	e.waitUngated("G2")
	e.finish(a, g, g2, h)
}

// A gated head short of its own Mac need has no note and reads nothing; a host run without a
// typical time waits behind it, and one with a typical time passes by the head's wait, computed
// from its Mac need alone. Mutation: the head's wait computed from its Docker need (2 GiB, which
// no hold's end frees) keeps H2 waiting.
func TestTraceShortOfMacNoNote(t *testing.T) {
	e, fake := dockerEnv(t, roomConfig(1024), map[string]string{"G": "2G"}, vm(8192))
	for label, ranS := range map[string]float64{"A": 3600, "H2": 600} {
		if err := e.Engine().Store().AppendRun(store.RunRecord{TimeNS: store.NowNS(), Label: label, RanS: ranS}); err != nil {
			t.Fatal(err)
		}
	}
	a := e.Run("A", "--mem", "768M")
	e.WaitStarted("A")
	g := e.Run("G", "--mem", "512M")
	e.WaitTicket("G")
	h1 := e.Run("H1", "--mem", "128M")
	e.WaitTicket("H1")
	h2 := e.Run("H2", "--mem", "128M")
	e.WaitStarted("H2")
	time.Sleep(observeWait)
	waiting(t, e, "H1", "H1, with no typical time, passed the head")
	if tk, _ := e.Ticket("G"); tk.Noted {
		t.Error("G, short of the Mac, has a note")
	}
	if n := fake.Conns(); n != 0 {
		t.Errorf("G, short of the Mac, read Docker: %d connections", n)
	}
	e.finish(a)
	e.WaitStarted("G")
	e.WaitStarted("H1")
	e.finish(g, h1, h2)
}

// A gated restart still waiting for calm fails its Mac test: no note, and it stays at the front. Once
// its conditions are met and it lacks only VM room it has its note, and the host run behind it
// starts. Mutation: a failed VM test publishing no note keeps H behind B.
func TestTraceRestartLacksOnlyVMRoom(t *testing.T) {
	e, fake := dockerEnv(t, watchConfig(""), map[string]string{"B": "2G"}, vm(8192), other(0))
	e.Learn("B")
	a := e.Run("A", "--mem", "100M")
	e.WaitStarted("A")
	e.WaitTicked("A")
	b := e.Run("B", "--mem", "100M")
	kid := e.WaitStarted("B")
	if h, _ := e.Hold("B"); h.DockerNeedMiB != 2048 {
		t.Fatalf("B's first attempt is not gated: docker_need_mib %d", h.DockerNeedMiB)
	}
	e.WaitTicked("B")
	fake.SetUsage(otherID, 7<<30) // the VM's room: 8192 − 7168 − 2048 < 0
	e.SetReadings(withVM(floor(1024), 8192))
	e.Eventually(startWait, "B's command is killed", func() bool { return !alive(kid.PID) || b.Exited() })
	if err := os.Remove(filepath.Join(e.MarkDir, "B.started")); err != nil {
		t.Fatal(err)
	}
	e.WaitTicket("B")
	h := e.Run("H", "--mem", "100M")
	e.WaitTicket("H")
	time.Sleep(observeWait)
	if tk := e.Status().Tickets; len(tk) != 2 || tk[0].Label != "B" || tk[0].Noted {
		t.Fatalf("queue %+v, want B at the front without a note while it waits for calm", tk)
	}
	e.SetReadings(withVM(calm(2048), 8192))
	e.waitNoted("B")
	e.WaitStarted("H")
	waiting(t, e, "B", "B restarted short of VM room")
	fake.SetUsage(otherID, 0)
	e.WaitStarted("B")
	e.finish(a, b, h)
}

// A small gated container request behind a large gated ticket starts only when the VM holds both:
// it leaves the large one its VM room whether that one waits for the VM (noted) or for the Mac.
// Mutation: the VM test not summing the gated tickets ahead starts S while the VM holds S alone.
func TestTraceSmallBesideLarge(t *testing.T) {
	// A 16 GiB VM with 8 GiB used: room 16384 − 8192 − 3072 = 5120 MiB holds S (1 GiB) alone, not L
	// (6 GiB); with nothing used, 13312 MiB holds both.
	needs := map[string]string{"L": "6G", "S": "1G"}
	t.Run("L waits for the VM", func(t *testing.T) {
		e, fake := dockerEnv(t, roomConfig(1024), needs, vm(16384), vm16, other(8192))
		l := e.Run("L", "--mem", "128M")
		e.waitNoted("L")
		s := e.runDocker("S")
		e.waitNoted("S")
		time.Sleep(observeWait)
		waiting(t, e, "S", "S took VM room that L, ahead of it, waits for")
		fake.SetUsage(otherID, 0)
		e.WaitStarted("L")
		e.WaitStarted("S")
		e.finish(l, s)
	})
	t.Run("L waits for the Mac", func(t *testing.T) {
		e, fake := dockerEnv(t, roomConfig(1024), needs, vm(16384), vm16, other(8192))
		a := e.Run("A", "--mem", "768M")
		e.WaitStarted("A")
		l := e.Run("L", "--mem", "512M")
		e.WaitTicket("L")
		s := e.runDocker("S")
		e.waitNoted("S")
		time.Sleep(observeWait)
		waiting(t, e, "S", "S took VM room that L, ahead of it, waits for")
		fake.SetUsage(otherID, 0)
		e.WaitStarted("S")
		waiting(t, e, "L", "L was admitted while A holds its Mac room")
		if tk, _ := e.Ticket("L"); tk.Noted {
			t.Error("L, short of the Mac, has a note")
		}
		e.finish(a, s)
		e.WaitStarted("L")
		e.finish(l)
	})
}

// Five gated shards started one after another take VM room in arrival order: the first two fit, the
// third does not and notes, the small fourth does not take the room the third waits for, and the
// host run behind the noted shards starts. With more room the small fourth starts beside the third,
// and the fifth starts once the first ends. Mutation: the VM test not summing the gated tickets
// ahead starts S4 before S3.
func TestTraceGatedShardsInArrivalOrder(t *testing.T) {
	// A 16 GiB VM with 3 GiB used: room 16384 − 3072 − 3072 = 10240 MiB.
	e, fake := dockerEnv(t, roomConfig(4096), map[string]string{"S1": "4G", "S2": "4G", "S3": "4G", "S4": "1G", "S5": "4G"},
		vm(16384), vm16, other(3072))
	var shards []*Proc
	for _, label := range []string{"S1", "S2", "S3", "S4", "S5"} {
		shards = append(shards, e.Run(label, "--mem", "128M"))
		e.Eventually(startWait, label+" is queued or admitted", func() bool {
			_, queued := e.Ticket(label)
			_, held := e.Hold(label)
			return queued || held
		})
	}
	e.WaitStarted("S1")
	e.WaitStarted("S2")
	for _, label := range []string{"S3", "S4", "S5"} {
		e.waitNoted(label)
	}
	h := e.Run("H", "--mem", "128M")
	e.WaitStarted("H")
	for _, label := range []string{"S3", "S4", "S5"} {
		waiting(t, e, label, label+" took VM room ahead of its turn")
	}
	fake.SetUsage(otherID, 0) // 13312 − 8192 = 5120 MiB: S3 and S4, not S5
	e.WaitStarted("S3")
	e.WaitStarted("S4")
	time.Sleep(observeWait)
	waiting(t, e, "S5", "S5 started with no VM room left")
	e.finish(shards[0])
	e.WaitStarted("S5")
	e.finish(append(shards[1:], h)...)
}
