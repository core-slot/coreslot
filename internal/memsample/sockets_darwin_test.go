//go:build darwin

package memsample

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

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

// A child that holds a connection to docker.sock is a container client, and is none once it has
// closed it. Mutation: read unsi_addr (the socket's own address) instead of unsi_caddr in peers,
// and the child, which has bound nothing, reads as no client.
func TestClientOfDockerSocket(t *testing.T) {
	pid, closeConn := client(t, listen(t, "docker.sock"), 0)
	if !ContainerClient(pid) {
		t.Errorf("ContainerClient(holder of docker.sock) = false, want true")
	}
	closeConn()
	if ContainerClient(pid) {
		t.Errorf("ContainerClient(after close) = true, want false")
	}
}

// A connection to a socket of another name is no sign. Mutation: count any peer path as a hit in
// ContainerClient, and the holder of other.sock is a client.
func TestClientOfOtherSocket(t *testing.T) {
	pid, _ := client(t, listen(t, "other.sock"), 0)
	if ContainerClient(pid) {
		t.Errorf("ContainerClient(holder of other.sock) = true, want false")
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

// The peer path of a connection made through a symlink is the bound path, not the link's name.
// Mutation: the one of TestClientOfDockerSocket; this pins that the kernel resolves the link.
func TestClientThroughSymlink(t *testing.T) {
	path := listen(t, "docker.sock")
	link := filepath.Join(filepath.Dir(path), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	pid, _ := client(t, link, 0)
	if !ContainerClient(pid) {
		t.Errorf("ContainerClient(holder through a symlink) = false, want true")
	}
}

// A process with thousands of descriptors still shows its socket. Mutation: read the descriptor
// list into a fixed 4096-entry buffer, and a socket past descriptor 4096 is missed.
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
