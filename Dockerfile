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
FROM --platform=${BUILDPLATFORM} golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS builder
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
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
LABEL org.opencontainers.image.source=https://github.com/MicleMaslow/cluster-api-ipam-provider-netbox
WORKDIR /
COPY --from=builder /workspace/manager .
# Numeric uid and gid, as Kubernetes expects for runAsNonRoot; the same as the manifests set.
USER 65530:65530
ENTRYPOINT ["/manager"]
