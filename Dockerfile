# syntax=docker/dockerfile:1.7
# Multi-stage build → tiny distroless runtime image (~15 MB).

# ---- Build stage ----
FROM golang:1.24-alpine AS build

WORKDIR /src

# Cache go.mod/go.sum download separately from source for layer reuse.
COPY go.mod go.sum* ./
RUN go mod download

COPY *.go ./

# Static binary (CGO_ENABLED=0) so we can use distroless/static.
# -trimpath strips local paths from binary; -s -w drops debug info → smaller image.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/gigachat-proxy \
    .

# ---- Runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/gigachat-proxy /usr/local/bin/gigachat-proxy

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/gigachat-proxy"]
