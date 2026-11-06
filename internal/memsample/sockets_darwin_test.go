//go:build darwin

package memsample

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// dialEnv makes the test binary a helper that opens FDS descriptors of /dev/null, dials the unix
// socket at that path, prints "ready", and on a line of stdin closes the connection, prints
// "closed" and sleeps for a minute.
const dialEnv = "MEMSAMPLE_HELPER_DIAL"

// fdsEnv is how many descriptors the dial helper opens first.
const fdsEnv = "MEMSAMPLE_HELPER_FDS"

// init is the dial helper, which never reaches TestMain.
func init() {
	path := os.Getenv(dialEnv)
	if path == "" {
		return
	}
	var held []*os.File
	n, _ := strconv.Atoi(os.Getenv(fdsEnv))
	for range n {
		f, err := os.Open("/dev/null")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		held = append(held, f)
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	_ = c.Close()
	fmt.Println("closed")
	time.Sleep(time.Minute)
	runtime.KeepAlive(held)
	os.Exit(0)
}

// listen binds a unix socket named name in a short directory (sun_path is 104 bytes) and returns
// its path.
func listen(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ms")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, name)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	return path
}

// client starts a child of the test binary that opens fds descriptors, dials path and holds it.
// close makes it drop the connection and stay alive.
func client(t *testing.T, path string, fds int) (pid int, close func()) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), dialEnv+"="+path, fdsEnv+"="+strconv.Itoa(fds))
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	r := bufio.NewReader(out)
	say := func(want string) {
		if line, err := r.ReadString('\n'); err != nil || line != want+"\n" {
			t.Fatalf("helper said %q, %v; want %s", line, err, want)
		}
	}
	say("ready")
	return cmd.Process.Pid, func() {
		_, _ = in.Write([]byte("\n"))
		say("closed")
	}
}

// A tree whose child holds a connection to docker.sock has a container client, and has none once the
// child has closed it; the scan starts at this process, so the socket sits below the root. Mutations:
// read unsi_addr (the socket's own address) instead of unsi_caddr in peers → this process's listener
// matches after the close, red; read the root's sockets alone in ContainerClient → the child's is
// missed, red.
func TestClientOfDockerSocket(t *testing.T) {
	_, closeConn := client(t, listen(t, "docker.sock"), 0)
	if !ContainerClient(os.Getpid()) {
		t.Errorf("ContainerClient(parent of a holder of docker.sock) = false, want true")
	}
	closeConn()
	if ContainerClient(os.Getpid()) {
		t.Errorf("ContainerClient(after close) = true, want false")
	}
}

// The listener holds the bound socket of docker.sock, whose peer path is empty: it is no client.
// Mutation: read unsi_addr instead of unsi_caddr, and the listener's own bound path matches.
func TestListenerIsNoClient(t *testing.T) {
	listen(t, "docker.sock")
	if ContainerClient(os.Getpid()) {
		t.Errorf("ContainerClient(the listener) = true, want false")
	}
}

// A socket of another kind has no peer path, whatever its bytes at that offset. Mutation: drop the kind
// check in peerPath, and the second call returns the bytes.
func TestPeerPathOfKind(t *testing.T) {
	s := make([]byte, sockFdInfoSize)
	copy(s[caddrAt+sunPathAt:], "/run/docker.sock")
	binary.LittleEndian.PutUint32(s[kindAt:], sockinfoUn)
	if got := peerPath(s); got != "/run/docker.sock" {
		t.Errorf("peerPath(unix) = %q", got)
	}
	binary.LittleEndian.PutUint32(s[kindAt:], sockinfoUn+1)
	if got := peerPath(s); got != "" {
		t.Errorf("peerPath(another kind) = %q, want none", got)
	}
}

// A process with thousands of descriptors still shows its socket. Mutation: drop the sizing call
// and read the descriptor list into a fixed 4096-entry buffer → the socket past it is missed, red.
func TestClientPastManyDescriptors(t *testing.T) {
	pid, _ := client(t, listen(t, "docker.sock"), 5000)
	if !ContainerClient(pid) {
		t.Errorf("ContainerClient(5000 descriptors, then docker.sock) = false, want true")
	}
}

// A pid that has exited has no sockets to read, and reading it does not panic. Mutation: pass
// &b[0] of an empty buffer in info, and the sizing call panics.
func TestScanSkipsGonePid(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if got := peers(cmd.Process.Pid); len(got) != 0 {
		t.Errorf("peers(exited) = %q, want none", got)
	}
}

// launchd belongs to root: reading it is refused, and nothing comes of it. Mutation: panic on a
// failed sizing call in peers instead of returning nothing, and peers(1) panics with EPERM.
func TestScanSkipsForeignPid(t *testing.T) {
	if got := peers(1); len(got) != 0 {
		t.Errorf("peers(1) = %q, want none", got)
	}
}
