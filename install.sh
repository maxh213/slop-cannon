#!/bin/sh
# Install the `sc` (slop-cannon) binary from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/maxh213/slop-cannon/HEAD/install.sh | sh
#
# "HEAD" is the repository's default branch whatever it is named; a released
# copy of this script is at .../slop-cannon/<tag>/install.sh.
#
# Options (flags or environment variables):
#   --version <tag>   SC_VERSION       release tag to install, or "latest" (default)
#   --dir <path>      SC_INSTALL_DIR   where to put the binary (default: the first
#                                      writable of /usr/local/bin, ~/.local/bin)
#   --repo <o/n>      SC_REPO          GitHub repository (default: maxh213/slop-cannon)
#   --help
#
# Also honoured, mainly for testing: SC_BASE_URL (release download base, may be a
# file:// URL), SC_API_URL (latest-release API endpoint), SC_NO_VERIFY=1 (skip the
# checksum check — not recommended).
#
# The archive layout, names and checksums.txt come from .goreleaser.yaml; the two
# files are changed together.
set -eu

REPO="${SC_REPO:-maxh213/slop-cannon}"
VERSION="${SC_VERSION:-latest}"
INSTALL_DIR="${SC_INSTALL_DIR:-}"
NO_VERIFY="${SC_NO_VERIFY:-0}"
BINARY=sc
PROJECT=slop-cannon

usage() {
	cat <<'EOF'
install.sh - install the sc (slop-cannon) binary from a GitHub release

  curl -fsSL https://raw.githubusercontent.com/maxh213/slop-cannon/HEAD/install.sh | sh
  (or .../slop-cannon/<tag>/install.sh for the copy that shipped with a release)

Options (flag, or environment variable):
  --version <tag>   SC_VERSION       release tag to install, or "latest" (default)
  --dir <path>      SC_INSTALL_DIR   where to put the binary (default: the first
                                     writable of /usr/local/bin, ~/.local/bin)
  --repo <owner/name>  SC_REPO       GitHub repository (default: maxh213/slop-cannon)
  --help

Also honoured, mainly for testing: SC_BASE_URL, SC_API_URL, SC_NO_VERIFY=1.
EOF
}

log() { printf '%s\n' "$*" >&2; }
die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--version | -v)
		[ "$#" -ge 2 ] || die "--version needs a value"
		VERSION="$2"
		shift 2
		;;
	--version=*)
		VERSION="${1#--version=}"
		shift
		;;
	--dir | -d)
		[ "$#" -ge 2 ] || die "--dir needs a value"
		INSTALL_DIR="$2"
		shift 2
		;;
	--dir=*)
		INSTALL_DIR="${1#--dir=}"
		shift
		;;
	--repo)
		[ "$#" -ge 2 ] || die "--repo needs a value"
		REPO="$2"
		shift 2
		;;
	--repo=*)
		REPO="${1#--repo=}"
		shift
		;;
	--help | -h)
		usage
		exit 0
		;;
	*) die "unknown option: $1 (try --help)" ;;
	esac
done

BASE_URL="${SC_BASE_URL:-https://github.com/${REPO}/releases/download}"
API_URL="${SC_API_URL:-https://api.github.com/repos/${REPO}/releases/latest}"

have() { command -v "$1" >/dev/null 2>&1; }

# fetch <url> <dest>
if have curl; then
	fetch() { curl -fsSL --retry 3 -o "$2" -- "$1"; }
	fetch_stdout() { curl -fsSL --retry 3 -- "$1"; }
elif have wget; then
	fetch() { wget -qO "$2" -- "$1"; }
	fetch_stdout() { wget -qO- -- "$1"; }
else
	die "neither curl nor wget is installed"
fi

have tar || die "tar is not installed"

detect_os() {
	os="$(uname -s)"
	case "$os" in
	Linux) echo linux ;;
	Darwin) echo darwin ;;
	# There is no `go install` route: the module path is `slop-cannon`, not a
	# URL, so no module proxy can resolve it. Building from a checkout is the
	# only fallback.
	*) die "unsupported operating system: $os (slop-cannon ships linux and darwin builds; build from source instead: git clone https://github.com/${REPO}.git && cd ${REPO#*/} && make install)" ;;
	esac
}

detect_arch() {
	arch="$(uname -m)"
	case "$arch" in
	x86_64 | amd64) echo amd64 ;;
	arm64 | aarch64) echo arm64 ;;
	*) die "unsupported architecture: $arch (slop-cannon ships amd64 and arm64 builds)" ;;
	esac
}

resolve_version() {
	if [ "$VERSION" != latest ]; then
		printf '%s\n' "$VERSION"
		return
	fi
	tag="$(fetch_stdout "$API_URL" | tr ',' '\n' | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
	[ -n "$tag" ] || die "could not work out the latest release of $REPO from $API_URL (pass --version <tag>)"
	printf '%s\n' "$tag"
}

sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | cut -d' ' -f1
	elif have shasum; then
		shasum -a 256 "$1" | cut -d' ' -f1
	elif have openssl; then
		openssl dgst -sha256 "$1" | sed 's/.*= *//'
	else
		echo ""
	fi
}

pick_install_dir() {
	if [ -n "$INSTALL_DIR" ]; then
		printf '%s\n' "$INSTALL_DIR"
		return
	fi
	for candidate in /usr/local/bin "$HOME/.local/bin"; do
		if [ -d "$candidate" ] && [ -w "$candidate" ]; then
			printf '%s\n' "$candidate"
			return
		fi
	done
	printf '%s\n' "$HOME/.local/bin"
}

on_path() {
	case ":${PATH}:" in
	*":$1:"*) return 0 ;;
	*) return 1 ;;
	esac
}

OS="$(detect_os)"
ARCH="$(detect_arch)"
TAG="$(resolve_version)"
# goreleaser strips the leading "v" from the version in artefact names.
NUM="${TAG#v}"
ARCHIVE="${PROJECT}_${NUM}_${OS}_${ARCH}.tar.gz"

TMPDIR_SC="$(mktemp -d "${TMPDIR:-/tmp}/slop-cannon-install.XXXXXX")"
cleanup() { rm -rf "$TMPDIR_SC"; }
trap cleanup EXIT INT TERM

log "downloading $ARCHIVE ($TAG)"
fetch "${BASE_URL}/${TAG}/${ARCHIVE}" "${TMPDIR_SC}/${ARCHIVE}" ||
	die "download failed: ${BASE_URL}/${TAG}/${ARCHIVE}"

if [ "$NO_VERIFY" = 1 ]; then
	log "warning: SC_NO_VERIFY=1, skipping the checksum check"
else
	fetch "${BASE_URL}/${TAG}/checksums.txt" "${TMPDIR_SC}/checksums.txt" ||
		die "could not download checksums.txt for $TAG (set SC_NO_VERIFY=1 to install anyway)"
	want="$(sed -n "s/^\([0-9a-f]\{64\}\)[[:space:]][[:space:]]*\.\{0,2\}\/\{0,1\}${ARCHIVE}$/\1/p" "${TMPDIR_SC}/checksums.txt" | head -n 1)"
	[ -n "$want" ] || die "$ARCHIVE is not listed in checksums.txt for $TAG"
	got="$(sha256_of "${TMPDIR_SC}/${ARCHIVE}")"
	[ -n "$got" ] || die "no sha256 tool found (install coreutils, or set SC_NO_VERIFY=1)"
	[ "$want" = "$got" ] || die "checksum mismatch for $ARCHIVE: expected $want, got $got"
	log "checksum ok"
fi

tar -xzf "${TMPDIR_SC}/${ARCHIVE}" -C "$TMPDIR_SC" "$BINARY" ||
	die "$ARCHIVE does not contain a $BINARY binary"
chmod 0755 "${TMPDIR_SC}/${BINARY}"

DEST="$(pick_install_dir)"
mkdir -p "$DEST" || die "cannot create $DEST"
[ -w "$DEST" ] || die "$DEST is not writable; re-run with --dir <a writable directory>, or with sudo"

# Move into place through a temporary name in the same directory so an `sc`
# that is currently running is replaced atomically rather than truncated.
mv "${TMPDIR_SC}/${BINARY}" "${DEST}/.${BINARY}.new.$$" || die "cannot write to $DEST"
mv "${DEST}/.${BINARY}.new.$$" "${DEST}/${BINARY}" || die "cannot write to $DEST"

log "installed ${DEST}/${BINARY}"
"${DEST}/${BINARY}" version || die "the installed binary did not run"

if ! on_path "$DEST"; then
	log ""
	log "$DEST is not on your PATH. Add it, for example:"
	log "  export PATH=\"$DEST:\$PATH\""
fi

log ""
log "next: sc doctor   (checks git, bd and the cartridge CLIs)"
