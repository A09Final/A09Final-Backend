# Build stage: compile a static binary.
FROM golang:1.24-alpine AS build
WORKDIR /src
# Download the modules first, so Docker can reuse this layer when only the code changes.
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/beforefire-backend ./cmd/beforefire-backend

# Runtime stage: no shell, includes CA certificates for APNs, runs as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/beforefire-backend /beforefire-backend
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["/beforefire-backend", "-healthcheck"]
ENTRYPOINT ["/beforefire-backend"]
