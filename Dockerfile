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

# Build the manager binary.
# Run with --build-arg builder_image=<golang:x.y.z> to override the Go toolchain image.
ARG builder_image=golang:1.27.1

# The build runs on the native platform and cross-compiles for the target.
FROM --platform=${BUILDPLATFORM} ${builder_image} AS builder
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
FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.source=https://github.com/MicleMaslow/cluster-api-ipam-provider-netbox
WORKDIR /
COPY --from=builder /workspace/manager .
# Numeric uid of the nonroot user, as Kubernetes expects for runAsNonRoot.
USER 65532
ENTRYPOINT ["/manager"]
