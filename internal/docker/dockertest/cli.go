//go:build unix

package dockertest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// cliScript is the stand-in docker: it prints the answer file of the one question it knows and
// refuses every other argument list.
const cliScript = `#!/bin/sh
d=$(dirname "$0")
if [ $# -eq 4 ] && [ "$1" = context ] && [ "$2" = ls ] && [ "$3" = --format ] && [ "$4" = '{{json .}}' ]; then
	exec /bin/cat "$d/context-ls.out"
fi
if [ $# -eq 2 ] && [ "$1" = buildx ] && [ "$2" = inspect ]; then
	exec /bin/cat "$d/buildx-inspect.out"
fi
echo "docker stand-in: unexpected arguments: $*" >&2
exit 1
`

// CLI writes a stand-in docker into a directory of its own and returns that directory, for a test to
// put first on a command's PATH. It answers `docker context ls --format '{{json .}}'` with one JSON
// object per line, keyed as the CLI prints them, one context per endpoint as its DockerEndpoint: the
// first named default, the last the current one. It answers `docker buildx inspect` with the current
// context's builder, whose Driver: line is driver. Any other arguments exit 1.
func CLI(t testing.TB, endpoints []string, driver string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	var ctxs strings.Builder
	name := "default"
	for i, ep := range endpoints {
		c := struct {
			Current                                  bool
			Description, DockerEndpoint, Error, Name string
		}{Current: i == len(endpoints)-1, DockerEndpoint: ep, Name: "default"}
		if i == 0 {
			c.Description = "Current DOCKER_HOST based configuration"
		} else {
			c.Name = "context-" + strconv.Itoa(i)
		}
		if c.Current {
			name = c.Name
		}
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		ctxs.Write(append(b, '\n'))
	}
	builder := "Name:   " + name + "\nDriver: " + driver + "\n\nNodes:\nName:             " + name + "\nEndpoint:         " + name + "\nStatus:           running\n"
	for file, content := range map[string]string{"context-ls.out": ctxs.String(), "buildx-inspect.out": builder} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(cliScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
