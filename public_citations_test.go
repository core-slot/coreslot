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

// The repository must be readable on its own: PROTOCOL.md is its only normative text, and a
// comment, test or document states a rule itself or cites the PROTOCOL.md section that states it.
// This test fails on citations of documents that are not part of this repository: their paths,
// their section numbers, their identifiers for decisions and review items, and the process
// language around them. The invariants I1 to I7 are PROTOCOL.md's own and stay citable.
//
// The patterns about specifications and designs need a section marker (a § sign, "section N", or
// a dotted number N.N) close by, so ordinary prose ("the design of the queue", "a spec-compliant
// client", the Go package spec) does not match. This file is scanned like every other, so no
// literal it forbids appears in it: the patterns are regular expressions whose source does not
// match itself, and the test cases are spelled with a "|" that is removed before matching.
var forbiddenCitations = []struct {
	why string
	re  *regexp.Regexp
}{
	{"a path of documents outside this repository", regexp.MustCompile(`docs/(specs|plans)/`)},
	{"the name of a document outside this repository", regexp.MustCompile(`coreslot-(design)\b`)},
	// The word, an optional possessive or comma, then a section marker within three words.
	{"a section of a specification or design", regexp.MustCompile(
		`(?i)\b(spec|design)s?('s|’s)?,?(\s+[^\s§]+){0,3}?\s+(§|sections?\s+\d|\d+\.\d+)`)},
	// The word directly before a section sign or the word "section", with or without a number.
	{"a section of a specification or design", regexp.MustCompile(`(?i)\b(spec|design)('s|’s)?\s+(§|sections?\b)`)},
	// A section number, then "of the" and the word.
	{"a section of a specification or design", regexp.MustCompile(
		`(?i)(§|\bsections?)\s*\d+(\.\d+)*\s+of\s+(the\s+)?(spec|design)\b`)},
	{"a specification or design cited as authority", regexp.MustCompile(`(?i)\bper\s+(the\s+)?(spec|design)\b`)},
	{"a document revision", regexp.MustCompile(`(?i)\brev(ision)?\s*4[a-z]?\b`)},
	{"a review item id", regexp.MustCompile(`\bR-[A-Z]\d+\b`)},
	{"an owner-decision id", regexp.MustCompile(`\bO[1-9]\d?\b`)},
	{"process language", regexp.MustCompile(`(?i)\b(owner('s|’s)?\s+(decision|ruling)|controller\s+ruling)\b`)},
}

// decisionID is a capital D followed by a number from one to twelve, as a word of its own. The word
// boundaries keep it off hex digits, "3D", colour codes, identifiers and longer ids.
// conformance/pyclient/AMBIGUITIES.md numbers its own entries by section letter and number, and the
// Python client and its tests cite them; its section D stops at nine, so under conformance/pyclient/
// only ten to twelve match, through pyclientDecisionID. A product name like d3.js written with a
// capital D would need an entry in citationAllowlist.
var (
	decisionID         = regexp.MustCompile(`\bD(1[0-2]|[1-9])\b`)
	pyclientDecisionID = regexp.MustCompile(`\bD1[0-2]\b`)
)

// citationAllowlist exempts, per file, the lines that contain one of the given substrings.
var citationAllowlist = map[string][]string{}

// citationFindings returns why line of file is a forbidden citation, one entry per pattern hit.
func citationFindings(file, line string) []string {
	for _, ok := range citationAllowlist[file] {
		if strings.Contains(line, ok) {
			return nil
		}
	}
	var out []string
	for _, c := range forbiddenCitations {
		if m := c.re.FindString(line); m != "" {
			out = append(out, c.why+" ("+m+")")
		}
	}
	ids := decisionID
	if strings.HasPrefix(file, "conformance/pyclient/") {
		ids = pyclientDecisionID
	}
	if m := ids.FindString(line); m != "" {
		out = append(out, "a decision id ("+m+")")
	}
	return out
}

// The matcher itself: every evasion form is caught, and the prose and code that share its words
// are not. A "|" in a case is removed before matching, so this file does not cite anything.
func TestCitationMatcher(t *testing.T) {
	bad := []string{
		"spe|c §5.7",
		"desi|gn section 10.4",
		"D|11: clamped",
		"§5.7 of the desi|gn",
		"the spe|c, §5.7",
		"the desi|gn's section 10.4",
		"(desi|gn 10.4)",
		"spe|c 6.2",
		"per the spe|c",
		"re|v 4c",
		"O|3",
		"R-|C1",
		"spe|c sections 5.10 and 6.2",
		"see docs/spe|cs/x.md",
		"owner deci|sion",
		"controller rul|ing",
		"Desi|gn §8.1",
	}
	for _, c := range bad {
		line := strings.ReplaceAll(c, "|", "")
		if len(citationFindings("x.go", line)) == 0 {
			t.Errorf("%q is not caught", line)
		}
	}
	good := []string{
		"the design of the queue keeps every waiter's timers on the front",
		"a spec-compliant client written from PROTOCOL.md section 10.4",
		"func need(spec string) spec.Request { return spec.Range{Min: 1, Max: 2} }",
		"PROTOCOL.md section 12.2: a request above the size waits",
		"go 1.26.0",
		"0xD1, 3D, #D1D1D1 and the C1 controls",
		"the design choice: 1 token per index",
	}
	for _, c := range good {
		if f := citationFindings("x.go", c); len(f) != 0 {
			t.Errorf("%q is caught: %v", c, f)
		}
	}
	if f := citationFindings("conformance/pyclient/AMBIGUITIES.md", strings.ReplaceAll("**D|6. §10.4 (B1/D|6)", "|", "")); len(f) != 0 {
		t.Errorf("an AMBIGUITIES entry id is caught: %v", f)
	}
	if f := citationFindings("conformance/pyclient/test.py", strings.ReplaceAll("D|12", "|", "")); len(f) == 0 {
		t.Error("a decision id above nine under conformance/pyclient is not caught")
	}
}

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

// TestNoExternalCitations scans every text file of the repository, this one included, with
// citationFindings.
func TestNoExternalCitations(t *testing.T) {
	files := publicFiles(t)
	if len(files) == 0 {
		t.Fatal("no files to check")
	}
	checked, binary, missing := 0, 0, 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			if os.IsNotExist(err) {
				missing++ // deleted in the working tree, still in the index
				continue
			}
			t.Fatal(err)
		}
		if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			binary++ // the pprof profiles
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, why := range citationFindings(f, line) {
				t.Errorf("%s:%d: %s: state the rule or cite PROTOCOL.md", f, i+1, why)
			}
		}
		checked++
	}
	if want := len(files) - binary - missing; checked != want {
		t.Fatalf("checked %d text files, want %d (%d listed, %d binary, %d missing)",
			checked, want, len(files), binary, missing)
	}
}
