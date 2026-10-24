#!/bin/sh
# Builds the stand of acceptance run 3 from public repositories at pinned commits.
#
#   evidence/overload/setup.sh [ROOT]     (default ROOT: ~/bench.noindex)
#
# The `.noindex` suffix keeps Spotlight from indexing the trees, and the root is excluded from
# Time Machine where tmutil allows it. Needs git, node with npm, uv and go. Network is used here
# only; the measured runs use none.
set -eu
ROOT=${1:-"$HOME/bench.noindex"}
VSCODE_TAG=1.142.0
VSCODE_SHA=d956e8a6ac5d904486557eea07e1d6bd62a4d36f
FASTAPI_TAG=0.156.2
FASTAPI_SHA=44cc7420b798b70b406516f5f0abf039f99edbe3
SQLALCHEMY_TAG=rel_2_1_5
SQLALCHEMY_SHA=4fbf72c07f5bebcabb02e35ee8b742a217ac649d

mkdir -p "$ROOT"
tmutil addexclusion "$ROOT" 2>/dev/null || true

# Git LFS objects (binary assets) are left as pointers: no gate reads them, and git-lfs need not
# be installed.
NOLFS="-c filter.lfs.process= -c filter.lfs.smudge=cat -c filter.lfs.required=false"
fetch() { # fetch URL TAG SHA DIR
	if [ ! -d "$4/.git" ]; then
		git $NOLFS -c advice.detachedHead=false clone --quiet --depth 1 --branch "$2" "$1" "$4"
	fi
	git $NOLFS -C "$4" -c advice.detachedHead=false checkout --quiet "$2"
	test "$(git -C "$4" rev-parse HEAD)" = "$3"
}

# VS Code: the client's type check, as its own package.json names it (typecheck-client, on the
# TypeScript 7 its lockfile installs).
fetch https://github.com/microsoft/vscode.git "$VSCODE_TAG" "$VSCODE_SHA" "$ROOT/vscode"
(cd "$ROOT/vscode" && npm ci --ignore-scripts --no-audit --no-fund --loglevel=error)
# The client imports Electron, whose types ship in the `electron` package at the version VS Code's
# .npmrc targets; installed without its scripts (no Electron download) and linked in.
ELECTRON=$(sed -n 's/^target="\(.*\)"$/\1/p' "$ROOT/vscode/.npmrc")
npm install --silent --prefix "$ROOT/electron-types" --ignore-scripts --no-audit --no-fund "electron@$ELECTRON"
ln -sfn "$ROOT/electron-types/node_modules/electron" "$ROOT/vscode/node_modules/electron"

# FastAPI: its lint and its test suite, in the environment its uv.lock pins (the tests group and
# every extra, which the CLI tests need).
fetch https://github.com/fastapi/fastapi.git "$FASTAPI_TAG" "$FASTAPI_SHA" "$ROOT/fastapi"
(cd "$ROOT/fastapi" && uv sync --quiet --frozen --python 3.13 --group tests --all-extras)

# SQLAlchemy: its ORM tests on the default SQLite. It has no lockfile, so the test tools are
# pinned here.
fetch https://github.com/sqlalchemy/sqlalchemy.git "$SQLALCHEMY_TAG" "$SQLALCHEMY_SHA" "$ROOT/sqlalchemy"
(cd "$ROOT/sqlalchemy" && uv venv --quiet --allow-existing --python 3.13 .venv &&
	uv pip install --quiet --python .venv/bin/python -e . pytest==9.1.1 pytest-xdist==3.8.0)
# Run 7's wide mix runs two more SQLAlchemy suites at once with the ORM tests; each gets its own
# checkout at the same commit, so that no two gates running at once write into one working tree
# (pytest writes bytecode caches there).
for dir in sqlalchemy-sql sqlalchemy-engine; do
	fetch https://github.com/sqlalchemy/sqlalchemy.git "$SQLALCHEMY_TAG" "$SQLALCHEMY_SHA" "$ROOT/$dir"
	(cd "$ROOT/$dir" && uv venv --quiet --allow-existing --python 3.13 .venv &&
		uv pip install --quiet --python .venv/bin/python -e . pytest==9.1.1 pytest-xdist==3.8.0)
done

# coreslot itself, built from the checkout this script is in.
mkdir -p "$ROOT/bin"
(cd "$(dirname "$0")/../.." && go build -o "$ROOT/bin/coreslot" ./cmd/coreslot)
"$ROOT/bin/coreslot" version
