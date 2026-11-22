# syntax=docker/dockerfile:1@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183

# --- build stage ---
FROM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
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
