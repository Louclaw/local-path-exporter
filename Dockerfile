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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --chmod=755 /out/local-path-exporter /local-path-exporter
USER 65532:65532
EXPOSE 9100
ENTRYPOINT ["/local-path-exporter"]
