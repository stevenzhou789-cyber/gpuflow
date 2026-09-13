FROM --platform=$BUILDPLATFORM node:22-alpine AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
COPY go.mod go.sum /src/
COPY cmd/ /src/cmd/
COPY internal/ /src/internal/
COPY pkg/ /src/pkg/
COPY scripts/check-community-boundary.mjs /src/scripts/
# Check original copied source and the fresh embedded bundle. The Go stage
# depends on this stage, so image publication cannot bypass either check.
RUN node /src/scripts/check-community-boundary.mjs --source-tree /src && \
    mkdir -p ../internal/webui && npm run build && \
    node /src/scripts/check-community-boundary.mjs --source-tree /src

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS go-build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
COPY --from=web-build /src/internal/webui/dist ./internal/webui/dist
RUN CGO_ENABLED=0 go test ./...
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/gpuflow ./cmd/gpuflow

FROM alpine:3.21
RUN apk add --no-cache ca-certificates docker-cli
COPY --from=go-build /out/gpuflow /usr/local/bin/gpuflow
LABEL org.opencontainers.image.title="GPUFlow" \
      org.opencontainers.image.description="Lightweight BYOC GPU batch scheduler and agent" \
      org.opencontainers.image.licenses="MIT"
ENTRYPOINT ["gpuflow"]
CMD ["server"]
