package main

import (
	"regexp"
	"strconv"
)

// placeholderRE matches a placeholder, `{n}` or `{n:RES}`, or its escape, `{{n}}` or
// `{{n:RES}}` (PROTOCOL.md section 17). Group 1 is the escape's resource part, group 2 the
// placeholder's; a resource part is ":" and a resource name.
var placeholderRE = regexp.MustCompile(`\{\{n(:[a-z][a-z0-9-]{0,31})?\}\}|\{n(:[a-z][a-z0-9-]{0,31})?\}`)

// placeholderResource is the resource a placeholder or escape names: cpu without a resource
// part. escaped reports `{{...}}`.
func placeholderResource(m []string) (resource string, escaped bool) {
	part := m[2]
	if escaped = len(m[0]) > 1 && m[0][1] == '{'; escaped {
		part = m[1]
	}
	if part == "" {
		return "cpu", escaped
	}
	return part[1:], escaped
}

// placeholderMatches returns, for each argument, the placeholders substitute would replace in
// it: the whole argument when it is exactly one placeholder or escape, and with inside (--subst)
// every occurrence within it too.
func placeholderMatches(arg string, inside bool) [][]int {
	if inside {
		return placeholderRE.FindAllStringSubmatchIndex(arg, -1)
	}
	m := placeholderRE.FindStringSubmatchIndex(arg)
	if m == nil || m[0] != 0 || m[1] != len(arg) {
		return nil
	}
	return [][]int{m}
}

func submatches(arg string, idx []int) []string {
	out := make([]string, len(idx)/2)
	for i := range out {
		if idx[2*i] >= 0 {
			out[i] = arg[idx[2*i]:idx[2*i+1]]
		}
	}
	return out
}

// placeholderResources lists the resources the placeholders of argv name (escapes name none),
// so that a placeholder naming a resource the request does not name is a usage error detected
// before the request queues.
func placeholderResources(argv []string, inside bool) []string {
	var out []string
	for _, arg := range argv {
		for _, idx := range placeholderMatches(arg, inside) {
			if res, escaped := placeholderResource(submatches(arg, idx)); !escaped {
				out = append(out, res)
			}
		}
	}
	return out
}

// substitute replaces the placeholders of argv with the grant (PROTOCOL.md section 17): an
// argument that is exactly `{n}` becomes the grant of cpu and `{n:RES}` the grant of RES;
// `{{n}}` and `{{n:RES}}` become the literal `{n}` and `{n:RES}`. With inside (--subst),
// placeholders and escapes within arguments are replaced too (`-j{n}`). A placeholder naming a
// resource missing from grant is left as it is; the caller has refused it before queueing.
func substitute(argv []string, grant map[string]int, inside bool) []string {
	out := make([]string, len(argv))
	for i, arg := range argv {
		matches := placeholderMatches(arg, inside)
		if matches == nil {
			out[i] = arg
			continue
		}
		var b []byte
		last := 0
		for _, idx := range matches {
			b = append(b, arg[last:idx[0]]...)
			m := submatches(arg, idx)
			res, escaped := placeholderResource(m)
			n, ok := grant[res]
			switch {
			case escaped:
				b = append(b, m[0][1:len(m[0])-1]...)
			case ok:
				b = strconv.AppendInt(b, int64(n), 10)
			default:
				b = append(b, m[0]...)
			}
			last = idx[1]
		}
		out[i] = string(append(b, arg[last:]...))
	}
	return out
}
