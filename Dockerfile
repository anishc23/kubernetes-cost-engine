# Multi-stage build producing a minimal, non-root image.

FROM golang:1.27-alpine AS build

# git is needed only to stamp the commit into the binary.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Dependencies are copied and downloaded first so that the module cache layer is
# reused across source changes, which is the difference between a 5-second and a
# 90-second rebuild during development.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG TARGETOS=linux
ARG TARGETARCH

# CGO is disabled so the binary is statically linked and can run on a distroless
# or scratch base without a libc.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/anishc23/k8s-cost-optimizer/internal/version.Version=${VERSION} \
        -X github.com/anishc23/k8s-cost-optimizer/internal/version.Commit=${COMMIT} \
        -X github.com/anishc23/k8s-cost-optimizer/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/optimizer ./cmd/optimizer \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/koctl ./cmd/koctl

# --- runtime ---
# Distroless static: no shell, no package manager, no libc. There is nothing for
# an attacker who achieves execution to pivot with, and it keeps the image small.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/optimizer /usr/local/bin/optimizer
COPY --from=build /out/koctl /usr/local/bin/koctl

# 65532 is the distroless "nonroot" user. Set explicitly rather than relying on
# the base image default, so that a base image change cannot silently promote the
# container to root.
USER 65532:65532

EXPOSE 8080 9090

ENTRYPOINT ["/usr/local/bin/optimizer"]
