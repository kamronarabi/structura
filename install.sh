#!/bin/sh
#
# Install Structura on macOS or Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/kamronarabi/structura/main/install.sh | sh
#
# The release archives are the same ones GitHub serves to anyone clicking the
# releases page, and every download is checked against the published
# checksums before anything is installed. Nothing is written outside the
# install directory, and no step needs root unless you point it somewhere
# that does.
#
# Environment:
#   STRUCTURA_VERSION       version to install, e.g. v0.1.0 (default: latest)
#   STRUCTURA_INSTALL_DIR   where to put the binary (default: see below)
#   STRUCTURA_BASE_URL      where to fetch from; used by the installer's own
#                           test, which serves a locally built release so that
#                           this script is exercised rather than trusted
#
# POSIX sh on purpose: this runs before the user has anything installed, and
# /bin/sh is not bash on Debian, Alpine, or a minimal container.

set -eu

REPO="kamronarabi/structura"
BIN="structura"

VERSION="${STRUCTURA_VERSION:-latest}"
INSTALL_DIR="${STRUCTURA_INSTALL_DIR:-}"
BASE_URL="${STRUCTURA_BASE_URL:-}"

say()  { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
	command -v "$1" >/dev/null 2>&1 || die "this installer needs $1, which is not on PATH"
}

# ---------------------------------------------------------------- platform

detect_platform() {
	os=$(uname -s)
	case "$os" in
		Darwin) os=darwin ;;
		Linux)  os=linux ;;
		MINGW*|MSYS*|CYGWIN*)
			die "Windows is not supported by this script. Download the .zip from
       https://github.com/$REPO/releases and put structura.exe on your PATH." ;;
		*) die "unsupported operating system: $os" ;;
	esac

	arch=$(uname -m)
	case "$arch" in
		x86_64|amd64)  arch=amd64 ;;
		arm64|aarch64) arch=arm64 ;;
		armv7l|armv7)  arch=armv7 ;;
		*) die "unsupported architecture: $arch
       Prebuilt binaries exist for x86_64, arm64, and armv7. For anything
       else, build from source: go install github.com/$REPO/cmd/structura@latest" ;;
	esac

	# Linux armv7 is the only 32-bit target, and it is not built for macOS.
	if [ "$os" = darwin ] && [ "$arch" = armv7 ]; then
		die "there is no armv7 build for macOS"
	fi

	PLATFORM="${os}_${arch}"
}

# ---------------------------------------------------------------- download

# fetch URL DEST
fetch() {
	# curl's own diagnostic is suppressed because the caller's message names
	# the URL and says what to do about it, and two errors for one failure
	# reads as two problems.
	if [ -n "${HAVE_CURL:-}" ]; then
		curl -fsSL "$1" -o "$2" 2>/dev/null
	else
		wget -qO "$2" "$1" 2>/dev/null
	fi
}

# resolve_version prints the tag to install.
#
# "latest" follows the redirect GitHub serves from /releases/latest rather
# than calling the API, which is rate limited to 60 requests an hour per
# address and would fail for everyone behind a busy NAT.
resolve_version() {
	if [ "$VERSION" != latest ]; then
		printf '%s\n' "$VERSION"
		return
	fi
	if [ -n "$BASE_URL" ]; then
		die "STRUCTURA_VERSION must be set explicitly when STRUCTURA_BASE_URL is used"
	fi

	if [ -n "${HAVE_CURL:-}" ]; then
		url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
			"https://github.com/$REPO/releases/latest") || url=""
	else
		url=$(wget -qS --max-redirect=10 --spider \
			"https://github.com/$REPO/releases/latest" 2>&1 |
			awk '/^[ \t]*Location:/ { print $2 }' | tail -1) || url=""
	fi

	tag=$(printf '%s\n' "$url" | sed -n 's|.*/releases/tag/\(.*\)$|\1|p')
	[ -n "$tag" ] || die "could not determine the latest version.
       Set one explicitly:  STRUCTURA_VERSION=v0.1.0 sh install.sh"
	printf '%s\n' "$tag"
}

# verify_checksum ARCHIVE_PATH CHECKSUMS_PATH
#
# The names here are prefixed because sh has no local variables: a plain
# "archive=" inside a function overwrites the caller's, which is exactly the
# bug that made the first version of this script try to untar a path with the
# temporary directory in it twice.
verify_checksum() {
	_vc_file="$1"
	_vc_sums="$2"
	_vc_name=$(basename "$_vc_file")

	# The archive name is what ties a line in checksums.txt to this download.
	_vc_want=$(awk -v f="$_vc_name" '$2 == f { print $1 }' "$_vc_sums")
	[ -n "$_vc_want" ] || die "$_vc_name is not listed in checksums.txt"

	if command -v sha256sum >/dev/null 2>&1; then
		_vc_got=$(sha256sum "$_vc_file" | awk '{print $1}')
	elif command -v shasum >/dev/null 2>&1; then
		_vc_got=$(shasum -a 256 "$_vc_file" | awk '{print $1}')
	else
		die "this installer needs sha256sum or shasum to verify the download"
	fi

	[ "$_vc_got" = "$_vc_want" ] || die "checksum mismatch for $_vc_name
       expected $_vc_want
       got      $_vc_got
       The download is not what was published. Nothing has been installed."
}

# ---------------------------------------------------------------- install

# choose_install_dir prints where the binary should go.
#
# It never runs sudo on its own. A script that escalates without being asked
# is a script nobody should pipe into a shell, and the fallback below needs no
# privileges at all.
choose_install_dir() {
	if [ -n "$INSTALL_DIR" ]; then
		printf '%s\n' "$INSTALL_DIR"
		return
	fi
	if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		printf '%s\n' /usr/local/bin
		return
	fi
	printf '%s\n' "$HOME/.local/bin"
}

on_path() {
	case ":${PATH}:" in
		*":$1:"*) return 0 ;;
		*) return 1 ;;
	esac
}

main() {
	if command -v curl >/dev/null 2>&1; then
		HAVE_CURL=1
	elif command -v wget >/dev/null 2>&1; then
		HAVE_CURL=""
	else
		die "this installer needs curl or wget"
	fi
	need tar
	need uname

	detect_platform
	tag=$(resolve_version)
	# Archive names carry the version without its leading "v".
	version=${tag#v}

	base="${BASE_URL:-https://github.com/$REPO/releases/download/$tag}"
	archive="${BIN}_${version}_${PLATFORM}.tar.gz"

	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t structura)
	# Nothing is left behind, including on a failed or interrupted download.
	trap 'rm -rf "$tmp"' EXIT INT TERM

	say "Installing $BIN $tag ($PLATFORM)"

	fetch "$base/$archive" "$tmp/$archive" ||
		die "could not download $base/$archive
       If that version exists, check https://github.com/$REPO/releases"
	fetch "$base/checksums.txt" "$tmp/checksums.txt" ||
		die "could not download checksums.txt; refusing to install an unverified binary"

	verify_checksum "$tmp/$archive" "$tmp/checksums.txt"

	tar -xzf "$tmp/$archive" -C "$tmp"
	[ -f "$tmp/$BIN" ] || die "the archive did not contain a $BIN binary"
	chmod +x "$tmp/$BIN"

	# Run it before installing it. A binary for the wrong architecture fails
	# here, where the message can say so, rather than the first time the user
	# tries to use it.
	"$tmp/$BIN" version >/dev/null 2>&1 ||
		die "the downloaded binary does not run on this machine ($PLATFORM)"

	dir=$(choose_install_dir)
	mkdir -p "$dir" 2>/dev/null || die "cannot create $dir"
	if [ ! -w "$dir" ]; then
		die "$dir is not writable.
       Either pick somewhere else:  STRUCTURA_INSTALL_DIR=\$HOME/.local/bin sh install.sh
       or re-run this with the privileges to write there."
	fi

	# Move into place rather than copying over a running binary: replacing an
	# open executable in place fails on some systems, and rename is atomic on
	# the same filesystem.
	mv -f "$tmp/$BIN" "$dir/$BIN"

	say "Installed $dir/$BIN"
	say ""
	"$dir/$BIN" version || true

	if ! on_path "$dir"; then
		say ""
		warn "$dir is not on your PATH. Add it:"
		warn ""
		warn "    export PATH=\"$dir:\$PATH\""
		warn ""
		warn "Put that in ~/.zshrc or ~/.bashrc to make it stick."
	fi

	say ""
	say "Next:"
	say "    cd your-repo"
	say "    $BIN scan --print"
	say "    $BIN install-mcp --client claude-code"
}

main "$@"
