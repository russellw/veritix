# Veritix on Windows

Windows is the platform the web interface exists for. The users this product
is aimed at are business people on Windows desktops who do not use a command
line, so everything below is written twice: once for them, and once for
whoever installs it.

Veritix runs on 64-bit Windows 10 and 11, and on Windows Server 2019 and
later. It has no installer, no runtime to install alongside it, and no
service to register. It is one executable.

## Getting started

1. Download `veritix_<version>_windows_amd64.zip` and unzip it anywhere you
   can write — your Desktop or Documents folder is fine.
2. Double-click **Start Veritix**.
3. A black window opens and your browser opens on Veritix.

That black window *is* Veritix. Closing it stops the server; so does pressing
Ctrl-C in it. Nothing is left running afterwards.

If Windows shows a blue "Windows protected your PC" box, see
[SmartScreen](#smartscreen-and-antivirus) below.

To audit a folder of exports, use **Add dataset** and give it the path — for
example `D:\exports\monthly`. Veritix reads those files where they are; it
does not copy them anywhere. Uploading a folder through the browser works
too, and is the right choice for a one-off, but a dataset you want audited
[on a schedule](scheduling.md) has to be registered by path: an upload is a
copy of the data as it was, so a nightly audit of it would produce the same
report forever.

## Where things are

| | |
|---|---|
| The program | wherever you unzipped it; it writes nothing beside itself |
| Everything else | `%AppData%\veritix` — that is `C:\Users\<you>\AppData\Roaming\veritix` |

The data directory holds the run store (`veritix.db`, a SQLite file: the
record of every audit), any datasets uploaded through the browser, and the
DuckDB copy each run makes of the data it audited. That last one is the big
one — roughly a third of the size of the files audited — and
`server.retain_databases` is what stops a nightly audit filling the disk.
See [scheduling.md](scheduling.md).

Move it with `--data-dir` or `VERITIX_DATA_DIR`. On a server, put it on the
volume you actually have room on.

**On the file permissions.** On Linux the data directory is created 0700,
because it holds customer data on a machine that may have other users.
Windows has no such thing to set, and Go's file mode argument is ignored
there: the directory inherits the ACL of its parent. Under `%AppData%` that
is your user profile, which by default is not readable by other standard
users — but it is readable by an administrator, and if you move the data
directory somewhere else, it inherits whatever that place allows. If the
machine has other people on it, check the permissions on the directory you
chose.

## What does not happen

Veritix does not send your data anywhere. It is a program you run, not a
service you upload to — that is the whole proposition, and the platform does
not change it:

- It binds to `127.0.0.1` by default, which is your own machine and nothing
  else. Windows Defender Firewall does not prompt, because nothing is
  listening on the network.
- It has no automatic updates and phones nothing home.
- The LLM auditor is off unless somebody configures a provider, and even then
  the model is sent shapes and counts rather than cell values. Every byte sent
  is in the run's trace, in the interface.

If you *do* expose it to a network — `--addr 0.0.0.0:8080` — Veritix refuses
to start without an auth token, and Windows will raise a firewall prompt the
first time. Both of those are working as intended.

## Time zones and scheduling

A scheduled audit runs at a wall-clock time in a named zone: "02:00 daily,
Europe/London". Windows does not ship the IANA zone database that name comes
from — it has its own, under different names — so a Go program that asks the
operating system for `Europe/London` on Windows gets an error.

Veritix carries the zone database inside the binary for exactly this reason.
Nothing needs installing, the schedule you set in the browser means what it
says, and the two nights a year the clocks move are handled: a 03:00 daily
audit runs at 03:00 on the 23-hour night too, a time that does not exist
resolves forward to one that does, and a time that happens twice fires once.

The zone box in the interface defaults to your browser's zone, which is
almost always the one you mean.

## SmartScreen and antivirus

The releases are not code-signed. Windows treats an unsigned executable
downloaded from the internet accordingly, and there is no way around that
short of buying a certificate:

- **"Windows protected your PC"** — click **More info**, then **Run anyway**.
- **The zip's contents are blocked** — right-click the zip *before*
  unzipping, choose **Properties**, tick **Unblock**, then unzip. Windows
  marks downloaded archives and the mark is copied to every file inside.
- **Your antivirus is slow the first time** — the binary is about 90 MB and
  contains a compiled analytical database engine. A first-run scan of that is
  not fast. It is scanned once.

Check what you downloaded against the `.sha256` file published beside it:

```powershell
Get-FileHash veritix_<version>_windows_amd64.zip -Algorithm SHA256
```

## From a terminal

Everything the interface does, the CLI does, and on Windows it is the same
program with the same flags. In PowerShell:

```powershell
.\veritix.exe audit D:\exports\monthly
.\veritix.exe audit D:\exports\monthly --format html -o report.html
.\veritix.exe audit D:\exports\monthly --fail-on error   # exits 1
.\veritix.exe serve --open
```

`--fail-on` and `--fail-on-regression` set the exit code, so a Windows build
agent can gate on an audit exactly as a Linux one does. See
[comparison.md](comparison.md).

Use `-o <file>` rather than redirecting a report into one: Windows
PowerShell 5.1, which is the one already on the machine, writes UTF-16 when
you redirect with `>`. PowerShell 7 does not, which is worse — it means the
same command produces a different file depending on which PowerShell somebody
happened to open.

## What is not there

- **64-bit Intel and AMD only.** The DuckDB engine ships as a prebuilt
  library for `windows/amd64`, and there is no `windows/arm64` build of it.
  On an ARM Windows machine the x64 build runs under emulation, which is not
  tested here.
- **No installer, no service.** Veritix does not register itself to start
  with Windows. If you want a scheduled audit to run unattended, the machine
  has to have Veritix running — which today means a logged-in session with
  that window open, or wrapping it in a service manager yourself. The
  container image is the supported way to run it unattended, and that is
  Linux. See [deployment.md](deployment.md).
- **No code signing**, as above.

## For whoever maintains this

The Windows build is tested on every push: the `Build and test (Windows)` job
in `.github/workflows/ci.yml` builds the interface, runs the whole Go test
suite, builds the binary, and then scores both fixtures against their defect
manifests with `veritix eval` — which fails on a planted defect the checks
missed or a check firing on clean data. Running is a lower bar than agreeing,
and the manifests are what say it agrees: the same 14 errors, 14 warnings and
9 info as the Linux job.

That job runs without `make`, deliberately, since a Windows developer has no
make; and without `-race`, because a data race is not platform-specific and
is already caught where it costs less.

There is no cross-compilation. CGO means a Windows binary needs a Windows
toolchain, so the release archives are built on a Windows runner
(`.github/workflows/release.yml`), which is also what builds the interface
that goes into them.
