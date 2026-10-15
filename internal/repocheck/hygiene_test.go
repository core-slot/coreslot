package repocheck

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Contract: every Markdown file name the repository writes names a file of the repository. A
// Markdown name is any token ending in ".md" or ".markdown", in any case, in prose, code, links or
// backticks; it resolves relative to the file it appears in, relative to the root, or, for a bare
// name, to any file of that name. A URL's Markdown name is another site's and is out of scope, and
// so is a name split across a line break: matching is per line. This file is scanned like every
// other, so its test cases are spelled with a "|" removed before matching. A pattern file
// (.gitignore, .gitattributes) is not checked for them: its names match files, whether or not
// they exist, and refer to none.
var (
	// mdName is a Markdown file name or path; urlRE is a URL, whose Markdown name is another site's.
	mdName = regexp.MustCompile(`(?i)[A-Za-z0-9_./-]*[A-Za-z0-9_-]\.(md|markdown)\b`)
	urlRE  = regexp.MustCompile(`[a-z]+://\S+`)
)

// resolves reports whether ref, a Markdown name found in file, names a file of the repository:
// relative to file's directory, relative to the root, or, for a bare name, any file of that name.
func resolves(ref, file string, files, bases map[string]bool) bool {
	if files[path.Join(path.Dir(file), ref)] || files[path.Clean(ref)] {
		return true
	}
	return !strings.Contains(ref, "/") && bases[ref]
}

// patternFile reports whether file holds path patterns rather than references to files.
func patternFile(file string) bool {
	base := path.Base(file)
	return base == ".gitignore" || base == ".gitattributes"
}

// danglingMarkdown returns the Markdown names in line of file that name no file of the repository,
// or nil. files and bases are the repository's files and their base names.
func danglingMarkdown(file, line string, files, bases map[string]bool) []string {
	var out []string
	for _, ref := range mdName.FindAllString(urlRE.ReplaceAllString(line, ""), -1) {
		if !resolves(ref, file, files, bases) && !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	return out
}

// The matcher itself: a name that resolves nowhere is caught, in every spelling, and a name that
// resolves (relative, from the root, bare, with an anchor) or sits in a URL is not.
func TestMarkdownNameMatcher(t *testing.T) {
	unbar := func(s string) string { return strings.ReplaceAll(s, "|", "") }
	files := map[string]bool{"PROTOCOL.md": true, "docs/compatibility.md": true, "evidence/README.md": true}
	bases := map[string]bool{"PROTOCOL.md": true, "compatibility.md": true, "README.md": true}
	mustCatch := []string{
		"see TOD|O.m|d",
		"the x-guid|e.m|d file",
		"[notes](NOTE|S.m|d)",
		"`docs/arch|itecture.m|d`",
		"see HISTOR|Y.M|D",
		"see notes/setu|p.markdow|n",
		"[compat](../compatibility.m|d), which climbs out of docs/ to the root",
	}
	inDocs := unbar("docs/x.m|d") // a file under docs/, for the relative links
	for _, c := range mustCatch {
		if len(danglingMarkdown(inDocs, unbar(c), files, bases)) == 0 {
			t.Errorf("%q is not caught", unbar(c))
		}
	}
	mustPass := []string{
		"PROTOCOL.md section 12.2: a request above the size waits",
		"[policy](../PROTOCOL.m|d#151-what-every-format-keeps) and `evidence/README.md`",
		"[compat](compatibility.m|d)",
		"README.md, https://example.com/x/NOTE|S.m|d",
		"go 1.26.0",
	}
	for _, c := range mustPass {
		if f := danglingMarkdown(inDocs, unbar(c), files, bases); len(f) != 0 {
			t.Errorf("%q is caught: %v", unbar(c), f)
		}
	}
	if f := danglingMarkdown("x.go", unbar("TOD|O.m|d and NOTE|S.m|d, TOD|O.m|d again"), files, bases); len(f) != 2 {
		t.Errorf("two names on one line, one twice, give %v, want two findings", f)
	}
}

// Contract: no file of this repository records a path of the machine it was written or built on.
// The guarantee covers a closed set of forms, and only those, in every file of the tree: in text
// as written, in a gzip stream (a pprof profile) after decompression, and in any other binary
// file as its raw bytes, which shows only strings stored uncompressed.
//
//   - a home directory, /Users/<name> or /home/<name>, unless <name> is one of placeholderUsers;
//   - a Windows home directory, <drive>:\Users\<name>\;
//   - a macOS temporary or per-user directory: /private/tmp, /private/var or /var/folders, each
//     followed by a slash;
//   - a numbered work directory name, task<N> or task-<N>, and a path flattened into a name,
//     -Users-<name>-;
//   - in a gzip stream or a binary file only, /tmp/ and a source tree under /opt, /usr or /nix (a
//     path with a /src/ in it) too: a profile built with -trimpath names its binary by its base
//     name and its sources by import path (runtime/proc.go,
//     golang.org/x/sys@v0.49.0/unix/syscall_bsd.go), so a /tmp/ there is a build directory and an
//     absolute source path is the toolchain's install directory, while the documentation's /tmp/
//     examples are placeholders.
//
// The patterns' sources do not match themselves, and the test cases are spelled with a "|"
// removed before matching, so this file passes its own scan.
var localPathForms = []struct {
	why string
	re  *regexp.Regexp
}{
	{"a Windows home directory", regexp.MustCompile(`(?i)\b[a-z]:\\User[s]\\[^\\\s]+\\`)},
	{"a macOS temporary or per-user directory", regexp.MustCompile(`/privat[e]/(tmp|var)/|/var/folder[s]/`)},
	{"a path flattened into a name", regexp.MustCompile(`-User[s]-[A-Za-z0-9_.]+-`)},
	{"a numbered work directory name", regexp.MustCompile(`\bta[s]k-?\d+\b`)},
}

var (
	// homeDir is a home directory and its user name.
	homeDir = regexp.MustCompile(`/(User[s]|hom[e])/([A-Za-z0-9_][A-Za-z0-9_.-]*)`)
	// binaryOnly are the forms only a gzip stream or a binary file is checked for.
	binaryOnly = []struct {
		why string
		re  *regexp.Regexp
	}{
		{"a temporary build directory", regexp.MustCompile(`/tm[p]/`)},
		{"a toolchain source path", regexp.MustCompile(`/(op[t]|us[r]|ni[x])/\S*/sr[c]/`)},
	}
	// placeholderUsers are the user names the documentation uses in example paths: PROTOCOL.md's
	// ana, and tester, the unprivileged user of scripts/linux-test.sh.
	placeholderUsers = []string{"ana", "tester"}
)

// localPathFindings returns the distinct local paths in s, or nil. binary selects the forms that
// apply to a gzip stream or a binary file.
func localPathFindings(s string, binary bool) []string {
	var out []string
	add := func(why string) {
		if !slices.Contains(out, why) {
			out = append(out, why)
		}
	}
	for _, m := range homeDir.FindAllStringSubmatch(s, -1) {
		if !slices.Contains(placeholderUsers, m[2]) {
			add("a home directory (" + m[0] + ")")
		}
	}
	for _, f := range localPathForms {
		if m := f.re.FindString(s); m != "" {
			add(f.why + " (" + m + ")")
		}
	}
	if binary {
		for _, f := range binaryOnly {
			if m := f.re.FindString(s); m != "" {
				add(f.why + " (" + m + ")")
			}
		}
	}
	return out
}

// maxInflated bounds a gzip stream's decompressed size, so a corrupt file cannot exhaust memory.
const maxInflated = 64 << 20

// isGzip reports whether b starts with the gzip magic number.
func isGzip(b []byte) bool { return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b }

// binaryLocalPaths returns the local paths in b, a gzip stream or another binary file, as
// "offset: finding" entries. A gzip stream is decompressed first, every member of it, and a
// stream that does not decompress is an error, since its content would go unscanned.
func binaryLocalPaths(b []byte) ([]string, error) {
	data := b
	if isGzip(b) {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		data, err = io.ReadAll(io.LimitReader(zr, maxInflated+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maxInflated {
			return nil, fmt.Errorf("decompresses to more than %d bytes", maxInflated)
		}
	}
	// Scan each run of printable bytes, as strings(1) shows them: a protobuf string is stored
	// whole, between its length and the next field's tag.
	var out []string
	start := -1
	for i := 0; i <= len(data); i++ {
		if i < len(data) && ((data[i] >= 0x20 && data[i] != 0x7f) || data[i] == '\t') {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			for _, f := range localPathFindings(string(data[start:i]), true) {
				out = append(out, fmt.Sprintf("byte %d: %s", start, f))
			}
			start = -1
		}
	}
	return out, nil
}

// gzipped compresses s, for the tests.
func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The matcher itself: every closed form is caught, and the placeholders and import paths that
// share its words are not. A "|" in a case is removed before matching.
func TestLocalPathMatcher(t *testing.T) {
	unbar := func(s string) string { return strings.ReplaceAll(s, "|", "") }
	mustCatch := []string{
		"/Use|rs/somebody/Dev/coreslot/main.go",
		"/Use|rs/somebody",
		"cd /hom|e/somebody/src",
		"HOME=/hom|e/somebody",
		`C:\Use|rs\somebody\go`,
		"/priva|te/tmp/x/bin/waiters",
		"/priva|te/var/folde|rs/ab/T/go-build1",
		"/var/folde|rs/ab/T/",
		"-Use|rs-somebody-Dev-coreslot",
		"/tmp/x/coreslot-tas|k18/bin/waiters",
		"tas|k-7",
	}
	for _, c := range mustCatch {
		if len(localPathFindings(unbar(c), false)) == 0 {
			t.Errorf("text %q is not caught", unbar(c))
		}
		if len(localPathFindings(unbar(c), true)) == 0 {
			t.Errorf("binary %q is not caught", unbar(c))
		}
	}
	mustPass := []string{
		"/hom|e/ana/.local/state/coreslot",
		"HOME=/hom|e/tester GOPATH=/hom|e/tester/go",
		"/tmp/b/waiters -coreslot /tmp/b/coreslot",
		"github.com/core-slot/coreslot/internal/engine/wait.go",
		"golang.org/x/sys@v0.49.0/unix/syscall_bsd.go",
		"runtime/proc.go",
		"macOS keeps home directories under /Use|rs/.",
		"a task 2 s later; multitask3",
		"/opt/homebrew/Cellar/go/1.27.2/libexec/src/runtime/slice.go",
	}
	for _, c := range mustPass {
		if f := localPathFindings(unbar(c), false); len(f) != 0 {
			t.Errorf("text %q is caught: %v", unbar(c), f)
		}
	}
	for _, c := range []string{"/tmp/b/waiters", "/opt/homebrew/Cellar/go/1.27.2/libexec/src/runtime/slice.go2)",
		"/usr/local/go/src/runtime/asm_amd64.s2", "/nix/store/x-go/share/go/src/os/file.go"} {
		if f := localPathFindings(c, true); len(f) != 1 {
			t.Errorf("binary %q gives %v, want one finding", c, f)
		}
	}
	for _, c := range []string{"/usr/lib/system/libsystem_kernel.dylib", "/usr/lib/x86_64-linux-gnu/libc.so.6"} {
		if f := localPathFindings(c, true); len(f) != 0 {
			t.Errorf("a system library mapping %q is caught: %v", c, f)
		}
	}
	if f := localPathFindings(unbar("/Use|rs/somebody/x and /priva|te/tmp/y"), false); len(f) != 2 {
		t.Errorf("two forms on one line give %d findings, want 2: %v", len(f), f)
	}

	// A gzip stream is scanned after decompression; its compressed bytes hide the path. Deflate
	// stores a short input as it is, so the fixture repeats enough to be compressed for real.
	pad := strings.Repeat("\x32\x0bruntime/x.go", 64)
	leak := pad + "\x32" + unbar("\x24/Use|rs/somebody/Dev/x.go") + pad
	z := gzipped(t, leak)
	if bytes.Contains(z, []byte("somebody")) {
		t.Fatal("the gzip fixture keeps its plaintext, so it cannot show decompression")
	}
	if f, err := binaryLocalPaths(z); err != nil || len(f) != 1 {
		t.Errorf("a gzipped profile string gives %v, %v; want one finding", f, err)
	}
	if f, err := binaryLocalPaths(gzipped(t, pad)); err != nil || len(f) != 0 {
		t.Errorf("a clean gzipped profile gives %v, %v; want nothing", f, err)
	}
	if f, err := binaryLocalPaths([]byte(leak)); err != nil || len(f) != 1 {
		t.Errorf("an uncompressed binary gives %v, %v; want one finding", f, err)
	}
	if _, err := binaryLocalPaths(z[:len(z)/2]); err == nil {
		t.Error("a truncated gzip stream is not an error")
	}
	two := append(gzipped(t, "clean"), gzipped(t, leak)...)
	if f, err := binaryLocalPaths(two); err != nil || len(f) != 1 {
		t.Errorf("a second gzip member gives %v, %v; want one finding", f, err)
	}
}

// minTextFiles is a floor on the text files a scan checks: about 500 exist, and a listing that
// lost most of the tree would otherwise pass on what is left.
const minTextFiles = 200

// listFiles lists the repository's files relative to the module root: tracked and
// untracked-but-not-ignored files when git can list them (git true), otherwise every file under
// the root outside .git and the build outputs .gitignore ignores (dist, Python bytecode).
func listFiles(t *testing.T) (files []string, git bool) {
	t.Helper()
	if _, err := exec.LookPath("git"); err == nil {
		cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		if out, err := cmd.Output(); err == nil {
			for _, f := range strings.Split(string(out), "\x00") {
				if f != "" {
					files = append(files, f)
				}
			}
			return files, true
		}
	}
	return walkFiles(t, nil), false
}

// walkFiles lists every regular file under the module root outside .git, dist and Python bytecode
// (__pycache__ directories, *.pyc files: a default `python3 -m unittest` writes them, embedding
// the checkout's path, and .gitignore ignores them) whose name keep accepts (every file when keep
// is nil).
func walkFiles(t *testing.T, keep func(string) bool) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == ".git" && !d.IsDir() {
			return nil // a worktree's .git file, which points at the repository
		}
		if d.IsDir() {
			if p == ".git" || p == "dist" || d.Name() == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && !strings.HasSuffix(p, ".pyc") && (keep == nil || keep(p)) {
			files = append(files, filepath.ToSlash(p))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// binaryText reports whether b is binary rather than text: a NUL byte in its first 8000 bytes. A
// binary file is scanned for local paths only, not for Markdown names.
func binaryText(b []byte) bool { return bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 }

// checkListing fails when a file under the module root that git does not ignore is missing from
// listed, so a listing that lost files cannot make the scan pass. Every file is scanned for local
// paths, binary files included, so every file counts.
func checkListing(t *testing.T, listed []string) {
	t.Helper()
	have := map[string]bool{}
	for _, f := range listed {
		have[f] = true
	}
	var unlisted []string
	for _, f := range walkFiles(t, nil) {
		if !have[f] {
			unlisted = append(unlisted, f)
		}
	}
	if len(unlisted) == 0 {
		return
	}
	cmd := exec.Command("git", "check-ignore", "-z", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(unlisted, "\x00") + "\x00")
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); err != nil && !(ok && ee.ExitCode() == 1) { // 1: none ignored
		t.Fatalf("git check-ignore: %v", err)
	}
	ignored := map[string]bool{}
	for _, f := range strings.Split(string(out), "\x00") {
		ignored[f] = true
	}
	for _, f := range unlisted {
		if !ignored[f] {
			t.Errorf("%s is in the tree, not ignored, and missing from the listing", f)
		}
	}
}

// TestRepositoryHygiene scans every file of the repository, this one included: a text file for
// Markdown names that resolve nowhere and for local paths, a gzip stream (a pprof profile) and any
// other binary file for local paths.
func TestRepositoryHygiene(t *testing.T) {
	listed, git := listFiles(t)
	if git {
		checkListing(t, listed)
	}
	files, bases := map[string]bool{}, map[string]bool{}
	for _, f := range listed {
		files[f] = true
		bases[path.Base(f)] = true
	}
	text, gz, binary := 0, 0, 0
	var missing, unreadable []string
	for _, f := range listed {
		b, err := os.ReadFile(f)
		if err != nil {
			if os.IsNotExist(err) {
				// Deleted in the working tree but still in the index: it would go unscanned.
				missing = append(missing, f)
				continue
			}
			t.Fatal(err)
		}
		if isGzip(b) || binaryText(b) {
			found, err := binaryLocalPaths(b)
			if err != nil {
				unreadable = append(unreadable, f)
				t.Errorf("%s: %v: its content is not scanned", f, err)
				continue
			}
			for _, why := range found {
				t.Errorf("%s: %s: write the path the way a -trimpath build records it", f, why)
			}
			if isGzip(b) {
				gz++
			} else {
				binary++
			}
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			if refs := danglingMarkdown(f, line, files, bases); refs != nil && !patternFile(f) {
				t.Errorf("%s:%d: %s: names no file of the repository", f, i+1, strings.Join(refs, ", "))
			}
			if why := localPathFindings(line, false); why != nil {
				t.Errorf("%s:%d: %s: use a placeholder path", f, i+1, strings.Join(why, "; "))
			}
		}
		text++
	}
	for _, f := range missing {
		t.Errorf("%s is listed but missing from the working tree, so it is not scanned", f)
	}
	if want := len(listed) - len(missing) - len(unreadable); text+gz+binary != want {
		t.Fatalf("scanned %d files (%d text, %d gzip, %d other binary), want %d (%d listed, %d missing, %d unreadable)",
			text+gz+binary, text, gz, binary, want, len(listed), len(missing), len(unreadable))
	}
	t.Logf("scanned %d text files, %d gzip streams and %d other binary files (%d listed, git listing %v)",
		text, gz, binary, len(listed), git)
	if text < minTextFiles {
		t.Fatalf("checked %d text files, fewer than %d: the listing lost the tree", text, minTextFiles)
	}
}
