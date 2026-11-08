package memsample

import (
	"os/exec"
	"regexp"
	"strconv"
	"testing"
)

// vmStatMiB runs vm_stat and returns its page counts, in MiB, by the label before the colon.
func vmStatMiB(t *testing.T) map[string]int64 {
	t.Helper()
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		t.Skipf("vm_stat: %v", err)
	}
	size := int64(16384)
	if m := regexp.MustCompile(`page size of (\d+) bytes`).FindSubmatch(out); m != nil {
		size, _ = strconv.ParseInt(string(m[1]), 10, 64)
	}
	got := map[string]int64{}
	for _, m := range regexp.MustCompile(`(?m)^"?([^":\n]+)"?:\s+(\d+)\.$`).FindAllSubmatch(out, -1) {
		n, _ := strconv.ParseInt(string(m[2]), 10, 64)
		got[string(m[1])] = n * size >> 20
	}
	return got
}

// wired and compressor agree with vm_stat within 256 MiB or 10 %, whichever is larger. Mutations:
// reading the 32-bit field one word along (wire_count at 4) or taking pages for MiB without the
// page size turns it red.
func TestDarwinPartsMatchVmStat(t *testing.T) {
	parts, _, ok := Parts()
	if !ok {
		t.Fatal("Parts not ok")
	}
	vm := vmStatMiB(t)
	t.Logf("parts %v\nvm_stat MiB %v", parts, vm)
	for part, label := range map[string]string{"wired": "Pages wired down", "compressor": "Pages occupied by compressor",
		"internal": "Anonymous pages", "external": "File-backed pages"} {
		want, have := vm[label], parts[part]
		tol := max(int64(256), want/10)
		if have < want-tol || have > want+tol {
			t.Errorf("%s = %d MiB, vm_stat %q = %d MiB (tolerance %d)", part, have, label, want, tol)
		}
	}
	for _, k := range []string{"free", "speculative", "purgeable"} {
		if _, found := parts[k]; !found {
			t.Errorf("no %s part in %v", k, parts)
		}
	}
	// free counts speculative pages, which vm_stat lists apart from its free.
	if parts["free"] < vm["Pages free"]-256 {
		t.Errorf("free = %d MiB, below vm_stat's free %d MiB", parts["free"], vm["Pages free"])
	}
}

// The kernel's pressure level is 1, 2 or 4. Mutation: returning the raw count of levels (3) or 0.
func TestPressureLevelKnown(t *testing.T) {
	_, level, ok := Parts()
	if !ok || level != 1 && level != 2 && level != 4 {
		t.Errorf("pressure %d, ok %v; want 1, 2 or 4", level, ok)
	}
}
