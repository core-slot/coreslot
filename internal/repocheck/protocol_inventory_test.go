package repocheck

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// contractInventory is every shared file, field, environment variable and transition of format 1,
// the items PROTOCOL.md's contract inventory (its section 2) must list, including the two the
// state-directory override adds (section 4.1): `config.toml` and `CORESLOT_STATE_DIR`. The list is
// kept here, apart from the document, so that an item added to, dropped from or renamed in only one
// of the two fails the test. title is the heading of the section that defines the item, without its
// number; a row must link to that heading, outside the worked examples. A word in the body is not
// enough: `gen`, `hold` or `res/` appear in many sections that do not define them. mention is what
// the defining section's text must contain.
var contractInventory = []struct{ item, title, mention string }{
	{"FORMAT", "FORMAT", "coreslot-protocol"},
	{"admit.lock", "admit.lock", "admit.lock"},
	{"gen", "gen", "reconciled_ns"},
	{"res/", "Tokens and descriptor ownership", "res/"},
	{"queue/", "Ticket", "queue/"},
	{"holds/", "Hold", "holds/"},
	{"cancel/", "Cancel request", "cancel/"},
	{"degraded", "degraded", "degraded"},
	{"log/", "log/ (run log)", "log/runs.jsonl"},
	{"config.toml", "Configuration file", "config.toml"},
	{"ticket body", "Ticket", "created_ns"},
	{"hold body", "Hold", "start_ns"},
	{"cancel body", "Cancel request", "requester_pid"},
	{"CORESLOT_GRANTED_*", "Descriptor and environment contract", "CORESLOT_GRANTED_"},
	{"CORESLOT_HELD_FD", "Descriptor and environment contract", "CORESLOT_HELD_FD"},
	{"CORESLOT_TOKEN_FDS", "Descriptor and environment contract", "CORESLOT_TOKEN_FDS"},
	{"CORESLOT_UNBUDGETED", "Errors, fail-open and degraded mode", "CORESLOT_UNBUDGETED"},
	{"CORESLOT_STATE_DIR", "Location", "CORESLOT_STATE_DIR"},
	{"enqueue", "Enqueue", "order key"},
	{"refresh", "Refresh and STALLED", "futimens"},
	{"admission", "Admission", "minimum pass"},
	{"clamp", "Effective request and clamp", "clamped"},
	{"cancellation", "Cancellation", "cancel/<ticket-id>"},
	{"withdrawal", "Withdrawal", "withdraws"},
	{"reconciliation", "Reconciliation", "reconciled_ns"},
	{"adoption", "Adoption", "\"adopted\": true"},
	{"token-only admission", "Token-only mode", "CORESLOT_UNBUDGETED=format"},
	{"pass-through", "Nested runs: pass-through", "pass-through"},
}

// examplesTitle is the title of PROTOCOL.md's worked examples, whose subsections illustrate the
// items and define none of them.
const examplesTitle = "Worked examples"

// protocolSection is one heading of PROTOCOL.md: its title without the section number, whether
// it lies inside the worked examples, and its text up to the next heading of the same or a higher
// level, subsections included.
type protocolSection struct {
	title   string
	example bool
	level   int
	text    string
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
	number := regexp.MustCompile(`^[0-9]+(\.[0-9]+)*\.? `)
	type mark struct {
		line, level int
		slug, title string
		example     bool
	}
	var marks []mark
	fenced, inExamples := false, false
	for i, l := range lines {
		if strings.HasPrefix(l, "```") {
			fenced = !fenced
			continue
		}
		m := heading.FindStringSubmatch(l)
		if m == nil || fenced {
			continue
		}
		level := len(m[1])
		if level == 2 {
			inExamples = number.ReplaceAllString(m[2], "") == examplesTitle
		}
		marks = append(marks, mark{i, level, githubSlug(m[2]), number.ReplaceAllString(m[2], ""),
			inExamples && level > 2})
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
		out[m.slug] = protocolSection{m.title, m.example, m.level, strings.Join(lines[m.line:end], "\n")}
	}
	return out
}

// TestProtocolCarriesTheContractInventory greps PROTOCOL.md's contract inventory (its section 2)
// for every item of contractInventory: each has exactly one row; every link of the row resolves to
// a heading; one of them is the heading that defines the item (not a worked example, and not a
// section that merely mentions the item), and that section's text carries the item. The table
// has no row the inventory does not name, so an item renamed in one place and not the other
// fails here.
func TestProtocolCarriesTheContractInventory(t *testing.T) {
	b, err := os.ReadFile("PROTOCOL.md")
	if err != nil {
		t.Fatal(err)
	}
	sections := protocolSections(string(b))
	inventory, ok := sections["2-contract-inventory"]
	if !ok {
		t.Fatal(`PROTOCOL.md has no "## 2. Contract inventory" heading`)
	}
	examples := 0
	for _, s := range sections {
		if s.level == 2 && s.title == examplesTitle {
			examples++
		}
	}
	if examples != 1 {
		// Without it no section counts as an example, and a row linking only its example passes.
		t.Fatalf("PROTOCOL.md has %d %q sections, want 1", examples, examplesTitle)
	}

	link := regexp.MustCompile(`\]\(#([^)]+)\)`)
	parenthetical := regexp.MustCompile(`\s*\([^)]*\)$`)
	rows := map[string][]string{} // item -> anchors its "Defined in" cell links to
	count := map[string]int{}
	for _, l := range strings.Split(inventory.text, "\n") {
		cells := strings.Split(l, "|")
		if !strings.HasPrefix(l, "| ") || len(cells) != 5 { // | Item | Kind | Defined in |
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
		switch count[want.item] {
		case 0:
			t.Errorf("contract inventory has no row for %q", want.item)
			continue
		case 1:
		default:
			t.Errorf("contract inventory has %d rows for %q, want one", count[want.item], want.item)
		}
		var defining *protocolSection
		for _, a := range rows[want.item] {
			s, ok := sections[a]
			if !ok {
				t.Errorf("row %q links to #%s, which is no heading of PROTOCOL.md", want.item, a)
				continue
			}
			if s.title == want.title && !s.example {
				defining = &s
			}
		}
		if defining == nil {
			t.Errorf("row %q links to %v, none of them the defining section %q", want.item, rows[want.item], want.title)
			continue
		}
		if !strings.Contains(defining.text, want.mention) {
			t.Errorf("row %q: section %q does not mention %q", want.item, want.title, want.mention)
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
