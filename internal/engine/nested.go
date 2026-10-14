//go:build unix

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
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
	index    int
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

	env map[string]string // the environment decided from
	dir string            // NestedPassThrough: the state directory the context was checked in
}

// DecideNested decides, from the environment a run inherited, what it does with the grant
// context in it (PROTOCOL.md section 18; an empty variable is unset), for the CLI and the library
// alike. Unless independent: a set CORESLOT_UNBUDGETED is rule 1; otherwise a set
// CORESLOT_TOKEN_FDS is rule 2, and when that context does not validate (ValidateInherited) err
// says why and the caller refuses, never queueing behind a parent that may hold the whole budget.
// Anything else, or independent, is an ordinary request (rule 3). It writes nothing and closes
// nothing: dropping the inherited context of an ordinary request is its caller's business.
//
// budget is the state directory of the caller's own budget, or empty for the one env names, which
// is then resolved here (the CLI's case: its state directory is always that one). A context is a
// parent's only in the budget it came from, the directory env's CORESLOT_STATE_DIR (or the
// default) names, since a run passes CORESLOT_STATE_DIR on to its command; a library client may
// open another (a test's Options.StateDir), and there the inherited context belongs to another
// budget, whose tokens this one never counts, so the request is ordinary.
//
// When budget is set and the directory env names cannot be resolved (CORESLOT_STATE_DIR is not
// an absolute path, or it is unset and the home directory cannot be learned), whether the context
// is this budget's cannot be told, and err says so: the caller refuses. Taking it for another
// budget would queue the request, and when budget is in fact the directory env names (a caller
// that spells out the default in Options.StateDir) it would queue behind its own parent, which
// may hold the whole budget; a refusal cannot hang, and independent is the way to queue anyway.
// A client whose directory is the one env names by construction calls DecideNestedNamed instead,
// which resolves nothing.
func DecideNested(env map[string]string, independent bool, budget string) (Nested, error) {
	return decideNested(env, independent, budget, false)
}

// DecideNestedNamed is DecideNested for a caller whose state directory dir is the one env names
// by construction: it resolved dir from this process's environment by the rule of PROTOCOL.md
// section 4.1, as a library Client opened without Options.StateDir does, and env is that
// environment. It does not resolve the directory again: a second home-directory lookup may run
// getent again and fail where the first succeeded (a timeout), and the request must then not be
// taken for another budget's and queue behind its own parent.
func DecideNestedNamed(env map[string]string, independent bool, dir string) (Nested, error) {
	return decideNested(env, independent, dir, true)
}

func decideNested(env map[string]string, independent bool, budget string, named bool) (Nested, error) {
	if independent || (env[EnvUnbudgeted] == "" && env[EnvTokenFDs] == "") {
		return Nested{}, nil
	}
	dir := "" // the directory env names, when known: validateInherited resolves it otherwise
	switch {
	case named:
		dir = filepath.Clean(budget)
	case budget != "":
		envDir, err := stateDirOf(env)
		if err != nil {
			return Nested{}, fmt.Errorf("the state directory the inherited context names cannot be resolved, so "+
				"whether it is this budget's (%s) cannot be told: %v", budget, err)
		}
		if !sameDir(envDir, budget) {
			return Nested{}, nil
		}
		dir = envDir
	}
	if reason := env[EnvUnbudgeted]; reason != "" {
		return Nested{Kind: NestedUnbudgeted, Reason: reason, env: env}, nil
	}
	held, dir, err := validateInherited(env, dir)
	if err != nil {
		return Nested{}, err
	}
	return Nested{Kind: NestedPassThrough, Held: held, env: env, dir: dir}, nil
}

// sameDir reports whether dir is budget: the same path, or the same directory reached through
// another one.
func sameDir(dir, budget string) bool {
	if dir == filepath.Clean(budget) {
		return true
	}
	a, err := os.Stat(dir)
	if err != nil {
		return false
	}
	b, err := os.Stat(budget)
	return err == nil && os.SameFile(a, b)
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
// held maps each resource to the descriptors of its tokens, in ascending index. They wrap the
// inherited descriptors, which stay open (and inheritable) for the caller's command, through
// inheritedFile: one *os.File per descriptor for the life of the process, so that a later
// validation (a second library request, or another Client) gets the same files, and none is ever
// collected, which would close the descriptor under the grant. The caller must not close them.
// Each error names the likely cause.
func ValidateInherited(env map[string]string) (held map[string][]*os.File, err error) {
	held, _, err = validateInherited(env, "")
	return held, err
}

// validateInherited is ValidateInherited in dir, the state directory env names, or in the one it
// resolves from env when dir is empty; it also returns the directory it checked in.
func validateInherited(env map[string]string, dir string) (held map[string][]*os.File, _ string, err error) {
	fds, err := ParseTokenFDs(env[EnvTokenFDs])
	if err != nil {
		return nil, "", err
	}
	want := make(map[devIno]int, len(fds))
	for _, fd := range fds {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return nil, "", fmt.Errorf("descriptor %d of %s is not open (%v): a launcher between the parent run and this one closed it", fd, EnvTokenFDs, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return nil, "", fmt.Errorf("descriptor %d of %s is not a token file: %s", fd, EnvTokenFDs, reusedCause)
		}
		id := devIno{uint64(st.Dev), uint64(st.Ino)}
		if other, dup := want[id]; dup {
			return nil, "", fmt.Errorf("descriptors %d and %d of %s are the same token file: a launcher duplicated a descriptor, and counting both would overstate the grant", other, fd, EnvTokenFDs)
		}
		want[id] = fd
	}
	if dir == "" {
		if dir, err = stateDirOf(env); err != nil {
			return nil, "", fmt.Errorf("the inherited grant cannot be checked: %v", err)
		}
	}
	found, err := findTokens(dir, want)
	if err != nil {
		return nil, "", fmt.Errorf("the inherited grant cannot be checked: %v", err)
	}
	for _, fd := range fds {
		tok, ok := found[fd]
		if !ok {
			return nil, "", fmt.Errorf("descriptor %d of %s is not a token file of %s: %s, or the parent run used another state directory", fd, EnvTokenFDs, dir, reusedCause)
		}
		locked, err := store.ProbeLocked(tok.path)
		if err != nil {
			return nil, "", fmt.Errorf("the inherited grant cannot be checked: %v", err)
		}
		if !locked {
			return nil, "", fmt.Errorf("descriptor %d of %s names %s, which no process holds locked: the parent's grant is gone", fd, EnvTokenFDs, tok.path)
		}
	}
	// Ascending index within each resource, the order of the descriptor contract (section 17),
	// whatever order the list named them in.
	slices.SortFunc(fds, func(a, b int) int { return found[a].index - found[b].index })
	held = make(map[string][]*os.File)
	for _, fd := range fds {
		tok := found[fd]
		held[tok.resource] = append(held[tok.resource], inheritedFile(fd, tok.path))
	}
	return held, dir, nil
}

// inherited keeps the *os.File of every inherited descriptor a validation wrapped, for the life
// of the process. An *os.File that is collected closes its descriptor, and two of them for one
// descriptor would each close it, the second time perhaps a descriptor that has since reused the
// number; the inherited grant must instead stay open for every later pass-through of this
// process, the library's included, and for the commands they start.
var inherited struct {
	mu    sync.Mutex
	files map[int]*os.File
}

// inheritedFile is the one *os.File of the inherited descriptor fd, named name when it is first
// wrapped. A file that no longer has fd (its owner closed it, against this package's contract)
// is replaced.
func inheritedFile(fd int, name string) *os.File {
	inherited.mu.Lock()
	defer inherited.mu.Unlock()
	if f := inherited.files[fd]; f != nil && f.Fd() == uintptr(fd) {
		return f
	}
	if inherited.files == nil {
		inherited.files = map[int]*os.File{}
	}
	f := os.NewFile(uintptr(fd), name)
	inherited.files[fd] = f
	return f
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
			found[fd] = tokenFile{resource: f.Resource, index: f.Index, path: f.Path}
		}
	}
	return found, nil
}
