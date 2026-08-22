# Veritix ships as a single binary with DuckDB statically linked and the web
# interface embedded, so the runtime image needs nothing but a C runtime and CA
# certificates. There is no Node, no Python and no DuckDB install in it.

# ── the interface ──────────────────────────────────────────────────────────
#
# Node is a build-time requirement only. This stage exists because a container
# built without it produces a working API and a page that says the interface is
# missing — and the interface is the primary one for the people this is for.
# The versions are pinned where the repository pins them: web/.nvmrc for Node
# and package.json's packageManager for pnpm, fetched by corepack.
FROM node:24-bookworm-slim AS web

WORKDIR /web
RUN corepack enable

# The lockfile is the source of truth, as go.sum is. --frozen-lockfile means an
# install that would rewrite it fails instead of resolving something new inside
# a build nobody is watching.
COPY web/package.json web/pnpm-lock.yaml web/.npmrc ./
RUN corepack pnpm install --frozen-lockfile

COPY web/ ./
RUN corepack pnpm build


# ── the binary ─────────────────────────────────────────────────────────────
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# The built interface replaces the committed placeholder, so //go:embed picks
# up a real bundle. This has to land before the build, not after it.
COPY --from=web /web/dist/ ./web/dist/

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

ENV CGO_ENABLED=1
RUN go build -trimpath \
      -ldflags "-s -w \
        -X github.com/russellw/veritix/internal/buildinfo.Version=${VERSION} \
        -X github.com/russellw/veritix/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/russellw/veritix/internal/buildinfo.Date=${DATE}" \
      -o /out/veritix ./cmd/veritix

# Fail the build rather than ship an image whose interface silently did not
# make it in. A missing bundle is invisible until somebody opens the page.
RUN /out/veritix version --json | grep -q '"web": *true' \
      || (echo "the web interface is not embedded in this binary" >&2; exit 1)


# ── what ships ─────────────────────────────────────────────────────────────
FROM gcr.io/distroless/cc-debian12:nonroot

COPY --from=build /out/veritix /usr/local/bin/veritix

# Datasets and the run store live here; mount a volume over it to persist runs.
VOLUME ["/data"]
ENV VERITIX_DATA_DIR=/data

# Containers are reached from outside, so bind to all interfaces. An auth
# token is then mandatory: see internal/cli/serve.go.
ENV VERITIX_ADDR=0.0.0.0:8080
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/veritix"]
CMD ["serve"]
