# A5 UMDK libraries require GLIBC_2.34 and GLIBCXX_3.4.29 or later.
ARG BASE_IMAGE=ubuntu:22.04
ARG RUNTIME_LD_LIBRARY_PATH=/usr/local/Ascend/driver/lib64:/usr/local/Ascend/driver/lib64/driver:/usr/local/Ascend/driver/lib64/common:/usr/lib64
FROM $BASE_IMAGE AS build

ARG GO_VERSION=1.26.2
ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive
RUN apt update -y && apt install -y gcc make wget ca-certificates git
# Install the official Go toolchain (matches go.mod). Avoids the fragile
# longsleep/golang-backports PPA, which depends on Launchpad API availability
# and flakes in CI (especially on the emulated arm64 leg).
RUN wget -qO /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:/root/go/bin:$PATH
ARG GOPROXY
ENV GOPATH=/go
ARG VERSION
WORKDIR /build
ADD . .
RUN go mod download github.com/Project-HAMi/HAMi
RUN go get github.com/Project-HAMi/ascend-device-plugin/internal/server
RUN go get huawei.com/npu-exporter
RUN go get huawei.com/npu-exporter/utils/logger@v0.0.0-00010101000000-000000000000
RUN make all

# A5's host-mounted libdcmi needs UnifiedBus user-space libraries. Keep the
# RPM layout: liburma finds its providers under /usr/lib64/urma. A5 nodes are
# arm64; the amd64 image keeps an empty UMDK layer for existing Ascend devices.
FROM $BASE_IMAGE AS umdk
ARG TARGETARCH
ARG UMDK_MIRROR=https://mirrors.huaweicloud.com/ascend/archive/rpm/unifiedbus/common
ARG UMDK_URMA_VERSION=26.06.0-B025
# The B025 archive uses "arrch64" in its filename.
ARG UMDK_URMA_PKG=umdk-urma-${UMDK_URMA_VERSION}.glibc217.arrch64.rpm.tar.gz
RUN set -eu; \
    mkdir /umdk-root; \
    if [ "${TARGETARCH}" = "arm64" ]; then \
      apt-get update && apt-get install -y --no-install-recommends ca-certificates wget libarchive-tools; \
      mkdir -p /umdk/rpms; \
      wget -qO /umdk/urma.rpm.tar.gz "${UMDK_MIRROR}/${UMDK_URMA_PKG}"; \
      tar -xzf /umdk/urma.rpm.tar.gz -C /umdk/rpms; \
      one() { set -- $1; [ $# -eq 1 ] && [ -f "$1" ] || { echo "expected exactly one RPM: $*" >&2; return 1; }; echo "$1"; }; \
      bsdtar -xf "$(one '/umdk/rpms/umdk-urma-lib-[0-9]*.rpm')" -C /umdk-root './usr/lib64/*'; \
      bsdtar -xf "$(one '/umdk/rpms/libummu-[0-9]*.rpm')" -C /umdk-root './usr/lib64/*'; \
      bsdtar -xf "$(one '/umdk/rpms/umdk-urma-bin-[0-9]*.rpm')" -C /umdk-root './usr/lib64/libtpsa.so*'; \
      rm -rf /umdk /var/lib/apt/lists/*; \
    fi

FROM $BASE_IMAGE
ARG TARGETARCH
ARG RUNTIME_LD_LIBRARY_PATH
ENV LD_LIBRARY_PATH=${RUNTIME_LD_LIBRARY_PATH}
COPY --from=umdk /umdk-root/ /
RUN if [ "${TARGETARCH}" = "arm64" ]; then \
      apt-get update && apt-get install -y --no-install-recommends libnl-3-200 libnl-genl-3-200 libstdc++6 && \
      rm -rf /var/lib/apt/lists/*; \
    fi
COPY --from=build /build/ascend-device-plugin /usr/local/bin/ascend-device-plugin
COPY --from=build /build/lib/hami-vnpu-core/* /usr/local/hami-vnpu-core-assets/
# ENPU assets are installed on the node and mounted into workloads.
COPY enpu-runtime-assets/ /usr/local/enpu-runtime-assets/
RUN if [ -e /usr/local/enpu-runtime-assets/libvruntime.so ] || [ -e /usr/local/enpu-runtime-assets/enpu-monitor ]; then \
      cd /usr/local/enpu-runtime-assets && sha256sum -c SHA256SUMS; \
    fi

ENTRYPOINT ["ascend-device-plugin"]
