#!/bin/sh
# Usage: notices.sh <version> <output file>
#
# Writes the third-party notices shipped with a release build: the licenses
# of Go, of the Go modules linked into dibi, of DuckDB and the libraries it
# bundles (build/licenses/duckdb.txt) and, for Windows, of the static Qt and
# the MinGW runtime that dibi.exe links.
#
# Run it after the build, where it was built: on Linux natively, for Windows
# in the image of build/windows (make notices, make windows-notices). The
# Go module licenses are read from the module cache.
set -eu
cd "$(dirname "$0")/.."
VERSION=$1
OUT=$2

GOOS=$(go env GOOS)
TAGS=
[ "$GOOS" = windows ] && TAGS=windowsqtstatic
QT=/opt/qt6
if [ "$GOOS" = windows ] && [ ! -d $QT/licenses ]; then
	echo "notices.sh: no $QT/licenses, run it in the image of build/windows" >&2
	exit 1
fi

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

{
	echo "Third-party notices for Dibi $VERSION ($GOOS)"
	echo
	echo "Dibi is distributed under the MIT License (see LICENSE). It includes"
	echo "the third-party software listed below, each under its own license,"
	echo "whose texts follow."
	if [ "$GOOS" = windows ]; then
		QT_VERSION=$(sed -n 's/^#define QT_VERSION_STR "\(.*\)".*/\1/p' $QT/include/QtCore/qconfig.h)
		cat <<EOF

dibi.exe contains Qt $QT_VERSION (qtbase), linked statically, which is used
under the GNU Lesser General Public License v3 (LGPL). The LGPL lets you
replace Qt with a modified version of it. The complete source code of Dibi,
with the scripts that build it, is at https://github.com/marcolardera/dibi
(this release is the tag $VERSION): build/windows/Dockerfile downloads the
source of Qt from
https://download.qt.io/official_releases/qt/${QT_VERSION%.*}/$QT_VERSION/submodules/qtbase-everywhere-src-$QT_VERSION.tar.xz
and builds it; change it to build your modified Qt, then build dibi.exe
with "make windows", as described in the README.
EOF
	else
		echo
		echo "Qt is not included: dibi uses the Qt 6 libraries installed on the system."
	fi

	section "Go"
	if [ -f "$(go env GOROOT)/LICENSE" ]; then
		licenses "$(go env GOROOT)"
	else
		licenses /usr/share/licenses/golang # Fedora's Go
	fi

	section "Go modules"
	CGO_ENABLED=1 go list -deps -tags "$TAGS" \
		-f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' \
		./cmd/dibi | sort -u |
		while read -r path version dir; do
			printf '\n\n%s %s\n' "$path" "$version"
			licenses "$dir"
		done

	section "DuckDB"
	tail -n +2 build/licenses/duckdb.txt

	if [ "$GOOS" = windows ]; then
		section "Qt $QT_VERSION"
		# Qt's attributions of the code it bundles, then the texts of their
		# licenses and of the LGPL (with the GPL it builds on). Qt is used
		# under the LGPL, not its commercial license.
		python3 - $QT/licenses <<'EOF'
import glob, json, os, re, sys

root = sys.argv[1]
ids = {"LGPL-3.0-only", "GPL-3.0-only"}
seen = set()
for path in sorted(glob.glob(root + "/**/qt_attribution.json", recursive=True)):
    with open(path, encoding="utf-8") as f:
        data = json.load(f, strict=False)  # some have raw tabs in strings
    for e in data if isinstance(data, list) else [data]:
        key = (e.get("Name"), e.get("Version"))
        if key in seen:
            continue
        seen.add(key)
        ids.update(re.findall(r"[\w.+-]+", e.get("LicenseId", "")))
        print("\n\n" + e.get("Name", e.get("Id", "")), e.get("Version", ""))
        print("License:", e.get("License", e.get("LicenseId", "")))
        copyright = e.get("Copyright", "")
        if isinstance(copyright, list):
            copyright = "\n".join(copyright)
        if copyright:
            print(copyright)
ids.discard("LicenseRef-Qt-Commercial")
for i in sorted(ids):
    path = os.path.join(root, "LICENSES", i + ".txt")
    if os.path.exists(path):
        print("\n\n----- " + i + "\n")
        with open(path, encoding="utf-8") as f:
            print(f.read())
EOF

		section "MinGW-w64 runtime and GCC runtime libraries"
		for p in ucrt64-crt ucrt64-winpthreads ucrt64-libgcc ucrt64-libstdc++; do
			printf '\n\n%s\n' "$(rpm -q $p)"
			for f in $(rpm -qL $p); do
				printf '\n----- %s\n\n' "$(basename "$f")"
				cat "$f"
			done
		done
	fi
} > "$OUT"
