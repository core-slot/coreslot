package coreslot

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// contractInventory is every item the contract inventory names, plus
// the two PROTOCOL.md adds for the state-directory override:
// `config.toml` and `CORESLOT_STATE_DIR`. needle is what the section a row links to must mention,
// so a row cannot point at a section that does not define its item.
var contractInventory = []struct{ item, needle string }{
	{"FORMAT", "FORMAT"},
	{"admit.lock", "admit.lock"},
	{"gen", "gen"},
	{"res/", "res/"},
	{"queue/", "queue/"},
	{"holds/", "holds/"},
	{"cancel/", "cancel/"},
	{"degraded", "degraded"},
	{"log/", "log/"},
	{"config.toml", "config.toml"},
	{"ticket body", "ticket"},
	{"hold body", "hold"},
	{"cancel body", "cancel"},
	{"CORESLOT_GRANTED_*", "CORESLOT_GRANTED_"},
	{"CORESLOT_HELD_FD", "CORESLOT_HELD_FD"},
	{"CORESLOT_TOKEN_FDS", "CORESLOT_TOKEN_FDS"},
	{"CORESLOT_UNBUDGETED", "CORESLOT_UNBUDGETED"},
	{"CORESLOT_STATE_DIR", "CORESLOT_STATE_DIR"},
	{"enqueue", "enqueue"},
	{"refresh", "refresh"},
	{"admission", "admission"},
	{"clamp", "clamp"},
	{"cancellation", "cancellation"},
	{"withdrawal", "withdraw"},
	{"reconciliation", "reconciliation"},
	{"adoption", "adopt"},
	{"token-only admission", "token-only"},
	{"pass-through", "pass-through"},
}

// protocolSection is one heading of PROTOCOL.md with the text up to the next heading of the same
// or a higher level, subsections included.
type protocolSection struct {
	level int
	text  string
}

// githubSlug is the anchor GitHub gives a Markdown heading: lower case, every character other than
// a letter, digit, space, hyphen or underscore dropped, spaces turned into hyphens.
func githubSlug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// protocolSections indexes PROTOCOL.md's headings by their GitHub anchor.
func protocolSections(doc string) map[string]protocolSection {
	lines := strings.Split(doc, "\n")
	heading := regexp.MustCompile(`^(#{1,6}) (.+)$`)
	type mark struct {
		line, level int
		slug        string
	}
	var marks []mark
	fenced := false
	for i, l := range lines {
		if strings.HasPrefix(l, "```") {
			fenced = !fenced
			continue
		}
		if m := heading.FindStringSubmatch(l); m != nil && !fenced {
			marks = append(marks, mark{i, len(m[1]), githubSlug(m[2])})
		}
	}
	out := map[string]protocolSection{}
	for i, m := range marks {
		end := len(lines)
		for _, n := range marks[i+1:] {
			if n.level <= m.level {
				end = n.line
				break
			}
		}
		out[m.slug] = protocolSection{m.level, strings.Join(lines[m.line:end], "\n")}
	}
	return out
}

// TestProtocolCarriesTheContractInventory greps PROTOCOL.md's contract inventory (its section 2)
// for every item of contractInventory: each has exactly one row, each row links only to headings
// that exist, and at least one linked section mentions the item. The table has no row the
// inventory does not name, so an item renamed in one place and not the other fails here.
func TestProtocolCarriesTheContractInventory(t *testing.T) {
	b, err := os.ReadFile("PROTOCOL.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	sections := protocolSections(doc)
	inventory, ok := sections["2-contract-inventory"]
	if !ok {
		t.Fatal(`PROTOCOL.md has no "## 2. Contract inventory" heading`)
	}

	link := regexp.MustCompile(`\]\(#([^)]+)\)`)
	parenthetical := regexp.MustCompile(`\s*\([^)]*\)$`)
	rows := map[string][]string{} // item -> anchors its "Defined in" cell links to
	count := map[string]int{}
	for _, l := range strings.Split(inventory.text, "\n") {
		cells := strings.Split(l, "|")
		if !strings.HasPrefix(l, "| ") || len(cells) != 6 {
			continue
		}
		item := strings.TrimSpace(parenthetical.ReplaceAllString(strings.TrimSpace(cells[1]), ""))
		item = strings.ReplaceAll(item, "`", "")
		if item == "Item" {
			continue
		}
		count[item]++
		for _, m := range link.FindAllStringSubmatch(cells[3], -1) {
			rows[item] = append(rows[item], m[1])
		}
	}

	known := map[string]bool{}
	for _, want := range contractInventory {
		known[want.item] = true
		anchors, present := rows[want.item]
		if !present && count[want.item] == 0 {
			t.Errorf("contract inventory has no row for %q", want.item)
			continue
		}
		if count[want.item] > 1 {
			t.Errorf("contract inventory has %d rows for %q, want one", count[want.item], want.item)
		}
		if len(anchors) == 0 {
			t.Errorf("row %q links to no section", want.item)
			continue
		}
		mentioned := false
		for _, a := range anchors {
			s, ok := sections[a]
			if !ok {
				t.Errorf("row %q links to #%s, which is no heading of PROTOCOL.md", want.item, a)
				continue
			}
			if strings.Contains(strings.ToLower(s.text), strings.ToLower(want.needle)) {
				mentioned = true
			}
		}
		if !mentioned {
			t.Errorf("row %q: no linked section (%v) mentions %q", want.item, anchors, want.needle)
		}
	}
	for item := range count {
		if !known[item] {
			t.Errorf("contract inventory row %q is not an item of contractInventory", item)
		}
	}
}

// The slug rule matches the anchors GitHub generates for PROTOCOL.md's own headings, including
// the ones with dots, slashes and parentheses.
func TestGithubSlug(t *testing.T) {
	for heading, want := range map[string]string{
		"2. Contract inventory":         "2-contract-inventory",
		"7.7 admit.lock":                "77-admitlock",
		"7.10 log/ (run log)":           "710-log-run-log",
		"10.2 Refresh and STALLED":      "102-refresh-and-stalled",
		"18. Nested runs: pass-through": "18-nested-runs-pass-through",
	} {
		if got := githubSlug(heading); got != want {
			t.Errorf("githubSlug(%q) = %q, want %q", heading, got, want)
		}
	}
}
