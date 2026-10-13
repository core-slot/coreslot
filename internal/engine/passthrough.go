//go:build unix

package engine

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/store"
)

// PassThrough is the grant of a library request made inside another run's grant (PROTOCOL.md
// section 18): the CLI's pass-through, as descriptors and an environment a caller starts its
// command with. It takes no token, publishes no ticket or hold, enters no section and bumps no
// gen; it holds the process's inherited descriptors, which it never closes.
type PassThrough struct {
	grant      map[string]int
	clamped    map[string]int
	unbudgeted []string
	files      []*os.File // nil under rule 1; otherwise the parent's hold (or nil), then its tokens
	env        []string
}

// PassThrough is the grant of req under the decision n, which is NestedUnbudgeted or
// NestedPassThrough, with the outcome a CLI pass-through has (section 18):
//
//   - rule 2: per requested resource min(max, held), held counted from the descriptors
//     (NestedGrant); a resource the parent holds none of is unbudgeted with its min, and a min
//     above what the parent holds is clamped. Files places the parent's hold, when its
//     CORESLOT_HELD_FD names one, at fd 3 and every token the parent passed from fd 4, in
//     ascending resource name and index, and Env names those numbers: the CLI passes the
//     parent's descriptors on unchanged at the numbers it inherited them at, and a launcher's
//     ExtraFiles renumbers them, so the numbers change but the descriptors do not.
//   - rule 1: each requested resource unbudgeted with its min, no descriptors of its own: the
//     CLI passes the inherited ones on where they are, and so does a command started without
//     ExtraFiles.
//
// Env holds, as the CLI's command sees them, the inherited context with CORESLOT_GRANTED_<RES>
// narrowed for each requested resource: the inherited CORESLOT_GRANTED_<RES> of a resource the
// request does not name, and under rule 1 the inherited CORESLOT_UNBUDGETED, CORESLOT_HELD_FD and
// CORESLOT_TOKEN_FDS, are passed on unchanged.
func (n Nested) PassThrough(req spec.Request) *PassThrough {
	req = spec.Normalize(req)
	p := &PassThrough{clamped: map[string]int{}}
	// set is what this grant sets itself; every other inherited context variable is passed on.
	var set []string
	replaced := map[string]bool{}
	if n.Kind == NestedUnbudgeted {
		p.grant = make(map[string]int, len(req.Need))
		for name, r := range req.Need {
			p.grant[name] = r.Min
		}
		p.unbudgeted = slices.Sorted(maps.Keys(req.Need))
		set = ChildEnv(p.grant, false, 0, "")
	} else {
		counts := make(map[string]int, len(n.Held))
		for name, files := range n.Held {
			counts[name] = len(files)
		}
		p.grant, p.clamped, p.unbudgeted = NestedGrant(req, counts)
		hold := inheritedHold(n.env, n.dir, n.Held)
		p.files = append(p.files, hold)
		for _, name := range slices.Sorted(maps.Keys(n.Held)) {
			p.files = append(p.files, n.Held[name]...)
		}
		set = ChildEnv(p.grant, hold != nil, len(p.files)-1, "")
		// The inherited numbers are not the ones Files places the descriptors at.
		replaced[EnvHeldFD], replaced[EnvTokenFDs] = true, true
	}
	for _, kv := range set {
		k, _, _ := strings.Cut(kv, "=")
		replaced[k] = true
	}
	for _, key := range slices.Sorted(maps.Keys(n.env)) {
		if IsContext(key) && !replaced[key] {
			p.env = append(p.env, key+"="+n.env[key])
		}
	}
	p.env = append(p.env, set...)
	return p
}

// inheritedHold is the parent's hold for a rule 2 pass-through to pass on: the descriptor
// CORESLOT_HELD_FD names when it is one of the hold files of dir, at fd 3 or above and none of
// the tokens, or nil. Section 18 validates the tokens alone; the hold only keeps the parent's
// hold live for status while descendants run, so a named descriptor that is no hold (its number
// was reused after a launcher closed the original, section 17) is not passed on, and that is no
// refusal.
func inheritedHold(env map[string]string, dir string, held map[string][]*os.File) *os.File {
	fd, ok := ParseFD(env[EnvHeldFD])
	if !ok || fd < 3 {
		return nil
	}
	for _, files := range held {
		for _, f := range files {
			if f.Fd() == uintptr(fd) {
				return nil
			}
		}
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil
	}
	holds := filepath.Join(dir, store.HoldsDir)
	entries, err := os.ReadDir(holds)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		var hs unix.Stat_t
		p := filepath.Join(holds, e.Name())
		if unix.Lstat(p, &hs) == nil && hs.Mode&unix.S_IFMT == unix.S_IFREG && hs.Dev == st.Dev && hs.Ino == st.Ino {
			return inheritedFile(fd, p)
		}
	}
	return nil
}

// Grant maps every requested resource to what the command may use.
func (p *PassThrough) Grant() map[string]int { return maps.Clone(p.grant) }

// Clamped maps each resource whose min was above what the parent holds to that min.
func (p *PassThrough) Clamped() map[string]int { return maps.Clone(p.clamped) }

// Unbudgeted lists, sorted, the requested resources run unbudgeted: those the parent holds none
// of, or every one under rule 1.
func (p *PassThrough) Unbudgeted() []string { return slices.Clone(p.unbudgeted) }

// TokenOnly is false: a pass-through takes no tokens of its own.
func (p *PassThrough) TokenOnly() bool { return false }

// Files are the descriptors for exec.Cmd.ExtraFiles: under rule 2 the parent's hold (nil when
// there is none, so fd 3 stays closed) and then its tokens; nil under rule 1.
func (p *PassThrough) Files() []*os.File { return slices.Clone(p.files) }

// Env is the CORESLOT_* environment matching Files.
func (p *PassThrough) Env() []string { return slices.Clone(p.env) }

// Close closes nothing: the descriptors are the process's inherited ones, which keep the
// parent's grant held and stay open for every later pass-through of this process.
func (p *PassThrough) Close() error { return nil }
