//go:build coreslot_testhooks && darwin

package conformance

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/docker/dockertest"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// sampledNS is the docker_sampled_ns of the hold named name, 0 when its body cannot be read (gone,
// or torn by its run's rewrite in place).
func (e Env) sampledNS(name string) int64 {
	data, err := os.ReadFile(filepath.Join(e.Dir, store.HoldsDir, name))
	if err != nil {
		return 0
	}
	h, err := wire.DecodeHold(data)
	if err != nil {
		return 0
	}
	return h.DockerSampledNS
}

// A gated container request's hold reserves on the Mac the VM growth its containers may cause, here
// the whole room: a second gated container request leaves that room and waits until the first ends,
// while an ungated one, as `docker compose down`, is judged on real memory alone and starts.
// Mutation: a gated container request's room without the holds' growth admits C2 beside C1.
func TestTraceThreeGatedContainerRequests(t *testing.T) {
	// A fresh VM: C1 reserves min(8192 − 2048, 2 × 2048) = 4096 MiB, the whole room.
	e, _ := dockerEnv(t, roomConfig(4096), map[string]string{"C1": "2G", "C2": "2G"}, vm(2048))
	c1 := e.runDocker("C1")
	e.WaitStarted("C1")
	if h, _ := e.Hold("C1"); h.DockerGrowthMiB != 4096 {
		t.Fatalf("C1's hold reserves %d MiB of growth, want 4096", h.DockerGrowthMiB)
	}
	c2 := e.runDocker("C2")
	e.WaitTicket("C2")
	down := e.runDocker("down")
	e.WaitStarted("down")
	time.Sleep(observeWait)
	waiting(t, e, "C2", "C2 took the room C1's growth reserves")
	if tk, _ := e.Ticket("C2"); tk.Noted {
		t.Error("C2, short of the Mac, has a note")
	}
	e.finish(c1)
	e.WaitStarted("C2")
	e.finish(c2, down)
}

// A gated hold's reservation lowers the room every other client computes: by twice its unfilled need
// on a fresh VM, less as its container grows, nothing once the VM's footprint reaches its size, and
// more again when the footprint falls; after Docker restarts on Docker VMM, by 3/2 of its unfilled
// need. Mutations: a room that leaves the holds' growth out stays at the need's term alone; a
// factor that does not follow the VM's backend keeps 2048 on VMM.
func TestTraceReservationLowersHostRoom(t *testing.T) {
	const roomMiB = 8192
	mine := dockertest.ID('a')
	e, fake := dockerEnv(t, roomConfig(roomMiB), map[string]string{"G": "2G"}, vm(2048),
		dockertest.WithContainer(dockertest.Container{ID: mine, Name: "mine", UsageBytes: 100 << 20, LimitBytes: 8 << 30}))
	if r := e.Status(); r.Mem.RoomMiB != roomMiB {
		t.Fatalf("room %d MiB before G, want %d", r.Mem.RoomMiB, roomMiB)
	}
	// G's command touches the container through its forwarder, then holds.
	touch := `curl -s -X POST --unix-socket "${DOCKER_HOST#unix://}" http://docker/containers/mine/restart >/dev/null; exec "$0" hold "$1" G`
	g := e.Start("run", "--restart=yes", "--label", "G", "--mem", "64M", "--", "sh", "-c", touch, bins.child, e.MarkDir)
	g.name = "G"
	e.WaitStarted("G")
	name := e.WaitHold("G", startWait).Name
	reserves := func(growthMiB int64) {
		t.Helper()
		e.Eventually(startWait, fmt.Sprintf("G reserves %d MiB of growth off the room", growthMiB), func() bool {
			r := e.Status()
			for _, h := range r.Holds {
				if h.Label == "G" {
					return h.DockerGrowthMiB == growthMiB && r.Mem.RoomMiB == roomMiB-max(0, h.NeedMiB-h.FootprintMiB)-growthMiB
				}
			}
			return false
		})
	}
	reserves(4096) // min(8192 − 2048, 2 × 2048)
	admitted := e.sampledNS(name)
	e.Eventually(startWait, "G's first sample takes its container's baseline", func() bool { return e.sampledNS(name) > admitted })
	fake.SetUsage(mine, (100+1024)<<20)
	reserves(2048) // min(8192 − 2048, 2 × (2048 − 1024))
	e.SetReadings(vm(8192))
	reserves(0)
	e.SetReadings(vm(7168))
	reserves(1024) // min(8192 − 7168, 2 × 1024)
	restarted := vm(2048)
	restarted["DockerVMPID"], restarted["DockerVMVMM"] = vmPID+1, 1
	e.SetReadings(restarted)
	reserves(1536) // min(8192 − 2048, 3/2 × 1024)
	e.finish(g)
}

// An engine that never answers delays no ungated run: a host run and an ungated container request
// are admitted with less wait than one call to it would cost, while the gated run waits with its
// note. Mutation: every engaged run gated, with a need of 0, makes down read the hung engine.
func TestTraceHungEngineDelaysNoUngated(t *testing.T) {
	e, _ := dockerEnv(t, roomConfig(1024), map[string]string{"G": "2G"}, vm(8192), dockertest.Hang())
	e.Run("G", "--mem", "128M")
	e.waitNoted("G")
	h := e.Run("H", "--mem", "128M")
	down := e.runDocker("down")
	for _, label := range []string{"H", "down"} {
		// A reading's deadline is 2 s: a call to the hung engine before admission costs that much.
		if hold := e.WaitHold(label, startWait); time.Duration(hold.StartNS-hold.ArrivalNS) >= 2*time.Second {
			t.Errorf("%s was admitted %v after it arrived", label, time.Duration(hold.StartNS-hold.ArrivalNS))
		}
	}
	waiting(t, e, "G", "G was admitted on a hung engine")
	e.finish(h, down)
}

// peerPID is the pid of the process at the other end of the unix connection c, 0 when unknown.
func peerPID(c net.Conn) int {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0
	}
	var pid int
	_ = raw.Control(func(fd uintptr) { pid, _ = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID) })
	return pid
}

// No process opens Docker's socket while it holds the admission section: at each connection, the
// engine (here one that looks at the section before it answers) finds the section free of the
// connecting process, through a gated admission, a gated waiter's readings, and its admission once
// the first ends. Mutation: a reading taken inside the section is seen there.
func TestTraceNothingInSectionOpensSocket(t *testing.T) {
	// G1 takes 4 GiB of the VM's 6144 MiB of room; G2's 4 GiB then waits, reading every 2 s.
	e, _ := dockerEnv(t, roomConfig(1024), map[string]string{"G1": "4G", "G2": "4G"}, vm(8192))
	eng := e.Engine()
	var mu sync.Mutex
	var conns int
	var inside []string
	fake := dockertest.Serve(t, func(c net.Conn, target string) {
		pid := peerPID(c)
		if r, err := eng.Snapshot(); err == nil && r.Section.Held && r.Section.PID == pid {
			mu.Lock()
			inside = append(inside, fmt.Sprintf("pid %d asked for %s", pid, target))
			mu.Unlock()
		}
		mu.Lock()
		conns++
		mu.Unlock()
		switch {
		case target == "/info":
			dockertest.Reply(c, 200, `{"OperatingSystem":"Docker Desktop","MemTotal":8589934592}`)
		case strings.HasPrefix(target, "/containers/json"):
			dockertest.Reply(c, 200, "[]")
		default:
			dockertest.Reply(c, 404, `{"message":"page not found"}`)
		}
	})
	e.dockerSock = fake.Sock
	g1 := e.Run("G1", "--mem", "128M")
	e.WaitStarted("G1")
	g2 := e.Run("G2", "--mem", "128M")
	e.waitNoted("G2")
	h := e.Run("H", "--mem", "128M")
	e.WaitStarted("H")
	time.Sleep(observeWait)
	e.finish(g1)
	e.WaitStarted("G2")
	e.finish(g2, h)
	mu.Lock()
	defer mu.Unlock()
	if conns < 6 { // G1's reading, and G2's: at least one before its note, one after it, one that admits
		t.Errorf("%d connections to the engine; the trace took too few readings to tell", conns)
	}
	if len(inside) > 0 {
		t.Errorf("Docker's socket opened inside the admission section: %s", strings.Join(inside, "; "))
	}
}

// A reading taken before a gated hold's latest sample would count that hold's recent growth as free
// twice: the gated waiter whose reading a sample made stale reads again at once, in the same attempt,
// and is admitted on the new reading. Mutation: no stamp rule admits G2 on its stale reading.
func TestTraceStaleReadingRetaken(t *testing.T) {
	e, fake := dockerEnv(t, roomConfig(1024), map[string]string{"G1": "1G", "G2": "1G"}, vm(8192))
	g1 := e.Run("G1", "--mem", "128M")
	e.WaitStarted("G1")
	name := e.WaitHold("G1", startWait).Name
	readings := func() (n int) {
		for _, r := range fake.Requests() {
			if strings.HasSuffix(r, "/info") {
				n++
			}
		}
		return n
	}
	before := readings()
	const enter = "admit.before-enter"
	g2 := e.Pausing(enter).Run("G2", "--mem", "128M")
	for { // G2 enters the section again after a reading: hold it there until G1 samples
		g2.WaitReached(enter)
		if readings() > before {
			break
		}
		g2.Release(enter)
		e.Eventually(hookWait, "G2 consumes its release", func() bool {
			_, err := os.Stat(g2.hookPath(enter, "go"))
			return os.IsNotExist(err)
		})
	}
	taken := readings()
	stamp := e.sampledNS(name)
	e.Eventually(startWait, "G1 samples its containers after G2's reading", func() bool { return e.sampledNS(name) > stamp })
	stop := g2.AutoRelease(enter)
	e.WaitStarted("G2")
	stop()
	if n := readings(); n <= taken {
		t.Errorf("G2 was admitted on the reading G1's sample made stale: no reading after it (%d in all)", n)
	}
	e.finish(g1, g2)
}
