# Copyright 2026 The cluster-api-ipam-provider-netbox Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Build the manager binary. The build runs on the native platform and cross-compiles for the target.
FROM --platform=${BUILDPLATFORM} golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS builder
WORKDIR /workspace

ARG TARGETOS
ARG TARGETARCH
ARG GIT_VERSION=v0.0.0-dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

# Cache deps before copying the sources.
COPY go.mod go.sum ./
RUN --mount=type=cache,id=go-mod,target=/go/pkg/mod \
    go mod download

COPY . .

ENV VERSION_PKG=github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/version
RUN --mount=type=cache,id=go-build,target=/root/.cache/go-build \
    --mount=type=cache,id=go-mod,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags "-X ${VERSION_PKG}.gitVersion=${GIT_VERSION} -X ${VERSION_PKG}.gitCommit=${GIT_COMMIT} -X ${VERSION_PKG}.buildDate=${BUILD_DATE}" \
    -o manager .

# Production image.
FROM gcr.io/distroless/static:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
LABEL org.opencontainers.image.source=https://github.com/MicleMaslow/cluster-api-ipam-provider-netbox
WORKDIR /
COPY --from=builder /workspace/manager .
# Numeric uid and gid, as Kubernetes expects for runAsNonRoot; the same as the manifests set.
USER 65530:65530
ENTRYPOINT ["/manager"]
