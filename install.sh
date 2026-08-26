#!/bin/sh
set -e

REPO="OWNER/lazyrecall"

# --- platform detection -------------------------------------------------
case "$(uname -s)" in
Darwin) os="darwin" ;;
Linux) os="linux" ;;
*)
	echo "lazyrecall: unsupported OS: $(uname -s) (expected darwin or linux)" >&2
	exit 1
	;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch="amd64" ;;
aarch64 | arm64) arch="arm64" ;;
*)
	echo "lazyrecall: unsupported architecture: $(uname -m) (expected x86_64, aarch64, or arm64)" >&2
	exit 1
	;;
esac

# --- resolve version ----------------------------------------------------
if [ -n "${LAZYRECALL_VERSION:-}" ]; then
	version="$LAZYRECALL_VERSION"
else
	version="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
	if [ -z "$version" ]; then
		echo "lazyrecall: could not resolve the latest release of $REPO" >&2
		exit 1
	fi
fi

version="${version#v}"
archive="lazyrecall_${version}_${os}_${arch}.tar.gz"
base_url="https://github.com/$REPO/releases/download/v${version}"

# --- download -----------------------------------------------------------
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM

echo "Downloading lazyrecall ${version} (${os}/${arch})..."
curl -fsSL -o "$tmpdir/$archive" "$base_url/$archive"
curl -fsSL -o "$tmpdir/checksums.txt" "$base_url/checksums.txt"

# --- checksum verification ---------------------------------------------
if command -v shasum >/dev/null 2>&1; then
	sha_cmd="shasum -a 256"
elif command -v sha256sum >/dev/null 2>&1; then
	sha_cmd="sha256sum"
else
	echo "lazyrecall: neither shasum nor sha256sum is available" >&2
	exit 1
fi

expected="$(awk -v a="$archive" '$2 == a {print $1}' "$tmpdir/checksums.txt")"
if [ -z "$expected" ]; then
	echo "lazyrecall: $archive not found in checksums.txt" >&2
	exit 1
fi
actual="$($sha_cmd "$tmpdir/$archive" | awk '{print $1}')"
if [ "$actual" != "$expected" ]; then
	echo "lazyrecall: checksum mismatch for $archive" >&2
	echo "  expected: $expected" >&2
	echo "  actual:   $actual" >&2
	exit 1
fi

# --- install ------------------------------------------------------------
installdir="${LAZYRECALL_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$installdir"
tar -xzf "$tmpdir/$archive" -C "$installdir"
chmod +x "$installdir/lazyrecall"

echo "Installed lazyrecall ${version} to $installdir/lazyrecall"

case ":$PATH:" in
*":$installdir:"*) ;;
*) echo "warning: $installdir is not on your PATH" >&2 ;;
esac
