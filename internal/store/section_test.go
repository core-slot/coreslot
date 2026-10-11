//go:build unix

package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// ---------------------------------------------------------------- helper processes

// A helper process re-executes the test binary into TestHelperProcess. It does what its mode
// says, prints "ready", and exits when its stdin reaches EOF (or when it is killed).
const (
	helperModeEnv = "CORESLOT_STORE_HELPER"
	helperDirEnv  = "CORESLOT_STORE_HELPER_DIR"
	helperArgEnv  = "CORESLOT_STORE_HELPER_ARG"
)

func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	dir := os.Getenv(helperDirEnv)
	var keep []any
	switch mode {
	case "section": // hold the admission section
		s, err := Open(dir)
		if err != nil {
			fmt.Println("error:", err)
			os.Exit(2)
		}
		sec, err := s.Enter(context.Background())
		if err != nil {
			fmt.Println("error:", err)
			os.Exit(2)
		}
		keep = append(keep, sec)
	case "lock": // lock each path listed, one per line
		for _, p := range strings.Split(os.Getenv(helperArgEnv), "\n") {
			f, err := OpenFile(p, os.O_RDONLY|os.O_CREATE)
			if err != nil {
				fmt.Println("error:", err)
				os.Exit(2)
			}
			if ok, err := flockNB(f); !ok || err != nil {
				fmt.Println("error: cannot lock", p, err)
				os.Exit(2)
			}
			keep = append(keep, f)
		}
	case "inherit": // only keep the descriptors inherited through ExtraFiles
	default:
		fmt.Println("error: unknown mode", mode)
		os.Exit(2)
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = keep
	os.Exit(0)
}

type helper struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	readerDone chan struct{} // closed when the stdout reader reached EOF
	once       sync.Once
}

// helperExitWait bounds how long stop waits for a helper to exit after its stdin closes.
const helperExitWait = 30 * time.Second

func startHelper(t *testing.T, mode, dir, arg string, extra []*os.File) *helper {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), helperModeEnv+"="+mode, helperDirEnv+"="+dir, helperArgEnv+"="+arg)
	cmd.ExtraFiles = extra
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &helper{cmd: cmd, stdin: stdin, readerDone: make(chan struct{})}
	t.Cleanup(h.stop)
	line := make(chan string, 1)
	go func() {
		defer close(h.readerDone)
		r := bufio.NewReader(stdout)
		l, _ := r.ReadString('\n')
		line <- strings.TrimSpace(l)
		_, _ = io.Copy(io.Discard, r)
	}()
	select {
	case l := <-line:
		if l != "ready" {
			t.Fatalf("helper %s: %q", mode, l)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("helper %s did not become ready within 60 s", mode)
	}
	return h
}

// stop closes the helper's stdin and waits for it to exit; it is idempotent.
// stop closes the helper's stdin and waits for it to exit, killing it when it has not exited
// within helperExitWait. The stdout reader finishes before Wait, which closes the pipe. It is
// idempotent.
func (h *helper) stop() {
	h.once.Do(func() {
		h.stdin.Close()
		select {
		case <-h.readerDone:
		case <-time.After(helperExitWait):
			_ = h.cmd.Process.Kill()
			<-h.readerDone
		}
		_ = h.cmd.Wait()
	})
}

// ---------------------------------------------------------------- shared test helpers

func enter(t *testing.T, s *Store) *Section {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sec, err := s.Enter(ctx)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	t.Cleanup(sec.Leave)
	return sec
}

// probeLocked reports whether path is locked by another open file description.
func probeLocked(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true
	}
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	return false
}

func ticketName(seq int) string {
	return wire.TicketName(wire.ID{OrderKey: 1791720000000000000, PID: 4711, Seq: seq}, "gate:unit")
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func setPoint(t *testing.T, f func(string)) {
	t.Helper()
	old := point
	point = f
	t.Cleanup(func() { point = old })
}

// ---------------------------------------------------------------- the section

func TestEnterHonoursContext(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	h := startHelper(t, "section", dir, "", nil)

	start := time.Now()
	sec, ok, err := s.TryEnter()
	if err != nil || ok || sec != nil {
		t.Fatalf("TryEnter while a helper holds the section = %v, %v, %v; want nil, false, nil", sec, ok, err)
	}
	if d := time.Since(start); d > 250*time.Millisecond {
		t.Errorf("TryEnter took %v, want at once (< 250 ms)", d)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start = time.Now()
	sec, err = s.Enter(ctx)
	d := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || sec != nil {
		t.Fatalf("Enter with a 300 ms deadline = %v, %v; want context.DeadlineExceeded", sec, err)
	}
	if d < 290*time.Millisecond || d > 600*time.Millisecond {
		t.Errorf("Enter returned after %v, want between the 300 ms deadline and 600 ms", d)
	}

	h.stop()
	sec, ok, err = s.TryEnter()
	if err != nil || !ok {
		t.Fatalf("TryEnter after the helper exited = %v, %v", ok, err)
	}
	sec.Leave()
	sec.Leave() // idempotent
}

func TestEnterExcludesWithinOneProcess(t *testing.T) {
	s := open(t, newDir(t))
	sec := enter(t, s)
	if _, ok, err := s.TryEnter(); ok || err != nil {
		t.Fatalf("a second TryEnter in the same process = %v, %v; want false (one lock per open file description)", ok, err)
	}
	sec.Leave()
	sec2, ok, err := s.TryEnter()
	if !ok || err != nil {
		t.Fatalf("TryEnter after Leave = %v, %v", ok, err)
	}
	sec2.Leave()
	if _, err := sec2.Publish(QueueDir, ticketName(1), []byte("{}"), true); err == nil {
		t.Error("Publish after Leave succeeded")
	}
}

// ---------------------------------------------------------------- publication

func TestPublishNoReplace(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	name := ticketName(1)
	f, err := sec.Publish(QueueDir, name, []byte(`{"first":true}`), true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	g, err := sec.Publish(QueueDir, name, []byte(`{"second":true}`), true)
	if !errors.Is(err, fs.ErrExist) || g != nil {
		t.Fatalf("second Publish of %s = %v, %v; want fs.ErrExist", name, g, err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, QueueDir, name))
	if string(body) != `{"first":true}` {
		t.Errorf("the published ticket was replaced: body %s", body)
	}
	if !probeLocked(t, filepath.Join(dir, QueueDir, name)) {
		t.Error("the published ticket lost its lock")
	}
	if got := names(t, filepath.Join(dir, QueueDir)); !slices.Equal(got, []string{name}) {
		t.Errorf("queue/ after the refused publication: %v, want only %s (no temporary left)", got, name)
	}
	// A non-lock-carrying publication returns no descriptor and leaves the file unlocked.
	c, err := sec.Publish(CancelDir, wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String(), []byte(`{}`), false)
	if err != nil || c != nil {
		t.Fatalf("Publish of a cancel request = %v, %v; want nil, nil", c, err)
	}
}

func TestPublishLinkFallback(t *testing.T) {
	for _, errno := range []unix.Errno{unix.EINVAL, unix.ENOSYS, unix.ENOTSUP} {
		t.Run(errno.Error(), func(t *testing.T) {
			var calls atomic.Int32
			old := renameNoReplace
			renameNoReplace = func(from, to string) error { calls.Add(1); return errno }
			t.Cleanup(func() { renameNoReplace = old })

			dir := newDir(t)
			s := open(t, dir)
			sec := enter(t, s)
			name := ticketName(7)
			f, err := sec.Publish(QueueDir, name, []byte(`{"a":1}`), true)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if calls.Load() == 0 {
				t.Fatal("the no-replace rename was not attempted")
			}
			p := filepath.Join(dir, QueueDir, name)
			if body, _ := os.ReadFile(p); string(body) != `{"a":1}` {
				t.Errorf("body %s", body)
			}
			if !probeLocked(t, p) {
				t.Error("the ticket published through link+unlink is not locked")
			}
			var st unix.Stat_t
			if err := unix.Stat(p, &st); err != nil || st.Nlink != 1 {
				t.Errorf("nlink %d, err %v; want 1 (the temporary name unlinked)", st.Nlink, err)
			}
			if got := names(t, filepath.Join(dir, QueueDir)); !slices.Equal(got, []string{name}) {
				t.Errorf("queue/: %v, want only %s", got, name)
			}
			if _, err := sec.Publish(QueueDir, name, []byte(`{"b":2}`), true); !errors.Is(err, fs.ErrExist) {
				t.Errorf("fallback publication over an existing name: %v, want fs.ErrExist", err)
			}
			if body, _ := os.ReadFile(p); string(body) != `{"a":1}` {
				t.Errorf("the fallback replaced the existing ticket: body %s", body)
			}
		})
	}
}

func TestPublishedTicketIsNeverSeenUnlocked(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	qdir := filepath.Join(dir, QueueDir)

	// From inside publication: before the rename the final name does not exist, and the
	// temporary file is already locked when it gets its final name.
	var checked atomic.Int32
	var current string
	setPoint(t, func(p string) {
		if !strings.HasPrefix(p, "publish.") {
			return
		}
		if _, err := os.Lstat(filepath.Join(qdir, current)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("at %s the final name %s already exists (err %v)", p, current, err)
		}
		if p != "publish.before-rename" {
			return
		}
		for _, n := range names(t, qdir) {
			if isTmpName(n) && !probeLocked(t, filepath.Join(qdir, n)) {
				t.Errorf("at publish.before-rename the temporary %s is not locked", n)
			}
		}
		checked.Add(1)
	})

	var held []*os.File
	const n = 200
	for i := 1; i <= n; i++ {
		current = ticketName(i)
		f, err := sec.Publish(QueueDir, current, []byte(`{}`), true)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, f)
	}
	for _, f := range held {
		f.Close()
	}
	if checked.Load() != n {
		t.Errorf("publish.before-rename was reached %d times, want %d", checked.Load(), n)
	}
}

func TestPublishIntoFullDirectoryIsStateError(t *testing.T) {
	old := maxEntries
	maxEntries = 4
	t.Cleanup(func() { maxEntries = old })
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	for i := 1; i <= 4; i++ {
		f, err := sec.Publish(QueueDir, ticketName(i), []byte(`{}`), true)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
	}
	_, err := sec.Publish(QueueDir, ticketName(5), []byte(`{}`), true)
	wantStateError(t, err, "Publish into a directory holding the maximum")
	if err := os.WriteFile(filepath.Join(dir, QueueDir, "junk"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.List(QueueDir)
	wantStateError(t, err, "List of a directory above the maximum")
}

func TestListParsesSortsAndCountsIgnored(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	for _, i := range []int{3, 1, 2} {
		f, err := sec.Publish(QueueDir, ticketName(i), []byte(`{}`), true)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
	}
	for _, junk := range []string{"junk", "00000000000000000001-0000000001-000001-bad-display", ".tmp-0000000001-000001"} {
		if err := os.WriteFile(filepath.Join(dir, QueueDir, junk), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, ignored, err := s.List(QueueDir)
	if err != nil {
		t.Fatal(err)
	}
	if ignored != 2 {
		t.Errorf("ignored = %d, want 2 (a temporary file is not an ignored entry)", ignored)
	}
	var got []int
	for _, e := range entries {
		got = append(got, e.ID.Seq)
		if e.MtimeNS == 0 {
			t.Errorf("%s: zero mtime", e.Name)
		}
	}
	if !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("List order by seq = %v, want [1 2 3]", got)
	}
	if err := os.Symlink(filepath.Join(dir, AdmitLock), filepath.Join(dir, QueueDir, ticketName(9))); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.List(QueueDir)
	wantStateError(t, err, "List with a symlink under a ticket name")
}

// ---------------------------------------------------------------- gen and FORMAT

func TestGenReadsMissingAndMalformedAsZero(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	if g, err := s.ReadGen(); err != nil || g != (Gen{}) {
		t.Fatalf("missing gen = %+v, %v", g, err)
	}
	for _, body := range []string{``, `{`, `{"gen":1}`, `{"gen":1,"reconciled_ns":2,"gen":3}`,
		`{"gen":1.0,"reconciled_ns":2}`, `{"gen":-1,"reconciled_ns":2}`, `[1,2]`, `{"gen":1,"reconciled_ns":"2"}`} {
		if err := os.WriteFile(filepath.Join(dir, GenFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if g, err := s.ReadGen(); err != nil || g != (Gen{}) {
			t.Errorf("gen %q = %+v, %v; want zero", body, g, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, GenFile), []byte(` {"gen":41,"reconciled_ns":1791719990000000000,"x":{"y":[1]}} `), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := s.ReadGen()
	if err != nil || g != (Gen{N: 41, ReconciledNS: 1791719990000000000}) {
		t.Fatalf("gen = %+v, %v", g, err)
	}
	sec := enter(t, s)
	before, _ := os.Stat(filepath.Join(dir, GenFile))
	if err := sec.BumpGen(); err != nil {
		t.Fatal(err)
	}
	g, _ = s.ReadGen()
	if g != (Gen{N: 42, ReconciledNS: 1791719990000000000}) {
		t.Errorf("after BumpGen: %+v, want gen 42 with reconciled_ns unchanged", g)
	}
	after, _ := os.Stat(filepath.Join(dir, GenFile))
	if os.SameFile(before, after) {
		t.Error("BumpGen rewrote gen in place; it must be replaced by rename")
	}
	if fi, _ := os.Lstat(filepath.Join(dir, GenFile)); fi.Mode().Perm() != 0o600 {
		t.Errorf("gen mode %v", fi.Mode().Perm())
	}
}

// snapshot is every entry under dir with its size and mtime, and admit.lock's body.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %v %d %d\n", p, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, AdmitLock))
	fmt.Fprintf(&b, "admit.lock: %q\n", body)
	return b.String()
}

func TestFormatUnknownIsReported(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	if err := os.WriteFile(filepath.Join(dir, FormatFile), []byte("coreslot-protocol 2"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	sec := enter(t, s)
	n, understood, err := sec.Format([]int{1})
	if err != nil || n != 2 || understood {
		t.Fatalf("Format over coreslot-protocol 2 = %d, %v, %v; want 2, false, nil", n, understood, err)
	}
	if after := snapshot(t, dir); after != before {
		t.Errorf("Format of an unknown format changed the directory:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// The same client implementing format 2 understands it.
	if n, understood, err := sec.Format([]int{1, 2}); err != nil || n != 2 || !understood {
		t.Errorf("Format with 2 implemented = %d, %v, %v", n, understood, err)
	}
}

func TestFormatWritesOneWhenAbsentAndRejectsMalformed(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	n, understood, err := sec.Format([]int{1})
	if err != nil || n != 1 || !understood {
		t.Fatalf("Format with no FORMAT = %d, %v, %v; want 1, true, nil", n, understood, err)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, FormatFile)); string(body) != "coreslot-protocol 1\n" {
		t.Errorf("FORMAT = %q", body)
	}
	// An understood format names the holder in admit.lock's body.
	body, _ := os.ReadFile(filepath.Join(dir, AdmitLock))
	if want := fmt.Sprintf(`{"pid":%d,"entered_ns":`, os.Getpid()); !strings.HasPrefix(string(body), want) {
		t.Errorf("admit.lock body %q, want prefix %q", body, want)
	}
	for content, ok := range map[string]bool{
		"coreslot-protocol 1":     true,
		"coreslot-protocol 1\n":   true,
		"coreslot-protocol 1\n\n": false,
		"coreslot-protocol x\n":   false,
		"coreslot-protocol -1\n":  false,
		"coreslot-protocol \n":    false,
		"coreslot-protocol 1 \n":  false,
		"coreslot-protocol 1\r\n": false,
		"":                        false,
	} {
		if err := os.WriteFile(filepath.Join(dir, FormatFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		n, _, err := sec.Format([]int{1})
		if ok && (err != nil || n != 1) {
			t.Errorf("FORMAT %q = %d, %v; want 1", content, n, err)
		}
		if !ok {
			wantStateError(t, err, fmt.Sprintf("FORMAT %q", content))
		}
	}
}

// ---------------------------------------------------------------- reconciliation

// writeTmp creates a temporary file under dir with the given age.
func writeTmp(t *testing.T, dir string, pid, seq int, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, tmpName(pid, seq))
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestReconcileRemovesOnlyDead(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	pub := func(d, name string, lock bool) *os.File {
		f, err := sec.Publish(d, name, []byte(`{}`), lock)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	liveID := wire.ID{OrderKey: 1791720000000000000, PID: 4711, Seq: 1}
	deadID := wire.ID{OrderKey: 1791720000000000000, PID: 4712, Seq: 1}
	live := pub(QueueDir, wire.TicketName(liveID, "live"), true)
	defer live.Close()
	pub(QueueDir, wire.TicketName(deadID, "dead"), true).Close() // withdrawn: dead at once
	liveHold := pub(HoldsDir, wire.HoldName(1791720600000000000, 7300, 4, "big"), true)
	defer liveHold.Close()
	pub(HoldsDir, wire.HoldName(1791720600000000000, 3999, 5, "build"), true).Close()
	pub(CancelDir, liveID.String(), false)
	pub(CancelDir, deadID.String(), false)
	pub(CancelDir, wire.ID{OrderKey: 1791720900000000000, PID: 5555, Seq: 1}.String(), false)

	// A live helper's ticket, locked from another process.
	otherID := wire.ID{OrderKey: 1791720100000000000, PID: 6100, Seq: 2}
	other := pub(QueueDir, wire.TicketName(otherID, "lint"), true)
	otherPath := filepath.Join(dir, QueueDir, wire.TicketName(otherID, "lint"))
	other.Close()
	startHelper(t, "lock", dir, otherPath, nil)

	oldTmps := []string{
		writeTmp(t, dir, 3998, 2, 2*time.Minute),
		writeTmp(t, filepath.Join(dir, HoldsDir), 3998, 3, 31*time.Second),
		writeTmp(t, filepath.Join(dir, CancelDir), 3998, 4, time.Hour),
	}
	youngTmp := writeTmp(t, filepath.Join(dir, QueueDir), 3998, 5, 5*time.Second)
	junk := filepath.Join(dir, QueueDir, "junk")
	if err := os.WriteFile(junk, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	notTmp := writeTmp(t, dir, 3998, 6, time.Hour)
	if err := os.Rename(notTmp, filepath.Join(dir, ".tmp-x")); err != nil {
		t.Fatal(err)
	}
	notTmp = filepath.Join(dir, ".tmp-x")

	g0, _ := s.ReadGen()
	now := time.Now().UnixNano()
	removed, err := sec.ReconcileIfDue(now)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 7 {
		t.Errorf("removed = %d, want 7 (dead ticket, dead hold, 2 cancel requests, 3 old temporaries)", removed)
	}
	q := filepath.Join(dir, QueueDir)
	for p, want := range map[string]bool{
		filepath.Join(q, wire.TicketName(liveID, "live")): true,
		otherPath: true,
		filepath.Join(q, wire.TicketName(deadID, "dead")):                                  false,
		filepath.Join(dir, HoldsDir, wire.HoldName(1791720600000000000, 7300, 4, "big")):   true,
		filepath.Join(dir, HoldsDir, wire.HoldName(1791720600000000000, 3999, 5, "build")): false,
		filepath.Join(dir, CancelDir, liveID.String()):                                     true,
		filepath.Join(dir, CancelDir, deadID.String()):                                     false,
		filepath.Join(dir, CancelDir, "01791720900000000000-0000005555-000001"):            false,
		youngTmp: true,
		junk:     true,
		notTmp:   true,
	} {
		if exists(p) != want {
			t.Errorf("%s exists = %v, want %v", p, !want, want)
		}
	}
	for _, p := range oldTmps {
		if exists(p) {
			t.Errorf("old temporary %s survived", p)
		}
	}
	if !probeLocked(t, otherPath) || !probeLocked(t, filepath.Join(q, wire.TicketName(liveID, "live"))) {
		t.Error("a live ticket lost its lock")
	}
	g, _ := s.ReadGen()
	if g.N != g0.N+1 || g.ReconciledNS != now {
		t.Errorf("gen after reconciliation = %+v, want gen %d and reconciled_ns %d", g, g0.N+1, now)
	}
	// Due again only 30 s later; with nothing dead, gen is not bumped.
	if r, err := sec.ReconcileIfDue(now + int64(30*time.Second)); r != 0 || err != nil {
		t.Errorf("ReconcileIfDue 30 s later = %d, %v", r, err)
	}
	if g2, _ := s.ReadGen(); g2 != g {
		t.Errorf("a reconciliation that was not due wrote gen: %+v", g2)
	}
	// 31 s later, the temporary file that was 5 s old is 36 s old by the reconciler's clock.
	later := now + int64(31*time.Second)
	if r, err := sec.ReconcileIfDue(later); r != 1 || err != nil || exists(youngTmp) {
		t.Errorf("ReconcileIfDue 31 s later = %d, %v (young temporary exists: %v); want 1", r, err, exists(youngTmp))
	}
	if g2, _ := s.ReadGen(); g2.N != g.N+1 || g2.ReconciledNS != later {
		t.Errorf("gen = %+v, want gen %d reconciled_ns %d", g2, g.N+1, later)
	}
	// With nothing to remove, a due reconciliation writes reconciled_ns and leaves gen.
	latest := later + int64(31*time.Second)
	if r, err := sec.ReconcileIfDue(latest); r != 0 || err != nil {
		t.Errorf("ReconcileIfDue with nothing dead = %d, %v", r, err)
	}
	if g3, _ := s.ReadGen(); g3.N != g.N+1 || g3.ReconciledNS != latest {
		t.Errorf("gen after an empty reconciliation = %+v, want gen %d reconciled_ns %d", g3, g.N+1, latest)
	}
}

func TestReconcileRespects30s(t *testing.T) {
	cases := []struct {
		name   string
		offset time.Duration // reconciled_ns relative to now
		due    bool
	}{
		{"29s ago", -29 * time.Second, false},
		{"30s ago", -30 * time.Second, false},
		{"31s ago", -31 * time.Second, true},
		{"29s ahead", 29 * time.Second, false},
		{"31s ahead", 31 * time.Second, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := newDir(t)
			s := open(t, dir)
			sec := enter(t, s)
			now := time.Now().UnixNano()
			g0 := Gen{N: 5, ReconciledNS: now + int64(c.offset)}
			if err := sec.writeGen(g0); err != nil {
				t.Fatal(err)
			}
			name := ticketName(1)
			f, err := sec.Publish(QueueDir, name, []byte(`{}`), true)
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
			removed, err := sec.ReconcileIfDue(now)
			if err != nil {
				t.Fatal(err)
			}
			dead := filepath.Join(dir, QueueDir, name)
			g, _ := s.ReadGen()
			if c.due {
				if removed != 1 || exists(dead) || g != (Gen{N: 6, ReconciledNS: now}) {
					t.Errorf("due: removed %d, dead ticket exists %v, gen %+v", removed, exists(dead), g)
				}
			} else if removed != 0 || !exists(dead) || g != g0 {
				t.Errorf("not due: removed %d, dead ticket exists %v, gen %+v (was %+v)", removed, exists(dead), g, g0)
			}
		})
	}
}

func TestPublishSkipsAnExistingTemporaryName(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	q := filepath.Join(dir, QueueDir)
	if err := os.Mkdir(q, 0o700); err != nil {
		t.Fatal(err)
	}
	// A young leftover of an earlier process with this pid holds the next temporary name.
	next := int((seq.Load() + 1) % wire.SeqModulus)
	leftover := filepath.Join(q, tmpName(os.Getpid(), next))
	if err := os.WriteFile(leftover, []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := sec.Publish(QueueDir, ticketName(1), []byte(`{}`), true)
	if err != nil {
		t.Fatalf("Publish over an existing temporary name: %v", err)
	}
	defer f.Close()
	if body, err := os.ReadFile(leftover); err != nil || string(body) != "leftover" {
		t.Errorf("the leftover temporary file was touched: %q, %v", body, err)
	}
	if body, _ := os.ReadFile(filepath.Join(q, ticketName(1))); string(body) != `{}` {
		t.Errorf("published body %q", body)
	}
}

func TestPublishRefusesNamesOutsideGrammar(t *testing.T) {
	s := open(t, newDir(t))
	sec := enter(t, s)
	id := wire.ID{OrderKey: 1, PID: 2, Seq: 3}
	for _, c := range []struct{ dir, name string }{
		{QueueDir, "junk"}, {QueueDir, "../holds/" + ticketName(1)}, {QueueDir, tmpName(1, 1)},
		{HoldsDir, ticketName(1) + "-x"}, {CancelDir, ticketName(1)}, {CancelDir, "junk"},
		{GenFile, "x"},
	} {
		if f, err := sec.Publish(c.dir, c.name, []byte(`{}`), false); err == nil || f != nil {
			t.Errorf("Publish(%s, %q) = %v, %v; want a refusal", c.dir, c.name, f, err)
		}
	}
	for _, c := range []struct{ dir, name string }{
		{QueueDir, ticketName(1)}, {HoldsDir, id.String()}, {CancelDir, id.String()},
	} {
		if _, err := sec.Publish(c.dir, c.name, []byte(`{}`), false); err != nil {
			t.Errorf("Publish(%s, %q): %v", c.dir, c.name, err)
		}
	}
}

func TestReconcileUnlinksWhileHoldingProbeLock(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	name := ticketName(1)
	f, err := sec.Publish(QueueDir, name, []byte(`{}`), true)
	if err != nil {
		t.Fatal(err)
	}
	f.Close() // withdrawn: dead
	path := filepath.Join(dir, QueueDir, name)
	reached := 0
	setPoint(t, func(p string) {
		if p != "remove.before-unlink" {
			return
		}
		reached++
		if !exists(path) {
			t.Error("at remove.before-unlink the dead ticket is already gone")
		} else if !probeLocked(t, path) {
			t.Error("at remove.before-unlink the reconciler no longer holds the probe lock")
		}
	})
	if removed, err := sec.ReconcileIfDue(NowNS()); err != nil || removed != 1 {
		t.Fatalf("ReconcileIfDue = %d, %v", removed, err)
	}
	if reached != 1 || exists(path) {
		t.Errorf("remove.before-unlink reached %d times; ticket exists %v", reached, exists(path))
	}
}
