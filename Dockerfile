# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.22.2-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/local-path-exporter ./cmd/local-path-exporter

FROM gcr.io/distroless/static-debian12
COPY --from=build --chmod=755 /out/local-path-exporter /local-path-exporter
# Soft heap cap under the 128Mi cgroup limit used by the example DaemonSet.
ENV GOMEMLIMIT=100MiB
# UID 0 so the runtime puts DAC_READ_SEARCH in the effective set.
# A non-root user plus allowPrivilegeEscalation=false never does.
USER 0:0
EXPOSE 9100
ENTRYPOINT ["/local-path-exporter"]
