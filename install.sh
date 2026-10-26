#!/bin/sh
# git-tab installer: puts git-tab on your PATH and its hooks in every repository.
#
#   curl -fsSL https://raw.githubusercontent.com/fish2lab/git-tab/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/fish2lab/git-tab/main/install.sh | sh -s -- --uninstall
#
# GIT_TAB_BIN_DIR picks where the binary goes (default ~/.local/bin).
set -eu

repo=fish2lab/git-tab
bin_dir=${GIT_TAB_BIN_DIR:-$HOME/.local/bin}

say() { printf '%s\n' "$*"; }
die() {
	printf 'git-tab: %s\n' "$*" >&2
	exit 1
}

uninstall() {
	exe=$(command -v git-tab 2>/dev/null || true)
	[ -n "$exe" ] || exe=$bin_dir/git-tab
	if [ -x "$exe" ]; then
		"$exe" uninstall --purge
		rm -f "$exe"
		say "✓ removed $exe"
	else
		say "git-tab is not installed"
	fi
	say "  commits keep their receipts: they are plain text in the history"
}

case "${1:-}" in
"") ;;
--uninstall)
	uninstall
	exit 0
	;;
*) die "usage: install.sh [--uninstall]" ;;
esac

command -v git >/dev/null 2>&1 || die "git is not installed"
mkdir -p "$bin_dir"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Run from a checkout: build it. Piped from curl: a release binary, else go install.
# Piped into sh, $0 is the shell's name and says nothing about the current directory.
here=
case $0 in
*install.sh) here=$(cd "$(dirname "$0")" 2>/dev/null && pwd || true) ;;
esac
if [ -n "$here" ] && grep -qs "^module github.com/$repo\$" "$here/go.mod"; then
	command -v go >/dev/null 2>&1 || die "building from source needs Go: https://go.dev/dl"
	say "building from $here"
	(cd "$here" && go build -o "$tmp/git-tab" .)
	install -m 755 "$tmp/git-tab" "$bin_dir/git-tab"
else
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) arch=$(uname -m) ;;
	esac
	url=https://github.com/$repo/releases/latest/download/git-tab_${os}_${arch}.tar.gz
	if curl -fsSL "$url" -o "$tmp/git-tab.tar.gz" 2>/dev/null && tar -xzf "$tmp/git-tab.tar.gz" -C "$tmp" git-tab; then
		install -m 755 "$tmp/git-tab" "$bin_dir/git-tab"
	elif command -v go >/dev/null 2>&1; then
		say "no release for ${os}_${arch}; building with go install"
		GOBIN=$bin_dir go install "github.com/$repo@latest"
	else
		die "no release for ${os}_${arch} and no Go to build one: https://go.dev/dl"
	fi
fi
say "✓ $bin_dir/git-tab ($("$bin_dir/git-tab" version))"

if ! "$bin_dir/git-tab" install --global; then
	say "  this git is older than 2.54, so hooks go in one repository at a time:"
	say "  run \`git tab install\` inside each repository"
fi

case ":$PATH:" in
*":$bin_dir:"*) ;;
*) say "  add $bin_dir to your PATH for \`git tab summary\` and friends" ;;
esac
