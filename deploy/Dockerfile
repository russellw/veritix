# Veritix ships as a single binary with DuckDB statically linked, so the
# runtime image needs nothing but a C runtime and CA certificates.

FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

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
