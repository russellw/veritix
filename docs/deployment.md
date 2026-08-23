# Deploying Veritix

Veritix is a program the customer runs, on their own machine or in their own
cluster. It is not SaaS, and every decision here follows from that: the
question is never "how do we reach the customer's data" but "how does this run
somewhere the data already is".

Three shapes, in increasing order of ceremony.

## 1. The binary

```sh
veritix serve
```

Loopback, no token, no container, nothing to install alongside it — one static
binary with DuckDB linked in and the web interface embedded. This is how one
analyst runs it on a Windows desktop, which is the majority case for this
product.

## 2. The container

```sh
make docker
docker run --rm -p 8080:8080 \
    -v veritix-data:/data \
    -e VERITIX_AUTH_TOKEN="$(openssl rand -hex 16)" \
    veritix:dev
```

`deploy/Dockerfile` is three stages: Node builds the interface, Go builds the
binary with it embedded, and the result is copied into
`gcr.io/distroless/cc-debian12:nonroot` — a C runtime and CA certificates,
nothing else. No Node, no Python, no DuckDB install.

The build **fails** rather than shipping an image whose interface silently did
not make it in. `go build` on its own produces a working API and a page saying
the interface is missing, which is the right behavior for a developer and the
wrong one for an image somebody deploys.

An image bound to `0.0.0.0` refuses to start without `VERITIX_AUTH_TOKEN`. That
is not a container rule, it is `serve`'s rule: exposing an instance to a network
is a deliberate act, and a deliberate act without authentication is a mistake.

```sh
make docker-smoke
```

runs the image and talks to it: `/health` unauthenticated, the API behind the
token, the interface under its CSP, `--read-only`, a dataset registered by
path, and a schedule accepted in a named time zone. The build can assert things
about the *binary*, and does; distroless has no shell, so anything about the
image that ships has to be asserted from outside it.

The zone is the check that exists nowhere else. A schedule names an IANA zone,
and Go resolves one from the operating system unless the binary carries the
database itself — which is what `internal/schedule`'s `time/tzdata` import is
for. Every machine that runs `go test` has a system zoneinfo that answers
first, so the whole suite passes whether that import is there or not. The
second phase of the smoke check bind-mounts an empty directory over every zone
source Go looks in and asks the container to accept `Europe/London` anyway.
That is not a contrived condition: it is what Go sees on a Windows desktop,
where there is no system zone source at all, and Windows desktops are who the
web interface is for. The check has been run against an image built without the
import, where the same request comes back 400.

The `cc-debian12` base does ship `/usr/share/zoneinfo` today, which is why the
check has to take it away to see anything — and why relying on the base image
for zones would be relying on something nobody here decided.

## 3. Kubernetes

```sh
kubectl create namespace veritix
kubectl -n veritix create secret generic veritix-auth \
    --from-literal=auth-token="$(openssl rand -hex 16)"
kubectl apply -k deploy/kubernetes
kubectl -n veritix port-forward svc/veritix 8080:80
```

`deploy/kubernetes/` is a kustomize base. Four things in it are decisions
rather than boilerplate.

### One replica, and it does not scale sideways

Each run keeps its DuckDB file at `<DataDir>/runs/<id>/dataset.duckdb` so that
`GET /runs/{id}/findings/{fid}/rows` can reopen it read-only afterwards, and the
SQLite store beside it is the audit trail. Both are on the pod's volume.

A second replica would serve a different history, and a request for a run's
offending rows would land on the pod that does not have them. So: `replicas: 1`,
`strategy: Recreate`, a `ReadWriteOnce` volume. Veritix scales by giving a team
an instance, not by adding replicas to one — which is also the licensing shape
and the data-locality shape, so this is a constraint that agrees with itself.

A schedule adds a second reason, since M7b: two replicas would both find the
same window due and both start an audit of the same dataset. If a process must
not run the clock, `schedule.enabled: false` stops it firing anything without
touching a single stored schedule — and expired run databases are still
discarded, because an operator who turned the clock off has not asked for the
disk to fill up. `docs/scheduling.md` is the whole of it.

If that is genuinely too small, the next instance is another namespace with
another volume, not another replica.

### A read-only root filesystem, and therefore two volumes

`readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, all
capabilities dropped, `runAsNonRoot`, `seccompProfile: RuntimeDefault`. The
process writes in exactly two places, and both are volumes: `/data` for
everything that has to survive, and an `emptyDir` at `/tmp` because DuckDB
spills large joins to disk. `engine.temp_dir: /tmp` in the ConfigMap is what
points it there, and it is also where a run that was not given a database path
of its own puts one — so `/tmp` wants room for roughly a third of the largest
dataset anybody audits from inside this container, on top of the spill.

### Memory: the limit that matters is the smaller one

DuckDB has its own memory limit and spills to disk when it reaches it. The
cgroup has a limit too, and reaching *that* is an OOMKill.

Keep `engine.memory_limit` comfortably below the container's `resources.limits.
memory`. The base ships 2GB against 3Gi, which is the right shape: hitting the
first costs time, hitting the second costs the run.

It bounds the working set, not the dataset. Every run keeps its tables in a
DuckDB file — on `/data` for a run started over HTTP or over MCP, which is
where the per-finding rows endpoint reopens it, and in a scratch directory
under `engine.temp_dir` for one started from the command line — so 2GB here is
not a ceiling of two gigabytes of CSV. What it does require is room on those
volumes: DuckDB's storage is compressed, and a dataset lands at roughly a third
of its CSV size. [scale.md](scale.md) has what a 2 GB dataset actually costs.

### Time: the limit that matters is the larger one

`engine.query_timeout` bounds one of Veritix's own measurements over one
column, and the base ships 30 minutes. It is deliberately generous, because the
failure it produces when it is too small is not a failed audit: the column is
dropped, a stub that reads as clean takes its place, and the rest of the
dataset is reported as if the table had been looked at. Measured at 20M rows,
every column of the largest table timed out against a two-minute limit and the
audit reported a clean bill of health on it. `column.not_profiled` is the
finding that makes that visible now, and `--fail-on warning` is what turns it
into a non-zero exit.

`engine.agent_query_timeout` is the short one, at two minutes, and it bounds a
statement the model wrote. That is where a runaway query actually comes from.

### Egress denied by default

`deploy/kubernetes/networkpolicy.yaml` denies egress and allows back DNS.
Nothing else, until somebody uncomments something.

This is the product's central promise written as a cluster object. Veritix's
egress guard bounds what the *agent* sends to a model, and the trace at
`/runs/{id}/trace` records every byte of it; a NetworkPolicy bounds what the
process can reach at all. They answer different questions and are worth having
together — one is a design a reviewer has to read, the other is a control an
auditor can check.

Three commented blocks are where a real deployment opens it back up: a model
endpoint, an OpenTelemetry collector, and — since M7b — a notification webhook,
which is the only one of the three that a deterministic install with no model
and no telemetry might still want. A model endpoint *inside the cluster*
is the case this product is built for. Reaching `api.anthropic.com` means
opening egress to the internet, and that is a different decision — one worth
making explicitly, with the trace as the record of what actually left.

It needs a CNI that enforces NetworkPolicy. Where nothing enforces it the
object is accepted and does nothing, which is worth knowing before treating it
as a control.

## What is in the data directory

| | |
|---|---|
| `veritix.db` | SQLite: datasets, runs, findings, traces, proposals — the audit trail |
| `runs/<id>/dataset.duckdb` | the loaded data for one run, so rows can be shown later |
| `datasets/<id>/` | uploaded files |
| `datasets/<id>/rules.yaml` | rules accepted from the agent's proposals, applied to every later audit of that dataset |

`runs/<id>/dataset.duckdb` is the one that grows without bound once audits are
scheduled — roughly a third of the dataset's size, every run.
`server.retain_databases` discards the old ones and keeps everything else; see
`docs/scheduling.md`.

Back up `veritix.db` and `datasets/`. The DuckDB files are re-creatable from
the sources; the SQLite store is the thing somebody wants six months later, and
`datasets/<id>/rules.yaml` is what a person decided to enforce.

Two databases, on purpose: DuckDB holds content that is large, disposable and
re-creatable; SQLite holds the record of what was done, which is small and
long-lived. `internal/store` knows nothing about the report's shape, so
changing the report schema is not a migration.

## Configuration

Precedence, lowest to highest: defaults, config file, `VERITIX_*` environment,
flags. The container reads `/etc/veritix/config.yaml` from a ConfigMap through
`VERITIX_CONFIG`; secrets — the auth token, a model API key — come from the
environment, out of a Secret, and never from the file.

The settings a deployment usually touches:

| | |
|---|---|
| `VERITIX_ADDR` | listen address. Non-loopback requires a token. |
| `VERITIX_AUTH_TOKEN` | bearer token required on every API request |
| `VERITIX_DATA_DIR` | where the audit trail lives |
| `VERITIX_SOURCE_URL` | the AGPL §13 source offer shown in the interface's footer |
| `VERITIX_ENGINE_MEMORY_LIMIT` | keep it under the container's limit |
| `VERITIX_LLM_PROVIDER` | `none` by default. Setting it offers the agent per run. |
| `VERITIX_OTEL_ENABLED` | off by default |
| `VERITIX_SCHEDULE_ENABLED` | on by default. It runs the clock; a schedule still has to exist. |
| `VERITIX_RETAIN_DATABASES` | how long a run's ingested data is kept. `336h` (14 days) by default. |
| `VERITIX_NOTIFY_WEBHOOK_URL` | empty by default. Where a scheduled audit's regressions are sent. |

A duration here is a Go duration, which has **no day unit**: `336h`, not `14d`.
In the config file `14d` is a startup error; in the environment it is worse,
because a value that will not parse is ignored and the default silently stands.

### The source offer

`server.source_url` is not decoration. If you modified Veritix and serve it to
other people, AGPL section 13 obliges you to offer *those people* your source,
and the interface's footer makes that offer on every screen including the token
gate — which is why it rides on `/health`, the one unauthenticated endpoint.
Point it at your fork. Empty removes the link, which is the right setting for a
build shipped under commercial terms.

## TLS

Veritix serves plain HTTP and should sit behind something that terminates TLS —
an Ingress, a Gateway, a reverse proxy. It does not manage certificates, and a
tool whose value is that it holds sensitive data has no business also being a
half-competent TLS implementation.

The bearer token is sent on every request, so the hop in front of Veritix has
to be encrypted anywhere but loopback.

## OpenTelemetry

Off by default, and that is deliberate rather than lazy: an OTLP endpoint is a
network egress, and this process holds data the customer will not send to a
vendor. A build that started exporting because an ambient
`OTEL_EXPORTER_OTLP_ENDPOINT` happened to be set would be Veritix making that
decision for them.

```yaml
otel:
  enabled: true
  endpoint: http://opentelemetry-collector.observability.svc.cluster.local:4318
  service_name: veritix
```

Once enabled, the standard `OTEL_EXPORTER_OTLP_*` variables are honored by the
exporters, so an existing collector configuration works. Enabling is Veritix's
switch; where to send is the operator's.

**What a span carries.** Counts, durations, stage names, tool names, severity,
the provider and model identifiers, the stop reason, the run id. **Never** a
table name, a column name, a file path, SQL text, model prose, or a cell value.
A span is an access log that leaves the machine, and the schema of a customer's
export is itself commercially sensitive. `TestNoSpanCarriesCustomerData` runs a
real audit of the fixtures and scans every exported span for exactly that — the
same shape of test as the one that scans reports and the one that scans what
goes to a model.

What you get: a trace per audit with a span per pipeline stage, a span per
agent step and tool call, and a span per HTTP request; metrics for run count
and duration, findings by severity and origin, agent steps and tokens.

## In CI

The exit code is the interface:

```sh
veritix audit ./exports --fail-on error
```

Non-zero when anything of that severity was found. `--format sarif` uploads to
GitHub code scanning. A rule accepted from an agent's proposal is what makes
this gate mean more over time — see [rules-proposal.md](rules-proposal.md).

Run it as a `Job` with the same image, `args: ["audit", "/data/exports",
"--fail-on", "error"]`. It needs no volume beyond the data and no token,
because nothing is being served.

## Upgrading

The store migrates itself on open. The run documents are stored as opaque blobs
and served back verbatim, so a report schema change is not a migration — an old
run keeps rendering the way it did when it ran, which is the point of an audit
trail.

Stop, replace the image, start. With `Recreate` and one replica that is what
the Deployment already does.
