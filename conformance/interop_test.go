//go:build coreslot_testhooks && unix

package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/wire"
)

// TestInteropPythonAndGo runs the independent Python client beside the Go CLI and library in one
// state directory: waiters of all three queue in one FIFO order and are admitted in it, the
// Python client's tickets and holds are what `coreslot status` reports, and the Go and Python
// decoders agree on every wire fixture.
func TestInteropPythonAndGo(t *testing.T) {
	py, pyc, err := pyClient(t)
	if err != nil {
		t.Skipf("the independent Python client is not exercised: no Python 3.11+ (%v)", err)
	}
	lib := buildLibClient(t)

	t.Run("fifo across languages", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(cpuConfig(1))
		block := e.Run("block", "--need", "cpu=1")
		e.WaitHold("block", startWait)

		type waiter struct {
			name, lang string
			p          *Proc
		}
		order := []*waiter{{name: "go1", lang: "cli"}, {name: "py1", lang: "py"}, {name: "lib1", lang: "lib"},
			{name: "py2", lang: "py"}, {name: "go2", lang: "cli"}, {name: "lib2", lang: "lib"}, {name: "py3", lang: "py"}}
		for _, w := range order {
			switch w.lang {
			case "cli":
				w.p = e.Run(w.name, "--need", "cpu=1")
			case "py":
				w.p = e.StartCommand(py, pyc, "run", "--label", w.name, "--need", "cpu=1..1", "--",
					bins.child, "hold", e.MarkDir, w.name)
			case "lib":
				w.p = e.StartCommand(lib, "-dir", e.Dir, "-mark", e.MarkDir, "-name", w.name, "-label", w.name,
					"-need", "cpu=1..1")
			}
			// The next waiter arrives only once this one's ticket is published, so the order
			// of arrival is the order of this list.
			e.WaitTicket(w.name)
		}

		// status --json lists every ticket, the Python ones included, in queue order.
		doc := statusJSON(t, e)
		var labels []string
		for i, tk := range doc.Tickets {
			labels = append(labels, tk.Label)
			if tk.Position != i+1 || tk.State != "waiting" {
				t.Errorf("ticket %s: position %d, state %s; want %d, waiting", tk.Label, tk.Position, tk.State, i+1)
			}
		}
		var want []string
		for _, w := range order {
			want = append(want, w.name)
		}
		if !slices.Equal(labels, want) {
			t.Fatalf("status --json lists the tickets %v, want %v", labels, want)
		}

		e.ReleaseChild("block")
		block.ExitCode(0)
		for i, w := range order {
			e.Eventually(startWait, "the command of "+w.name+" starts", func() bool {
				for _, later := range order[i+1:] {
					if _, ok := e.Started(later.name); ok {
						t.Fatalf("%s (%s) was admitted before %s (%s), which arrived first", later.name, later.lang, w.name, w.lang)
					}
				}
				_, ok := e.Started(w.name)
				return ok
			})
			if w.lang == "py" {
				pythonHoldVisible(t, e, w.name)
			}
			e.ReleaseChild(w.name)
			w.p.ExitCode(0)
		}
	})

	// The CLI conventions of PROTOCOL.md section 21 are not protocol, but both clients follow
	// them: on an 8-token budget each run of the same arguments gets the same grant and exit
	// status from either client, and logs the same normalized request.
	t.Run("need forms and the default request agree", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(cpuConfig(8))
		report := []string{"--", "sh", "-c", `echo "$CORESLOT_GRANTED_CPU $1"`, "sh"}
		cases := []struct {
			flags  []string
			arg    string
			out    string
			code   int
			logged string
		}{
			{nil, "x", "1 x", 0, "map[cpu:[1 1]]"},
			{nil, "{n}", "8 8", 0, "map[cpu:[1 16]]"},
			{nil, "{n:cpu}", "8 8", 0, "map[cpu:[1 16]]"},
			{nil, "{{n}}", "1 {n}", 0, "map[cpu:[1 1]]"},
			{nil, "-j{n}", "1 -j{n}", 0, "map[cpu:[1 1]]"},
			{[]string{"--subst"}, "-j{n}", "8 -j8", 0, "map[cpu:[1 16]]"},
			{[]string{"--need", "cpu=4.."}, "x", "8 x", 0, "map[cpu:[4 16]]"},
			{[]string{"--need", "cpu=20.."}, "x", "8 x", 0, "map[cpu:[20 20]]"},
			{[]string{"--need", "cpu=..2"}, "x", "2 x", 0, "map[cpu:[1 2]]"},
			{[]string{"--need", "cpu=.."}, "x", "", 64, ""},
			{[]string{"--need", "cpu=..."}, "x", "", 64, ""},
			{[]string{"--need", "cpu=-1.."}, "x", "", 64, ""},
			{[]string{"--need", "cpu=..0"}, "x", "", 64, ""},
			{[]string{"--need", "cpu=257.."}, "x", "", 64, ""},
		}
		for _, c := range cases {
			args := append(append(append([]string{"run"}, c.flags...), report...), c.arg)
			for _, client := range []struct {
				name string
				argv []string
			}{{"go", []string{bins.coreslot}}, {"python", []string{py, pyc}}} {
				before := len(runLogNeeds(t, e))
				p := e.StartCommand(append(slices.Clone(client.argv), args...)...)
				p.ExitCode(c.code)
				if got := strings.TrimSpace(p.Stdout()); got != c.out {
					t.Errorf("%s %q: printed %q, want %q", client.name, args, got, c.out)
				}
				needs := runLogNeeds(t, e)
				switch {
				case c.logged == "" && len(needs) != before:
					t.Errorf("%s %q: a refused run appended to the run log", client.name, args)
				case c.logged != "" && (len(needs) != before+1 || needs[len(needs)-1] != c.logged):
					t.Errorf("%s %q: run log needs %v, want %s appended", client.name, args, needs[before:], c.logged)
				}
			}
		}
	})

	t.Run("fixture decoders agree", func(t *testing.T) {
		root, err := moduleRoot()
		if err != nil {
			t.Fatal(err)
		}
		pydir := filepath.Join(root, "conformance", "pyclient")
		unit := exec.Command(py, "-m", "unittest", "-v", "test_fixtures")
		unit.Dir = pydir
		if out, err := unit.CombinedOutput(); err != nil {
			t.Fatalf("the Python fixture test failed: %v\n%s", err, out)
		}
		testdata := filepath.Join(root, "internal", "wire", "testdata")
		pyVerdicts := pythonDecodes(t, py, pydir, testdata)
		goVerdicts := goDecodes(t, testdata)
		if len(goVerdicts) < 20 {
			t.Fatalf("only %d fixtures decoded", len(goVerdicts))
		}
		for _, k := range sortedKeys(goVerdicts) {
			g, p := goVerdicts[k], pyVerdicts[k]
			if _, ok := pyVerdicts[k]; !ok {
				t.Errorf("%s: decoded by Go only", k)
				continue
			}
			if !reflect.DeepEqual(g, p) {
				t.Errorf("%s: Go decodes %s, Python %s", k, show(g), show(p))
			}
		}
		for k := range pyVerdicts {
			if _, ok := goVerdicts[k]; !ok {
				t.Errorf("%s: decoded by Python only", k)
			}
		}
		if !t.Failed() {
			t.Logf("%d fixtures: Go and Python agree", len(goVerdicts))
		}
	})
}

// runLogNeeds is the need of every run-log line, formatted as fmt prints a decoded map.
func runLogNeeds(t *testing.T, e Env) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.Dir, "log", "runs.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec struct {
			Need map[string][]int `json:"need"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("run log line %q: %v", line, err)
		}
		out = append(out, fmt.Sprint(rec.Need))
	}
	return out
}

type statusDoc struct {
	Resources map[string]struct {
		Locked         int `json:"locked"`
		UnlistedLocked int `json:"unlisted_locked"`
	} `json:"resources"`
	Holds []struct {
		Label string         `json:"label"`
		PID   int            `json:"pid"`
		Grant map[string]int `json:"grant"`
	} `json:"holds"`
	Tickets []struct {
		ID       string `json:"id"`
		Label    string `json:"label"`
		Position int    `json:"position"`
		State    string `json:"state"`
	} `json:"tickets"`
}

func statusJSON(t *testing.T, e Env) statusDoc {
	t.Helper()
	p := e.Start("status", "--json")
	p.ExitCode(0)
	var doc statusDoc
	if err := json.Unmarshal([]byte(p.Stdout()), &doc); err != nil {
		t.Fatalf("status --json: %v\n%s", err, p.Stdout())
	}
	return doc
}

// pythonHoldVisible checks that the Python run labelled name holds one cpu token in status,
// human and JSON, with no locked token unaccounted for.
func pythonHoldVisible(t *testing.T, e Env, name string) {
	t.Helper()
	doc := statusJSON(t, e)
	found := false
	for _, h := range doc.Holds {
		if h.Label == name {
			found = true
			if h.Grant["cpu"] != 1 {
				t.Errorf("status --json: the Python hold %s grants %v, want cpu 1", name, h.Grant)
			}
		}
	}
	if !found || len(doc.Holds) != 1 {
		t.Errorf("status --json: holds %+v, want the Python run %s alone", doc.Holds, name)
	}
	if r := doc.Resources["cpu"]; r.Locked != 1 || r.UnlistedLocked != 0 {
		t.Errorf("status --json: cpu locked %d, unlisted %d; want 1 and 0", r.Locked, r.UnlistedLocked)
	}
	human := e.Start("status")
	human.ExitCode(0)
	if !strings.Contains(human.Stdout(), name) {
		t.Errorf("status does not show the Python hold %s:\n%s", name, human.Stdout())
	}
}

// pyDecodeScript prints, as one JSON object, the Python client's verdict on every fixture:
// a body's decoded value or null, a name's parsed fields or null.
const pyDecodeScript = `
import json, os, sys
pydir, testdata = sys.argv[1], sys.argv[2]
sys.path.insert(0, pydir)
import coreslot_client as cc
out = {}
for kind, dec in (("ticket", cc.decode_ticket), ("hold", cc.decode_hold), ("cancel", cc.decode_cancel)):
    d = os.path.join(testdata, "bodies", kind)
    for n in sorted(os.listdir(d)):
        if not n.endswith(".body"):
            continue
        with open(os.path.join(d, n), "rb") as fh:
            raw = fh.read()
        try:
            out["body " + kind + "/" + n] = dec(raw)
        except cc.Malformed:
            out["body " + kind + "/" + n] = None
with open(os.path.join(testdata, "names.json")) as fh:
    names = json.load(fh)
for key in ("valid", "invalid"):
    for case in names[key]:
        r = cc.parse_name(case["name"])
        out["name " + case["name"]] = None if r is None else [str(r[0]), r[1], r[2]]
for key in ("cancel_valid", "cancel_invalid"):
    for case in names[key]:
        r = cc.parse_cancel_name(case["name"])
        out["cancel " + case["name"]] = None if r is None else [str(r[0]), r[1], r[2]]
json.dump(out, sys.stdout)
`

func pythonDecodes(t *testing.T, py, pydir, testdata string) map[string]any {
	t.Helper()
	script := filepath.Join(t.TempDir(), "decode.py")
	if err := os.WriteFile(script, []byte(pyDecodeScript), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, script, pydir, testdata)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("Python decoding: %v\n%s", err, stderr.String())
	}
	return decodeNumbers(t, out).(map[string]any)
}

// decodeNumbers parses JSON keeping numbers exact, as their text.
func decodeNumbers(t *testing.T, data []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return v
}

func goDecodes(t *testing.T, testdata string) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, kind := range []string{"ticket", "hold", "cancel"} {
		dir := filepath.Join(testdata, "bodies", kind)
		names, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if !strings.HasSuffix(n.Name(), ".body") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, n.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var v any
			switch kind {
			case "ticket":
				v, err = wire.DecodeTicket(raw)
			case "hold":
				v, err = wire.DecodeHold(raw)
			case "cancel":
				v, err = wire.DecodeCancel(raw)
			}
			key := "body " + kind + "/" + n.Name()
			if err != nil {
				out[key] = nil
				continue
			}
			body, err := wire.Encode(v)
			if err != nil {
				t.Fatalf("%s: re-encoding the decoded value: %v", key, err)
			}
			out[key] = decodeNumbers(t, body)
		}
	}
	var names struct {
		Valid         []struct{ Name string } `json:"valid"`
		Invalid       []struct{ Name string } `json:"invalid"`
		CancelValid   []struct{ Name string } `json:"cancel_valid"`
		CancelInvalid []struct{ Name string } `json:"cancel_invalid"`
	}
	data, err := os.ReadFile(filepath.Join(testdata, "names.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &names); err != nil {
		t.Fatal(err)
	}
	fields := func(id wire.ID) any {
		return []any{fmt.Sprint(id.OrderKey), json.Number(fmt.Sprint(id.PID)), json.Number(fmt.Sprint(id.Seq))}
	}
	for _, c := range append(names.Valid, names.Invalid...) {
		id, ok := wire.ParseName(c.Name)
		out["name "+c.Name] = nil
		if ok {
			out["name "+c.Name] = fields(id)
		}
	}
	for _, c := range append(names.CancelValid, names.CancelInvalid...) {
		id, ok := wire.ParseID(c.Name)
		out["cancel "+c.Name] = nil
		if ok {
			out["cancel "+c.Name] = fields(id)
		}
	}
	return out
}

func show(v any) string {
	if v == nil {
		return "malformed"
	}
	b, _ := json.Marshal(v)
	return string(b)
}
