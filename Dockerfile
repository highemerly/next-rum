# syntax=docker/dockerfile:1

# --- build stage ---
FROM golang:1.26 AS build
WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

# Assets are embedded into the binary (see embed.go).
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/rum-collector ./cmd/rum-collector

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/rum-collector /rum-collector

# rum-config.json is mounted from a ConfigMap at /etc/rum/rum-config.json.
ENV CONFIG_PATH=/etc/rum/rum-config.json \
    LISTEN_ADDR=:8080

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/rum-collector"]
