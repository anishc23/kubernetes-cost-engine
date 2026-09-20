# The synthetic workload generator used for end-to-end validation on a real
# cluster. Kept separate from the optimizer image because it is a test fixture,
# not a component of the system, and shipping a CPU-burning binary inside the
# production image would be a poor idea.

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/workload-gen ./cmd/workload-gen

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/workload-gen /usr/local/bin/workload-gen
USER 65532:65532
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/workload-gen"]
