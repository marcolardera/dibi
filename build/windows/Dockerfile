# Cross-compiles dibi.exe (static, x86_64, UCRT) on Linux.
#
# The prebuilt DuckDB Windows library targets the MinGW UCRT runtime with
# POSIX threads, so Qt and the Go cgo code must use the same toolchain.
# Fedora ships such a toolchain (ucrt64-gcc) but no Qt for it: qtbase is
# built here from source, statically, with the bundled third-party libs.
#
# Usage: make windows (see the Makefile). Pass --build-arg QT_VERSION=x.y.z
# to change Qt; it must match the qt6-qtbase-devel version in Fedora.

FROM fedora:44

ARG QT_VERSION=6.11.2

RUN dnf -y install --setopt=install_weak_deps=False \
        ucrt64-gcc ucrt64-gcc-c++ ucrt64-winpthreads-static ucrt64-filesystem \
        qt6-qtbase-devel cmake ninja-build perl python3 xz pkgconf-pkg-config \
        golang git && \
    dnf clean all

# Qt host tools (moc, rcc, uic...) come from Fedora's qt6-qtbase-devel,
# which must be the same version as the cross-compiled Qt.
RUN test "$(rpm -q --qf '%{VERSION}' qt6-qtbase-devel)" = "${QT_VERSION}"

RUN curl -fsSL -o /tmp/qtbase.tar.xz \
        "https://download.qt.io/official_releases/qt/${QT_VERSION%.*}/${QT_VERSION}/submodules/qtbase-everywhere-src-${QT_VERSION}.tar.xz" && \
    mkdir /tmp/qtbase && tar -xJf /tmp/qtbase.tar.xz -C /tmp/qtbase --strip-components=1 && \
    mkdir /tmp/qtbuild && cd /tmp/qtbuild && \
    /tmp/qtbase/configure \
        -prefix /opt/qt6 -static -release -opensource -confirm-license \
        -nomake examples -nomake tests \
        -qt-host-path /usr \
        -qt-zlib -qt-libpng -qt-libjpeg -qt-freetype -qt-harfbuzz -qt-pcre \
        -no-openssl -no-dbus -no-icu -no-feature-sql -no-feature-testlib \
        -- -DCMAKE_TOOLCHAIN_FILE=/usr/share/mingw/toolchain-ucrt64.cmake \
           -DQT_HOST_PATH_CMAKE_DIR=/usr/lib64/cmake && \
    cmake --build . --parallel && \
    cmake --install . && \
    cd / && rm -rf /tmp/qtbase /tmp/qtbase.tar.xz /tmp/qtbuild

COPY qt-pkgconfig.sh /usr/local/bin/
RUN qt-pkgconfig.sh /opt/qt6

ENV CC=x86_64-w64-mingw32ucrt-gcc \
    CXX=x86_64-w64-mingw32ucrt-g++ \
    PKG_CONFIG=pkg-config \
    PKG_CONFIG_LIBDIR=/opt/qt6/lib/pkgconfig \
    CGO_ENABLED=1 \
    GOOS=windows \
    GOARCH=amd64 \
    GOTOOLCHAIN=local \
    GOFLAGS=-buildvcs=false \
    CGO_LDFLAGS_ALLOW=.*
