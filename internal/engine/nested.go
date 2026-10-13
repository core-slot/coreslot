//go:build unix

package engine

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/config"
	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/store"
)

// The environment of a run's command (PROTOCOL.md section 17).
const (
	EnvTokenFDs   = "CORESLOT_TOKEN_FDS"
	EnvHeldFD     = "CORESLOT_HELD_FD"
	EnvUnbudgeted = "CORESLOT_UNBUDGETED"
	EnvGranted    = "CORESLOT_GRANTED_" // followed by a resource name in environment form
)

// ParseTokenFDs parses CORESLOT_TOKEN_FDS (PROTOCOL.md section 17): `item *("," item)`, each
// item `fd` or `a-b` with a <= b, every fd one to nine decimal digits. A list naming more than
// spec.MaxPerGrant descriptors is refused before any range is expanded, and so is a list naming
// one descriptor twice. The descriptors are returned in the order listed.
func ParseTokenFDs(s string) ([]int, error) {
	type span struct{ a, b int }
	var spans []span
	total := 0
	for _, item := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(item, "-")
		a, ok := ParseFD(lo)
		b := a
		if ok && isRange {
			b, ok = ParseFD(hi)
		}
		if !ok || b < a {
			return nil, fmt.Errorf("%s=%q is malformed", EnvTokenFDs, s)
		}
		if total += b - a + 1; total > spec.MaxPerGrant {
			return nil, fmt.Errorf("%s=%q names more than %d descriptors", EnvTokenFDs, s, spec.MaxPerGrant)
		}
		spans = append(spans, span{a, b})
	}
	seen := make(map[int]bool, total)
	fds := make([]int, 0, total)
	for _, sp := range spans {
		for fd := sp.a; fd <= sp.b; fd++ {
			if seen[fd] {
				return nil, fmt.Errorf("%s=%q names descriptor %d twice", EnvTokenFDs, s, fd)
			}
			seen[fd] = true
			fds = append(fds, fd)
		}
	}
	return fds, nil
}

// ParseFD parses one descriptor number of the descriptor contract (PROTOCOL.md section 17): one
// to nine decimal digits and nothing else, no sign and no space (spec.ParseDigits). It serves
// CORESLOT_HELD_FD and each fd of CORESLOT_TOKEN_FDS.
func ParseFD(s string) (int, bool) { return spec.ParseDigits(s) }

// stateDirOf is the state directory a context names: its CORESLOT_STATE_DIR when set (an
// absolute path), otherwise the default of PROTOCOL.md section 4.1. Only env counts, not this
// process's environment.
func stateDirOf(env map[string]string) (string, error) {
	dir, _, err := config.StateDirFrom(func(k string) string { return env[k] })
	return dir, err
}

// tokenFile is one token file found under res/.
type tokenFile struct {
	resource string
	path     string
}

type devIno struct{ dev, ino uint64 }

// reusedCause is the likely cause of a listed descriptor that is open but no token: once a
// launcher closes it, the next descriptor this process opens, the Go runtime's included, takes
// its number.
const reusedCause = "a launcher between the parent run and this one probably closed it and the number was reused"

// NestedKind is what a run, or a library request, does with the grant context it inherited
// (PROTOCOL.md section 18).
type NestedKind int

const (
	// NotNested is an ordinary request (rule 3): nothing was inherited, or the run is
	// --independent.
	NotNested NestedKind = iota
	// NestedUnbudgeted runs unbudgeted under a parent that runs unbudgeted (rule 1).
	NestedUnbudgeted
	// NestedPassThrough runs on the parent's grant (rule 2).
	NestedPassThrough
)

// Nested is the decision DecideNested makes.
type Nested struct {
	Kind NestedKind
	// Reason is the inherited CORESLOT_UNBUDGETED of a NestedUnbudgeted decision.
	Reason string
	// Held is ValidateInherited's held of a NestedPassThrough decision.
	Held map[string][]*os.File
}

// DecideNested decides, from the environment a run inherited, what it does with the grant
// context in it (PROTOCOL.md section 18; an empty variable is unset), for the CLI and the library
// alike. Unless independent: a set CORESLOT_UNBUDGETED is rule 1; otherwise a set
// CORESLOT_TOKEN_FDS is rule 2, and when that context does not validate (ValidateInherited) err
// says why and the caller refuses, never queueing behind a parent that may hold the whole budget.
// Anything else, or independent, is an ordinary request (rule 3). It writes nothing and closes
// nothing: dropping the inherited context of an ordinary request is its caller's business.
func DecideNested(env map[string]string, independent bool) (Nested, error) {
	if independent {
		return Nested{}, nil
	}
	if reason := env[EnvUnbudgeted]; reason != "" {
		return Nested{Kind: NestedUnbudgeted, Reason: reason}, nil
	}
	if env[EnvTokenFDs] == "" {
		return Nested{}, nil
	}
	held, err := ValidateInherited(env)
	if err != nil {
		return Nested{}, err
	}
	return Nested{Kind: NestedPassThrough, Held: held}, nil
}

// NestedGrant is a pass-through's grant (PROTOCOL.md section 18 rule 2): per requested resource
// min(max, held), where held counts the parent's valid token descriptors of it. A resource the
// parent holds none of runs unbudgeted with its min; one whose min is above what the parent
// holds is clamped to all of it, and clamped records the requested min.
func NestedGrant(req spec.Request, held map[string]int) (grant map[string]int, clamped map[string]int, unbudgeted []string) {
	grant = make(map[string]int, len(req.Need))
	clamped = map[string]int{}
	for name, r := range req.Need {
		h := held[name]
		switch {
		case h == 0:
			grant[name] = r.Min
			unbudgeted = append(unbudgeted, name)
		case r.Min > h:
			grant[name] = h
			clamped[name] = r.Min
		default:
			grant[name] = min(r.Max, h)
		}
	}
	slices.Sort(unbudgeted)
	return grant, clamped, unbudgeted
}

// ValidateInherited checks an inherited grant for a pass-through (PROTOCOL.md section 18 rule
// 2): CORESLOT_TOKEN_FDS of env must parse, name no descriptor twice and at most 512 in all,
// and every descriptor it names must be open, have the (st_dev, st_ino) of some
// $S/res/<name>/<i>, and that path's fresh probe must fail (some description still holds its
// lock). $S is env's CORESLOT_STATE_DIR, or the default. A context that cannot be checked
// because $S, res/ or a token file cannot be read does not validate either. It enters no
// section and writes nothing.
//
// held maps each resource to the descriptors of its tokens, in the order listed. They wrap the
// inherited descriptors, which stay open (and inheritable) for the caller's command: the caller
// keeps held reachable until the command has exited, since an *os.File that is collected
// closes its descriptor. Each error names the likely cause.
func ValidateInherited(env map[string]string) (held map[string][]*os.File, err error) {
	fds, err := ParseTokenFDs(env[EnvTokenFDs])
	if err != nil {
		return nil, err
	}
	want := make(map[devIno]int, len(fds))
	for _, fd := range fds {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return nil, fmt.Errorf("descriptor %d of %s is not open (%v): a launcher between the parent run and this one closed it", fd, EnvTokenFDs, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return nil, fmt.Errorf("descriptor %d of %s is not a token file: %s", fd, EnvTokenFDs, reusedCause)
		}
		id := devIno{uint64(st.Dev), uint64(st.Ino)}
		if other, dup := want[id]; dup {
			return nil, fmt.Errorf("descriptors %d and %d of %s are the same token file: a launcher duplicated a descriptor, and counting both would overstate the grant", other, fd, EnvTokenFDs)
		}
		want[id] = fd
	}
	dir, err := stateDirOf(env)
	if err != nil {
		return nil, fmt.Errorf("the inherited grant cannot be checked: %v", err)
	}
	found, err := findTokens(dir, want)
	if err != nil {
		return nil, fmt.Errorf("the inherited grant cannot be checked: %v", err)
	}
	for _, fd := range fds {
		tok, ok := found[fd]
		if !ok {
			return nil, fmt.Errorf("descriptor %d of %s is not a token file of %s: %s, or the parent run used another state directory", fd, EnvTokenFDs, dir, reusedCause)
		}
		locked, err := store.ProbeLocked(tok.path)
		if err != nil {
			return nil, fmt.Errorf("the inherited grant cannot be checked: %v", err)
		}
		if !locked {
			return nil, fmt.Errorf("descriptor %d of %s names %s, which no process holds locked: the parent's grant is gone", fd, EnvTokenFDs, tok.path)
		}
	}
	held = make(map[string][]*os.File)
	for _, fd := range fds {
		tok := found[fd]
		held[tok.resource] = append(held[tok.resource], os.NewFile(uintptr(fd), tok.path))
	}
	return held, nil
}

// findTokens finds, among the token files of dir, those whose identity want lists, by
// descriptor. It creates nothing (store.TokenFiles).
func findTokens(dir string, want map[devIno]int) (map[int]tokenFile, error) {
	files, err := store.TokenFiles(dir)
	if err != nil {
		return nil, err
	}
	found := make(map[int]tokenFile, len(want))
	for _, f := range files {
		if fd, ok := want[devIno{f.Dev, f.Ino}]; ok {
			found[fd] = tokenFile{resource: f.Resource, path: f.Path}
		}
	}
	return found, nil
}
