#!/bin/sh
# Builds the stand of the acceptance run from public sources at pinned commits.
#
#   evidence/air-v04/setup.sh STAND
#
# Under STAND: VS Code (its client type check, on the TypeScript its lockfile installs, with the
# Electron types its .npmrc targets), FastAPI (its lint and tests, in the environment its uv.lock
# pins) and SQLAlchemy (its ORM tests on SQLite; it has no lockfile, so the test tools are pinned
# here). A name ending in `.noindex` keeps Spotlight out of the trees. Needs git, node with npm and
# uv; the network is used here only, the measured runs use none. run.py checks the same pins.
set -eu
[ $# -eq 1 ] || { sed -n '4p' "$0" | sed 's/^# *//' >&2; exit 64; }
STAND=$1
VSCODE_TAG=1.142.0
VSCODE_SHA=d956e8a6ac5d904486557eea07e1d6bd62a4d36f
FASTAPI_TAG=0.156.2
FASTAPI_SHA=44cc7420b798b70b406516f5f0abf039f99edbe3
SQLALCHEMY_TAG=rel_2_1_5
SQLALCHEMY_SHA=4fbf72c07f5bebcabb02e35ee8b742a217ac649d

mkdir -p "$STAND"
tmutil addexclusion "$STAND" 2>/dev/null || true

# Git LFS objects are left as pointers: no gate reads them.
NOLFS="-c filter.lfs.process= -c filter.lfs.smudge=cat -c filter.lfs.required=false"
fetch() { # fetch URL TAG SHA DIR
	if [ ! -d "$4/.git" ]; then
		git $NOLFS -c advice.detachedHead=false clone --quiet --depth 1 --branch "$2" "$1" "$4"
	fi
	git $NOLFS -C "$4" -c advice.detachedHead=false checkout --quiet "$2"
	test "$(git -C "$4" rev-parse HEAD)" = "$3"
}

fetch https://github.com/microsoft/vscode.git "$VSCODE_TAG" "$VSCODE_SHA" "$STAND/vscode"
(cd "$STAND/vscode" && npm ci --ignore-scripts --no-audit --no-fund --loglevel=error)
ELECTRON=$(sed -n 's/^target="\(.*\)"$/\1/p' "$STAND/vscode/.npmrc")
npm install --silent --prefix "$STAND/electron-types" --ignore-scripts --no-audit --no-fund "electron@$ELECTRON"
ln -sfn "$STAND/electron-types/node_modules/electron" "$STAND/vscode/node_modules/electron"

fetch https://github.com/fastapi/fastapi.git "$FASTAPI_TAG" "$FASTAPI_SHA" "$STAND/fastapi"
(cd "$STAND/fastapi" && uv sync --quiet --frozen --python 3.13 --group tests --all-extras)

fetch https://github.com/sqlalchemy/sqlalchemy.git "$SQLALCHEMY_TAG" "$SQLALCHEMY_SHA" "$STAND/sqlalchemy"
(cd "$STAND/sqlalchemy" && uv venv --quiet --allow-existing --python 3.13 .venv &&
	uv pip install --quiet --python .venv/bin/python -e . pytest==9.1.1 pytest-xdist==3.8.0)
echo "stand ready: $STAND"
