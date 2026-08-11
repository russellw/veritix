# Licensing

Veritix is dual licensed. You may use it under **either**:

- the **GNU Affero General Public License, version 3 or later** (AGPL-3.0-or-later),
  the full text of which is in [`LICENSE`](LICENSE); **or**
- a **commercial license** from the copyright holder, on terms that do not
  require you to publish your own source code.

SPDX expression: `AGPL-3.0-or-later OR LicenseRef-Veritix-Commercial`.

Everything in this repository is offered under both, unless a file says
otherwise in its own header. Nothing here is a dual-license trick where the
open version is crippled: the AGPL build is the whole product. The commercial
license sells different *terms*, not different code.

## Which one applies to you

You are already covered by the AGPL, at no cost, and you do not have to tell
anyone. It only starts to matter when you do one of the things the AGPL asks
something in return for.

**The AGPL is very likely fine if you** run Veritix on your own data, inside
your own organization — on a laptop, on a build server, in your own cloud —
whether or not you have modified it. That is the case Veritix was built for,
and it is the case the license leaves alone.

**You probably want a commercial license if you**

- ship Veritix, or code derived from it, inside a product you distribute under
  terms of your own;
- run a modified Veritix as a service that people outside your organization
  interact with over a network, and do not want to publish your modifications;
- link Veritix's code into a larger system whose source you cannot release;
- need the things a public license cannot give: a warranty, an indemnity, a
  support commitment, or a signed document your procurement department will
  accept.

To ask about one, write to **russell.wallace@gmail.com**.

## What the AGPL actually requires

This section is a plain-language summary for orientation. It is not the
license and it does not modify it — [`LICENSE`](LICENSE) governs.

- **Using it costs nothing and obliges nothing.** Running the program, however
  you like, on whatever data you like, is not a licensed act with conditions
  attached.
- **If you distribute it** — the binary, a fork, or a product containing either
  — the people you give it to get the source, and get it under the AGPL too.
- **Section 13 is the one people miss.** If you *modify* Veritix and let users
  interact with it *remotely over a network*, those users must be offered the
  source of your modified version, even though you never handed them a copy of
  anything. Veritix ships a web interface and an HTTP API, so this is a
  realistic trigger rather than a theoretical one. Note the two conditions
  together: an unmodified Veritix served internally does not put you here.
- **Your data and your reports are yours.** The AGPL is about the program's
  source, not its output. A report Veritix writes is not a derivative work of
  Veritix, and neither is the dataset you pointed it at.
- **Your rules files and configuration are yours.** A `veritix-rules.yaml` is
  input to the program, like a spreadsheet is input to a spreadsheet
  application.

If you are unsure which side of a line you are on, ask. Getting a commercial
license is usually quicker than getting an opinion on whether you need one.

## If you run a modified Veritix for other people

Section 13 is easier to comply with than to notice, so the program helps. Every
screen of the web interface carries a footer with the version and a **Source**
link, and `veritix version` prints the same offer on the command line.

By default that link points at this repository, which is the correct answer for
an unmodified build and the wrong one for yours: your users are owed *your*
source. Point it at where you publish it —

```yaml
# veritix.yaml
server:
  source_url: https://git.example.com/ops/veritix
```

or `VERITIX_SOURCE_URL=https://git.example.com/ops/veritix`, no rebuild
required. A fork that relinks anyway can set the default at build time with
`-ldflags "-X github.com/russellw/veritix/internal/buildinfo.SourceURL=…"`.

Setting `server.source_url` to the empty string removes the link. That is
there for builds shipped under the commercial license, where there is no such
offer to make; under the AGPL, removing the offer does not remove the
obligation.

## Third-party components

A commercial license covers Veritix's own code. It cannot re-license code
Veritix depends on, and does not try to.

Every third-party component Veritix links or embeds is under a permissive
license — MIT, BSD-3-Clause, or Apache-2.0 — which is deliberate: a copyleft
dependency would be a term the commercial license could not deliver. The
notable ones are DuckDB and its Go driver, `modernc.org/sqlite`, excelize,
cobra, the Anthropic SDK, and React with `react-dom` in the web interface.
Their terms travel with the binary and are unaffected by which Veritix license
you hold.

Keeping it that way is a constraint on new dependencies, alongside the
supply-chain rules in [`docs/frontend-stack.md`](docs/frontend-stack.md): a
GPL, AGPL, or SSPL-licensed dependency is not adoptable here at any technical
merit.

## Contributing

Contributions are accepted under the Contributor License Agreement in
[`CLA.md`](CLA.md), which is what makes the dual license possible: the
copyright holder can only offer commercial terms for code it has the right to
offer them for. You keep the copyright in what you write.
[`CONTRIBUTING.md`](CONTRIBUTING.md) has the mechanics.
