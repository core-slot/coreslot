package coreslot

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// citationGuardFile is this file: it spells out every pattern it forbids, so it skips itself.
const citationGuardFile = "public_citations_test.go"

// forbiddenCitations are references to the private design notes the public repository must never
// carry. PROTOCOL.md is the only normative text: a comment or document states the rule itself or
// points at the PROTOCOL.md section that states it. The invariants I1 to I7 are PROTOCOL.md's own
// and stay citable.
var forbiddenCitations = []struct {
	why string
	re  *regexp.Regexp
}{
	{"a path of the private design notes", regexp.MustCompile(`docs/(specs|plans)/`)},
	{"the private design document's name", regexp.MustCompile(`coreslot-design`)},
	{"a section of the private design", regexp.MustCompile(`(?i)\b(spec|design) §`)},
	{"a section of the private design", regexp.MustCompile(`(?i)\b(spec|design) sections?\b`)},
}

// decisionID is a decision id of the private design, D1 to D12. The word boundaries keep it off
// hex (`0xD1`), identifiers and longer ids (`D13`, `D1a`); nothing else in the tree uses these
// tokens, except conformance/pyclient/AMBIGUITIES.md, which numbers its own entries A1 to F3 by
// section, and the Python client and its tests, which cite those entries (`B1/D6`, `D8, §15.2`).
// Its section D ends at D9, so under conformance/pyclient/ only D10 to D12 are decision ids and
// pyclientDecisionID is the narrower pattern applied there.
var (
	decisionID         = regexp.MustCompile(`\bD(1[0-2]|[1-9])\b`)
	pyclientDecisionID = regexp.MustCompile(`\bD1[0-2]\b`)
)

// publicFiles lists the files the repository publishes, relative to the module root: tracked and
// untracked-but-not-ignored files when git can list them, otherwise every file under the module
// root outside .git and dist.
func publicFiles(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("git"); err == nil {
		cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		if out, err := cmd.Output(); err == nil {
			var files []string
			for _, f := range strings.Split(string(out), "\x00") {
				if f != "" {
					files = append(files, f)
				}
			}
			return files
		}
	}
	var files []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == ".git" || path == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			files = append(files, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestNoPrivateDesignCitations fails on any citation of the private design notes in a text file
// of the repository: their paths, the design's name, "spec §N" or "design section N", and the
// design's decision ids D1 to D12.
func TestNoPrivateDesignCitations(t *testing.T) {
	files := publicFiles(t)
	if len(files) == 0 {
		t.Fatal("no files to check")
	}
	checked := 0
	for _, f := range files {
		if f == citationGuardFile {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			if os.IsNotExist(err) {
				continue // deleted in the working tree, still in the index
			}
			t.Fatal(err)
		}
		if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			continue // binary (the pprof profiles)
		}
		checked++
		ids := decisionID
		if strings.HasPrefix(f, "conformance/pyclient/") {
			ids = pyclientDecisionID
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, c := range forbiddenCitations {
				if m := c.re.FindString(line); m != "" {
					t.Errorf("%s:%d: %s (%q): state the rule or cite PROTOCOL.md", f, i+1, c.why, m)
				}
			}
			if m := ids.FindString(line); m != "" {
				t.Errorf("%s:%d: a decision id of the private design (%q): state the decision or cite PROTOCOL.md", f, i+1, m)
			}
		}
	}
	if checked < 50 {
		// A listing that missed the tree would pass vacuously.
		t.Fatalf("checked only %d text files", checked)
	}
}
