#!/usr/bin/env bash
# Installs the GoReleaser version in .github/goreleaser-version into $RUNNER_TEMP and puts it on
# the job's PATH, only after `gh attestation verify` shows that GoReleaser's own release workflow,
# at that version's tag, built the very archive downloaded. A swapped release asset or download
# carries no such Sigstore certificate, and the job stops before anything runs it.
#
# Usage (in a GitHub Actions step, with GH_TOKEN set): scripts/install-goreleaser.sh
set -euo pipefail
cd "$(dirname "$0")/.."

v="$(cat .github/goreleaser-version)"
[[ "$v" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "::error::.github/goreleaser-version: '$v'"; exit 1; }

case "$(uname -s)/$(uname -m)" in
	Linux/x86_64) asset=goreleaser_Linux_x86_64.tar.gz ;;
	Linux/aarch64) asset=goreleaser_Linux_arm64.tar.gz ;;
	Darwin/*) asset=goreleaser_Darwin_all.tar.gz ;;
	*) echo "::error::no GoReleaser archive for $(uname -s)/$(uname -m)"; exit 1 ;;
esac

dir="${RUNNER_TEMP:?}/goreleaser"
mkdir -p "$dir"
gh release download "$v" --repo goreleaser/goreleaser --pattern "$asset" --dir "$dir" --clobber
gh attestation verify "$dir/$asset" --repo goreleaser/goreleaser \
	--cert-identity "https://github.com/goreleaser/goreleaser/.github/workflows/release.yml@refs/tags/$v" \
	--cert-oidc-issuer https://token.actions.githubusercontent.com
echo "$asset $v: attestation verified"
tar -xzf "$dir/$asset" -C "$dir" goreleaser
echo "$dir" >>"${GITHUB_PATH:?}"
