# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e

# --- build stage ---
FROM golang:1.27.3@sha256:c292963ad0e5f51e843c726edc204a4e4c26a859ba1e4d736d91028aa3633048 AS build
WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

# Assets are embedded into the binary (see embed.go).
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/rum-collector ./cmd/rum-collector

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
WORKDIR /
COPY --from=build /out/rum-collector /rum-collector

# rum-config.json is mounted from a ConfigMap at /etc/rum/rum-config.json.
ENV CONFIG_PATH=/etc/rum/rum-config.json \
    LISTEN_ADDR=:8080

EXPOSE 8080
# distroless nonroot の UID/GID を数値で指定する。名前 (nonroot) だと
# Kubernetes の runAsNonRoot が非 root かを検証できず起動に失敗する。
USER 65532:65532
ENTRYPOINT ["/rum-collector"]
