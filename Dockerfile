FROM oven/bun:1.4.3-alpine@sha256:629e17411f1f129dbec3af78d5af9c9f2a937435c80349437206c6b0b7422373 AS panel-builder

WORKDIR /panel

COPY web/management-center/package.json web/management-center/bun.lock ./
RUN --mount=type=cache,id=cpamc-bun-1.4.3,target=/root/.bun/install/cache \
    bun install --frozen-lockfile
COPY web/management-center/ ./

ARG CPAMC_COMMIT
ARG PANEL_BUILT_AT=1970-01-01T00:00:00Z
RUN test -n "$CPAMC_COMMIT" && VERSION="$CPAMC_COMMIT" bun run build \
    && mkdir -p dist/management-assets \
    && html_sha="$(sha256sum dist/index.html | cut -d' ' -f1)" \
    && printf '{"schema_version":1,"cpamc_commit":"%s","management_api":{"min":1,"max":1},"built_at":"%s","sha256":"%s"}\n' \
         "$CPAMC_COMMIT" "$PANEL_BUILT_AT" "$html_sha" > dist/management-artifact.json

FROM golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514 AS builder

WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential git \
    && apt-get clean \
    && find /var/lib/apt/lists -mindepth 1 -delete

COPY go.mod go.sum ./

RUN go mod download

COPY . .

# The panel is always rebuilt from the pinned submodule inside this build, so the
# embedded bundle never depends on committed or working-tree build output.
COPY --from=panel-builder /panel/dist/index.html /app/internal/managementasset/bundled/management.html
COPY --from=panel-builder /panel/dist/management-artifact.json /app/internal/managementasset/bundled/management-artifact.json
COPY --from=panel-builder /panel/dist/management-assets /app/internal/managementasset/bundled/management-assets

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

FROM debian:bookworm@sha256:6ebd97fa83deb272194a2cf015b3d26a4d538e9ad3a7a79d544c8af5b0a01443

RUN apt-get update \
    && apt-get install -y --no-install-recommends tzdata ca-certificates nodejs npm \
    && apt-get clean \
    && find /var/lib/apt/lists -mindepth 1 -delete

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI
COPY --from=builder ./app/internal/managementasset/bundled/management.html /CLIProxyAPI/bundled/management.html
COPY --from=builder ./app/internal/managementasset/bundled/management-artifact.json /CLIProxyAPI/bundled/management-artifact.json
COPY --from=builder ./app/internal/managementasset/bundled/management-assets /CLIProxyAPI/bundled/management-assets
COPY LICENSE /CLIProxyAPI/licenses/CPA-LICENSE
COPY web/management-center/LICENSE /CLIProxyAPI/licenses/CPAMC-LICENSE
COPY internal/cpauk/LICENSE.upstream /CLIProxyAPI/licenses/CPAUK-LICENSE

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
