# Contributing to Veritix

Veritix is a one-person project with a commercial future, which shapes two
things: contributions need a licence grant before they can be merged, and a
few design constraints are not open to being traded away for convenience.
Everything else is ordinary.

## Before a patch: the licence

Veritix is dual licensed — AGPL-3.0-or-later, or commercial terms — as
described in [`LICENSING.md`](LICENSING.md). Contributions are accepted under
the Contributor Licence Agreement in [`CLA.md`](CLA.md). You keep your
copyright; you grant the project the right to ship your code under both
licences.

Signing is a `Signed-off-by` trailer on every commit:

```sh
git commit -s -m "Explain what the change makes possible"
```

Adding that trailer means you have read `CLA.md` and agree to it for the
commit it is on. There is nothing else to sign. If you are contributing on
behalf of an employer, name the employer once in the pull request.

If you would rather not sign, a bug report with a reproduction, or a failing
test that demonstrates the problem, is genuinely useful and asks nothing of
you.

## Working on it

Read [`CLAUDE.md`](CLAUDE.md) first. It is the orientation document for
picking this codebase up cold — what the packages are, why they are split that
way, and a list of gotchas that each cost somebody a debugging cycle. It is
not AI-specific; it is just where the working notes live.

```sh
make build test lint        # what has to pass
make web                    # the web interface, into web/dist
make release                # web, then build: the binary that ships an interface
make e2e                    # browser tests (see e2e/README.md)
make audit                  # dependency and vulnerability checks
go test -race ./...         # before committing: profile, ingest and api all fan out
```

`golangci-lint` is not installed by the `Makefile`; without it `make lint`
falls back to `go vet` alone and will miss what CI catches. Install it with
`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`.
Repo-wide lint is clean and should stay that way — fix the code rather than
widening `.golangci.yml`.

Commit messages say what the change makes possible, in the imperative, on one
line: "Serve audits over HTTP, with progress and an audit trail". The body, if
there is one, explains why. `git log` is the model.

Pull requests go to `main`. The maintainer commits to `main` directly — that
is not a standard anyone else has to meet, it is just what a project with one
reviewer looks like.

## Four things a patch must not do

These are load-bearing. A change that weakens one is not a trade-off to be
discussed in review; it is a change to what the product claims to be. The full
reasoning is in `CLAUDE.md`, and each is pinned by a test.

1. **Do not add a second way to get at raw cell values.** Reports describe
   columns by derived shapes, not contents. `GET
   /runs/{id}/findings/{fid}/rows` is the single deliberate exception, asked
   for one finding at a time. `TestDefaultReportContainsNoRawValues` and
   `TestReportOmitsRawValuesByDefault` pin it.

2. **Do not weaken `Set.Verify`.** Every finding carries a re-runnable
   `CountQuery`, and a finding that no longer reproduces is dropped. For the
   agent this is the whole mechanism: the model chooses what to look at, the
   engine decides what is true.

3. **Do not route around `internal/agent/redact`.** It is the only path from
   the process to a model, and it is enforced by types rather than by care. A
   new tool that returns raw values should fail to compile into a leak.

4. **Do not add a runtime dependency to the web interface casually.** It has
   three, it is served under a strict CSP, and the page can display customer
   data. [`docs/frontend-stack.md`](docs/frontend-stack.md) is the argument and
   the policy, including the release-age cooldown and the licence constraint —
   a copyleft dependency cannot be adopted here, because the commercial licence
   could not deliver it.

## Tests

`testdata/dirty-retail/` carries deliberately broken files with a defect
manifest in `internal/checks/checks_test.go`: 21 planted defects, each named
with the check that must catch it, plus a companion list of places the data is
clean and must stay quiet. **A new check adds to both lists.** A check that
fires on everything is useless, and only the second list catches that.

No test calls a real model. The agent's tests drive the real loop against a
scripted provider; the API's tests point the server at a local HTTP endpoint
speaking chat-completions. They are about what Veritix does with what a model
said.

## Reporting a security issue

Do not open a public issue. Write to russell.wallace@gmail.com with enough
detail to reproduce it. Anything that gets customer data out of the process —
past the report redaction, past the egress guard, or past the CSP — is the
category this project cares most about.
