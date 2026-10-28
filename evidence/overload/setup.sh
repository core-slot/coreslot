#!/bin/sh
# Builds the overload stand from public sources at pinned commits: run 3's trees (VS Code,
# FastAPI, SQLAlchemy; run 7's heavy and wide agents still use them), run 8's stand-ins
# (overload/stand8.json) and coreslot's pinned release binary.
#
#   evidence/overload/setup.sh [ROOT]     (default ROOT: ~/bench.noindex)
#
# Run 8's part: under ROOT/run8/A1..A4, one checkout per agent of each class stand-in suite, each
# with its own environment, warmed so that a first measured run compiles nothing; zstd built from
# its release tag into ROOT/bin; and the long batch's corpus, a Linux release tarball verified by
# SHA-256 and gunzipped once to ROOT/run8/linux-6.12.tar (about 1.5 GB).
#
# The `.noindex` suffix keeps Spotlight from indexing the trees, and the root is excluded from
# Time Machine where tmutil allows it. Needs git, node with npm and corepack, uv, go, make, a C compiler, curl
# and gunzip. Network is used here only; the measured runs use none. On a second run, run 8's
# part fetches and builds nothing.
set -eu
ROOT=${1:-"$HOME/bench.noindex"}
VSCODE_TAG=1.142.0
VSCODE_SHA=d956e8a6ac5d904486557eea07e1d6bd62a4d36f
FASTAPI_TAG=0.156.2
FASTAPI_SHA=44cc7420b798b70b406516f5f0abf039f99edbe3
SQLALCHEMY_TAG=rel_2_1_5
SQLALCHEMY_SHA=4fbf72c07f5bebcabb02e35ee8b742a217ac649d
RAFT_TAG=v1.7.3
RAFT_SHA=c0dc6a0b2c7e889f31e5ab2f7ed90ceb159acffe
SEMVER_TAG=3.0.4
SEMVER_SHA=6adf8765f6e21910f1f0c13151ce84f32f8d431d
DJANGO_TAG=6.0.9
DJANGO_SHA=ea10f2ee50995ae9202dd18aa1a9206a8dab7b1a
VUE_TAG=v3.5.44
VUE_SHA=bf5f0dd877dd3c23ce85c2cbc1dd771567e84c8c
PYRIGHT=1.1.414
PYDANTIC_TAG=v2.13.5
PYDANTIC_SHA=001dea020e0809844e5b17666432c9135a976f46
PANDAS_TAG=v3.0.6
PANDAS_SHA=2905718b126579d2cf0fe5bae05bd75facb67d77
ZSTD_TAG=v1.5.7
ZSTD_SHA=f8745da6ff1ad1e7bab384bd1f9d742439278e99
LINUX=linux-6.12
LINUX_TGZ_SHA256=1376ce98485a0c8de4635d0bfb88760924e4a818c0439d830738bb1c690b7ca4
LINUX_TAR_SHA256=d1da13cc60d2fb52b8d7cc436fd97990d308ade6e9b6a0ee4712d6b2ee07816d
CORESLOT_TAG=v0.3.0
CORESLOT_SHA256=e3806d2d6115a4eb0a85ef175d24724edea2d2df8479eae0d23f5f4139db79ba

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

# Run 8's stand-ins, one checkout per agent: the trace mix runs one class in several agents at
# once, and the suites write into their trees (bytecode, mutmut's mutants/, vitest's cache). Each
# class runs a repository of its own. Collecting SQLAlchemy's tests once writes the bytecode a
# first run would write.
for agent in A1 A2 A3 A4; do
	d=$ROOT/run8/$agent
	fetch https://github.com/sqlalchemy/sqlalchemy.git "$SQLALCHEMY_TAG" "$SQLALCHEMY_SHA" "$d/sqlalchemy"
	(cd "$d/sqlalchemy" && uv venv --quiet --allow-existing --python 3.13 .venv &&
		uv pip install --quiet --compile-bytecode --python .venv/bin/python -e . pytest==9.1.1 pytest-xdist==3.8.0 &&
		.venv/bin/python -m pytest -q -p no:cacheprovider --collect-only test >/dev/null)
	# go_test: hashicorp/raft's snapshot-restore and transport tests under the race detector,
	# which mostly wait. The cluster election tests are left out: they fail under load on timing.
	# Building the race test binary once fills the build cache.
	fetch https://github.com/hashicorp/raft.git "$RAFT_TAG" "$RAFT_SHA" "$d/raft"
	(cd "$d/raft" && go test -race -count=1 -run '^$' . >/dev/null)
	# mutation: mutmut over python-semver. The library itself is not installed: mutmut puts its
	# mutants/ copy first on the path. Its [mutmut] section, rewritten on every run, makes the
	# mutants/ copy carry .pytest.ini (whose pythonpath would otherwise point the tests at the
	# unmutated src/), replaces the addopts that need pytest-cov, deselects the one test that cannot
	# run twice in a process (its parameters are spent context managers), and passes tests/ and
	# docs/ to every mutant's run: each mutant runs the whole suite and the doctests until a test
	# kills it, not only the tests mutmut's stats tie to the mutated function.
	fetch https://github.com/python-semver/python-semver.git "$SEMVER_TAG" "$SEMVER_SHA" "$d/semver"
	(cd "$d/semver" && uv venv --quiet --allow-existing --python 3.13 .venv &&
		uv pip install --quiet --compile-bytecode --python .venv/bin/python mutmut==3.8.0 pytest==9.1.1 packaging==26.3 &&
		git checkout --quiet -- setup.cfg && cat >>setup.cfg <<'EOF'

[mutmut]
also_copy =
    .pytest.ini
    docs
    README.rst
pytest_add_cli_args =
    -o
    addopts=--import-mode=importlib --doctest-glob=*.rst --doctest-modules
    --deselect
    tests/test_pysemver-cli.py::test_should_process_parsed_cli_arguments
    tests
    docs
EOF
	)
	# py_db: Django's own suite on SQLite in one process. Its two dependencies are pinned here.
	# Compiling the tests writes their bytecode; one test module is a syntax error on purpose.
	fetch https://github.com/django/django.git "$DJANGO_TAG" "$DJANGO_SHA" "$d/django"
	(cd "$d/django" && uv venv --quiet --allow-existing --python 3.13 .venv &&
		uv pip install --quiet --compile-bytecode --python .venv/bin/python -e . asgiref==3.13.0 sqlparse==0.6.0 &&
		{ .venv/bin/python -m compileall -q -x tests_syntax_error tests >/dev/null; })
	# other: Vue's unit tests under vitest, on the pnpm its package.json names, from its lockfile;
	# no install scripts (the git hooks), and no browser: the unit projects run in node.
	fetch https://github.com/vuejs/core.git "$VUE_TAG" "$VUE_SHA" "$d/vue-core"
	(cd "$d/vue-core" && COREPACK_ENABLE_DOWNLOAD_PROMPT=0 corepack pnpm install --frozen-lockfile --ignore-scripts --silent)
done

# typecheck: pyright, pinned, over FastAPI's tree and its environment (above); it only reads the
# tree, so the agents share it.
npm install --silent --prefix "$ROOT/run8/pyright" --no-audit --no-fund "pyright@$PYRIGHT"
"$ROOT/run8/pyright/node_modules/.bin/pyright" --version
# Larger trees for pyright, read-only too: pydantic at the release FastAPI's environment carries,
# checked in that environment; pandas, in an environment of its two runtime dependencies.
fetch https://github.com/pydantic/pydantic.git "$PYDANTIC_TAG" "$PYDANTIC_SHA" "$ROOT/run8/pydantic"
fetch https://github.com/pandas-dev/pandas.git "$PANDAS_TAG" "$PANDAS_SHA" "$ROOT/run8/pandas"
(cd "$ROOT/run8/pandas" && uv venv --quiet --allow-existing --python 3.13 .venv &&
	uv pip install --quiet --python .venv/bin/python numpy==2.5.4 python-dateutil==3.0.0)
# typecheck's pick checks pandas with pandas' own pyright settings (its pyproject.toml), over the
# library and two of its test directories, series and arithmetic: the library alone is lighter
# than the class's band, the whole test tree several times heavier.
(cd "$ROOT/run8/pandas" && {
	printf '{"extends": "pyproject.toml", "include": ["pandas", "typings"], "exclude": ["pandas/io/clipboard", "pandas/util/version", "pandas/core/_numba/extensions.py"'
	for t in pandas/tests/*; do
		case $t in */__init__.py | */series | */arithmetic) ;; *) printf ', "%s"' "$t" ;; esac
	done
	printf ']}\n'
} >pyrightconfig.run8.json)

# The long batch: zstd from its release tag, so the measured binary is fixed, over a Linux
# release tarball.
fetch https://github.com/facebook/zstd.git "$ZSTD_TAG" "$ZSTD_SHA" "$ROOT/run8/zstd"
if [ ! -x "$ROOT/bin/zstd" ]; then
	make -s -C "$ROOT/run8/zstd/programs" zstd
	mkdir -p "$ROOT/bin" && cp "$ROOT/run8/zstd/programs/zstd" "$ROOT/bin/zstd"
fi
"$ROOT/bin/zstd" -V
TAR=$ROOT/run8/$LINUX.tar
if [ ! -f "$TAR" ] || [ "$(shasum -a 256 "$TAR" | cut -d' ' -f1)" != "$LINUX_TAR_SHA256" ]; then
	curl -sSfL -o "$TAR.gz" "https://cdn.kernel.org/pub/linux/kernel/v6.x/$LINUX.tar.gz"
	test "$(shasum -a 256 "$TAR.gz" | cut -d' ' -f1)" = "$LINUX_TGZ_SHA256"
	gunzip -f "$TAR.gz"
	test "$(shasum -a 256 "$TAR" | cut -d' ' -f1)" = "$LINUX_TAR_SHA256"
fi

# coreslot itself: the release binary the stand pins (stand8.json's coreslot_sha256), from its
# release archive, checked against the release's checksums and the pin.
BIN=$ROOT/bin/coreslot
if [ ! -f "$BIN" ] || [ "$(shasum -a 256 "$BIN" | cut -d' ' -f1)" != "$CORESLOT_SHA256" ]; then
	A=coreslot_${CORESLOT_TAG#v}_darwin_arm64.tar.gz
	T=$(mktemp -d)
	curl -sSfL -o "$T/$A" "https://github.com/core-slot/coreslot/releases/download/$CORESLOT_TAG/$A"
	curl -sSfL -o "$T/checksums.txt" "https://github.com/core-slot/coreslot/releases/download/$CORESLOT_TAG/checksums.txt"
	(cd "$T" && grep " $A\$" checksums.txt | shasum -a 256 -c - >/dev/null && tar -xzf "$A" coreslot)
	test "$(shasum -a 256 "$T/coreslot" | cut -d' ' -f1)" = "$CORESLOT_SHA256"
	mkdir -p "$ROOT/bin" && mv "$T/coreslot" "$BIN" && rm -rf "$T"
fi
"$BIN" version
