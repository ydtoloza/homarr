# homarr-go — single static binary, no Node/Redis/nginx at runtime.
# Build context = repo root:  docker build -f server/Dockerfile .
# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/homarr-go ./cmd/homarr-go

FROM gcr.io/distroless/static-debian12:nonroot
# The frontend is embedded in the binary via go:embed — nothing else to copy.
COPY --from=build /out/homarr-go /homarr-go
ENV HOMARR_PORT=7575 HOMARR_DATA_DIR=/appdata
EXPOSE 7575
VOLUME /appdata
USER nonroot:nonroot
ENTRYPOINT ["/homarr-go"]
