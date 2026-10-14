#!/bin/sh
# Usage: notices.sh <version> <output file>
#
# Writes the third-party notices shipped with a release build: the licenses
# of Go, of the Go modules linked into dibi, of DuckDB and the libraries it
# bundles (build/licenses/duckdb.txt) and, for Windows, of the Qt and C
# libraries linked statically from MSYS2, with the texts of the LGPL and GPL.
#
# Run it after the build, on the platform being built: the Go module
# licenses are read from the module cache and, on Windows, the MSYS2 ones
# from the installed packages (the libraries are found in the pkg-config
# files that make windows-native writes into bin/.qt-pkgconfig).
set -eu
cd "$(dirname "$0")/.."
VERSION=$1
OUT=$2

GOOS=$(go env GOOS)
TAGS=
[ "$GOOS" = windows ] && TAGS=windowsqtstatic
MSYS=/ucrt64
PKG=mingw-w64-ucrt-x86_64-

# unixpath <path>: a path Go printed, usable from MSYS2's shell.
unixpath() {
	if command -v cygpath >/dev/null; then cygpath -u "$1"; else echo "$1"; fi
}

# licenses <dir>: the license files in dir, each under a header.
licenses() {
	for f in "$1"/*; do
		case $(basename "$f" | tr '[:upper:]' '[:lower:]') in
		licen[cs]e* | copying* | notice*)
			[ -f "$f" ] || continue
			printf '\n----- %s\n\n' "$(basename "$f")"
			cat "$f"
			;;
		esac
	done
}

section() {
	printf '\n\n%s\n%s\n' "$1" "$(echo "$1" | sed 's/./=/g')"
}

# The DuckDB licenses must match the linked version.
N=$(sed -n 's|^\s*github.com/duckdb/duckdb-go-bindings v0\.\([0-9]*\)\..*|\1|p' go.mod)
DUCKDB=$((N / 10000)).$((N / 100 % 100)).$((N % 100))
if [ "$(head -1 build/licenses/duckdb.txt)" != "DuckDB $DUCKDB" ]; then
	echo "notices.sh: build/licenses/duckdb.txt is not for DuckDB $DUCKDB, run build/duckdb-licenses.sh" >&2
	exit 1
fi

# The MSYS2 packages that own the static libraries dibi.exe links, plus the
# C/C++ runtime ones that the linker adds by itself.
msys_packages() {
	{
		for lib in $(sed -n 's/^Libs: //p' bin/.qt-pkgconfig/*.pc); do
			case $lib in
			-l*) f=$MSYS/lib/lib${lib#-l}.a ;;
			*.a) f=$(unixpath "$lib") ;;
			*) continue ;;
			esac
			[ -f "$f" ] && pacman -Qqo "$f" 2>/dev/null
		done
		for p in qt6-static gcc-libs winpthreads libwinpthread crt-git; do
			pacman -Qq "$PKG$p" 2>/dev/null
		done
	} | sort -u
}

{
	echo "Third-party notices for Dibi $VERSION ($GOOS)"
	echo
	echo "Dibi is distributed under the MIT License (see LICENSE). It includes"
	echo "the third-party software listed below, each under its own license,"
	echo "whose texts follow."
	if [ "$GOOS" = windows ]; then
		QT=$(pacman -Q "${PKG}qt6-static" | cut -d' ' -f2)
		cat <<EOF

dibi.exe contains Qt $QT (qtbase), linked statically, which is used under
the GNU Lesser General Public License v3 (LGPL), as are some of the other
libraries listed below. The LGPL lets you replace these libraries with
modified versions of them. The complete source code of Dibi, with the
scripts that build it, is at https://github.com/marcolardera/dibi (this
release is the tag $VERSION): build dibi.exe with "make windows-native",
as described in the README, against the modified libraries.

The libraries come from the MSYS2 packages listed below. Their source code
is at https://repo.msys2.org/mingw/sources/ and their build scripts at
https://github.com/msys2/MINGW-packages.
EOF
	else
		echo
		echo "Qt is not included: dibi uses the Qt 6 libraries installed on the system."
	fi

	section "Go"
	licenses "$(unixpath "$(go env GOROOT)")"

	section "Go modules"
	CGO_ENABLED=1 go list -deps -tags "$TAGS" \
		-f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' \
		./cmd/dibi | sort -u |
		while read -r path version dir; do
			printf '\n\n%s %s\n' "$path" "$version"
			licenses "$(unixpath "$dir")"
		done

	section "DuckDB"
	tail -n +2 build/licenses/duckdb.txt

	if [ "$GOOS" = windows ]; then
		section "MSYS2 packages"
		for p in $(msys_packages); do
			printf '\n\n%s\n' "$(pacman -Q "$p")"
			for f in $(pacman -Qlq "$p" | grep '/share/licenses/.*[^/]$'); do
				printf '\n----- %s\n\n' "${f#*/share/licenses/}"
				cat "$f"
			done
		done

		section "GNU Lesser General Public License v3"
		cat build/licenses/LGPL-3.0.txt
		section "GNU General Public License v3"
		cat build/licenses/GPL-3.0.txt
	fi
} > "$OUT"
