FROM oven/bun:1.3.14-alpine@sha256:5acc90a93e91ff07bf72aa90a7c9f0fa189765aec90b47bdbf2152d2196383c0 AS panel-builder

WORKDIR /panel

COPY web/management-center/package.json web/management-center/bun.lock ./
RUN --mount=type=cache,id=cpamc-bun-1.3.14,target=/root/.bun/install/cache \
    bun install --frozen-lockfile
COPY web/management-center/ ./
COPY internal/managementasset/bundled/management-artifact.json /expected/management-artifact.json
RUN cpamc_commit="$(sed -n 's/.*"cpamc_commit": "\([^"]*\)".*/\1/p' /expected/management-artifact.json)" \
    && test -n "$cpamc_commit" \
    && VERSION="$cpamc_commit" bun run build

FROM golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514 AS builder

WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential git \
    && apt-get clean \
    && find /var/lib/apt/lists -mindepth 1 -delete

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

FROM debian:bookworm@sha256:6ebd97fa83deb272194a2cf015b3d26a4d538e9ad3a7a79d544c8af5b0a01443

RUN apt-get update \
    && apt-get install -y --no-install-recommends tzdata ca-certificates \
    && apt-get clean \
    && find /var/lib/apt/lists -mindepth 1 -delete

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI
COPY --from=panel-builder /panel/dist/index.html /CLIProxyAPI/bundled/management.html
COPY internal/managementasset/bundled/management-artifact.json /CLIProxyAPI/bundled/management-artifact.json
COPY LICENSE /CLIProxyAPI/licenses/CPA-LICENSE
COPY web/management-center/LICENSE /CLIProxyAPI/licenses/CPAMC-LICENSE
COPY internal/cpauk/LICENSE.upstream /CLIProxyAPI/licenses/CPAUK-LICENSE

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
