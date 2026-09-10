FROM --platform=$BUILDPLATFORM node:22-alpine AS web
ENV NODE_OPTIONS=--max-old-space-size=768
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN mkdir -p ../internal/webui && npm run build

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS verified
ENV CGO_ENABLED=0 GOMAXPROCS=2 GOMEMLIMIT=768MiB
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY pkg/ ./pkg/
COPY --from=web /src/internal/webui/dist ./internal/webui/dist
RUN go test -count=1 -p 2 ./... && go vet -p 2 ./...
RUN mkdir -p /out/linux-amd64 /out/linux-arm64 /out/windows-amd64 && \
    GOOS=linux GOARCH=amd64 go build -p 2 -trimpath -ldflags="-s -w" -o /out/linux-amd64/gpuflow ./cmd/gpuflow && \
    GOOS=linux GOARCH=arm64 go build -p 2 -trimpath -ldflags="-s -w" -o /out/linux-arm64/gpuflow ./cmd/gpuflow && \
    GOOS=windows GOARCH=amd64 go build -p 2 -trimpath -ldflags="-s -w" -o /out/windows-amd64/gpuflow.exe ./cmd/gpuflow

FROM scratch AS binaries
COPY --from=verified /out/ /

FROM alpine:3.21 AS runtime
ARG TARGETARCH
ARG VCS_REF
RUN apk add --no-cache ca-certificates docker-cli
COPY --from=verified /out/linux-${TARGETARCH}/gpuflow /usr/local/bin/gpuflow
LABEL org.opencontainers.image.title="GPUFlow" \
      org.opencontainers.image.description="Lightweight BYOC GPU batch scheduler and agent" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.revision=$VCS_REF
ENTRYPOINT ["gpuflow"]
CMD ["server"]
