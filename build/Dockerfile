# syntax=docker/dockerfile:1
# Build from the repo root for Cloud Run (linux/amd64):
#   docker build --platform linux/amd64 -f build/Dockerfile -t tf-review:local .
# Every FROM is pinned to the multi-arch index digest; --platform picks the amd64 manifest.

FROM golang:1.26.11@sha256:4165ee6bf3b7811fd7b40373290405c83d0167dcb87ee5f29c427cdcef45ff8f AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tf-review ./cmd/tf-review

FROM hashicorp/terraform-mcp-server:1.4.1@sha256:11dd418bba6003dd5777e9d0979b68de59929a155b355f338f407947e00e9d80 AS mcp

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=builder /out/tf-review /app/tf-review
COPY --from=mcp /bin/terraform-mcp-server /app/terraform-mcp-server
COPY build/config.json /app/config.json
USER nonroot
ENTRYPOINT ["/app/tf-review"]
