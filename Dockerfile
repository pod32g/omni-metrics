# syntax=docker/dockerfile:1

# --- build stage: compile a static, self-contained binary (web assets are
#     embedded via embed.FS, so the runtime image needs nothing else) ---
FROM golang:1.25-bookworm AS build
WORKDIR /src

# Cache module downloads separately from the source for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=0.1.0-m1
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /omni ./cmd/omni

# --- runtime stage: distroless static. The `omni healthcheck` subcommand lets
#     the container self-probe without a shell or curl. ---
FROM gcr.io/distroless/static-debian12

COPY --from=build /omni /omni

EXPOSE 9090
VOLUME ["/data"]

ENTRYPOINT ["/omni"]
CMD ["-listen", "0.0.0.0:9090", "-storage", "/data"]
