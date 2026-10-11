# The image scripts/build-linux.sh builds the Linux binaries in. It reads no
# files from a build context (scripts/build-linux.sh pipes it to docker).
#
# AlmaLinux 8 because of its C library: a binary runs on the glibc it was
# linked against and anything newer, and 8 has glibc 2.28, older than every
# server distribution still supported. It is also where DuckDB builds the
# static library the daemon links in (duckdb-go-bindings: manylinux_2_28,
# which is AlmaLinux 8, with Red Hat's GCC 12), so that library meets the
# C++ runtime it expects. gcc-toolset-14 is the newest compiler 8 has.
#
# Go comes out of the official image, so one line names its version. It must
# be at least the "go" line in go.mod.
ARG GO_VERSION=1.27.2
FROM golang:${GO_VERSION}-bookworm AS go

FROM almalinux:8.10
RUN dnf install -y gcc-toolset-14-gcc gcc-toolset-14-gcc-c++ gcc-toolset-14-binutils \
    && dnf clean all \
    # -lstdc++ takes the first libstdc++ on the search path. A folder that
    # holds only the static one, put first with -L, makes that the one: the
    # binaries then need nothing of the machine but glibc itself.
    && mkdir -p /opt/static-libstdc++ \
    && ln -s "$(/opt/rh/gcc-toolset-14/root/usr/bin/gcc -print-file-name=libstdc++.a)" /opt/static-libstdc++/libstdc++.a \
    && test -f /opt/static-libstdc++/libstdc++.a
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/opt/rh/gcc-toolset-14/root/usr/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    CGO_ENABLED=1 \
    CGO_LDFLAGS="-L/opt/static-libstdc++ -static-libgcc"
