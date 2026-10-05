# Installing the binary

The release archives are one file each and need nothing installed beside them.
This page is the binary's half of what [docker.md](docker.md) says for the
image: download, verify, run as a service, upgrade, back up. The rest —
[quickstart.md](quickstart.md) for the first trace, [retention.md](retention.md)
for how long data is kept — is the same either way.

## Download

Every release on [GitHub Releases](https://github.com/tracepad/tracepad/releases)
carries six archives and one checksum file:

| Platform | Archive |
|---|---|
| Linux, x86-64 | `tracepad_<version>_linux_amd64.tar.gz` |
| Linux, ARM64 | `tracepad_<version>_linux_arm64.tar.gz` |
| macOS, Intel | `tracepad_<version>_darwin_amd64.tar.gz` |
| macOS, Apple silicon | `tracepad_<version>_darwin_arm64.tar.gz` |
| Windows, x86-64 | `tracepad_<version>_windows_amd64.zip` |
| Windows, ARM64 | `tracepad_<version>_windows_arm64.zip` |
| all of them | `checksums.txt` |

`<version>` is the tag without its `v`: `0.1.0` for `v0.1.0`. An archive holds
`tracepad` (`tracepad.exe`), `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES` and the
licences under `third_party/`. The binary carries the web interface, the CLI,
the MCP server and the agent skill; there is nothing else to install.

With the [GitHub CLI](https://cli.github.com/), from a terminal:

```sh
gh release download v0.1.0 --repo tracepad/tracepad \
  --pattern 'tracepad_0.1.0_linux_amd64.tar.gz' --pattern checksums.txt
```

A release marked *pre-release* (`v0.1.0-rc.1`) is a candidate: it is published
under its own tag and moves nothing that says "latest".

**`go install` is not a way to get it.** It builds without the `ui` build tag,
which leaves a stub page where the web interface should be. Use an archive, or
`make build` in a checkout ([the README](../README.md#getting-it)).

## What a version is called, where

One release has one version and several names for it, because each registry has
its own rule for what a version may look like. For the candidate `0.1.0-rc.1` and
for the release `0.1.0` that follows it:

| Where | Name | Candidate | Release |
|---|---|---|---|
| Git tag, server | `v<version>` | `v0.1.0-rc.1` | `v0.1.0` |
| Git tags, packages | `sdk-py/v…`, `sdk-js/v…`, `sdk/go/v…` | `sdk-py/v0.1.0-rc.1` | `sdk-py/v0.1.0` |
| Release archives | `tracepad_<version>_<os>_<arch>` | `tracepad_0.1.0-rc.1_linux_amd64.tar.gz` | `tracepad_0.1.0_linux_amd64.tar.gz` |
| Container image | tag | `ghcr.io/tracepad/tracepad:0.1.0-rc.1` | `…:0.1.0`, and also `…:0.1` and `…:latest` |
| PyPI | PEP 440 | `0.1.0rc1` | `0.1.0` |
| npm | semver, under a dist-tag | `0.1.0-rc.1`, dist-tag `next` | `0.1.0`, dist-tag `latest` |
| Go module | tag of the nested module | `sdk/go/v0.1.0-rc.1` | `sdk/go/v0.1.0` |
| `tracepad version`, the first line of the server's log | semver, without the `v` | `0.1.0-rc.1` | `0.1.0` |

Python's spelling is the only one that differs from the tag's, and it is derived
from it (`-rc.N` is `rcN`, `-beta.N` is `bN`, `-alpha.N` is `aN`). A candidate
moves nothing that says "latest" — not the image's `latest` or `X.Y`, not npm's
`latest` dist-tag, not Homebrew, not Docker Hub — so the unqualified commands in
the README and the quickstart do not reach one. To install a candidate, name it:

```sh
docker run -d --name tracepad -v tracepad:/data -p 127.0.0.1:4318:4318 \
  ghcr.io/tracepad/tracepad:0.1.0-rc.1      # the exact tag; a candidate has no `latest`
pip install tracepad==0.1.0rc1              # without `==`, pip skips pre-releases
npm install tracepad@next
go get github.com/tracepad/tracepad/sdk/go@v0.1.0-rc.1
curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION=0.1.0-rc.1 sh   # the binary and the skill
```

**Until 0.1.0 is released, the unqualified commands do not install the
candidate.** `docker run … ghcr.io/tracepad/tracepad` with no tag asks for
`latest`, which does not exist yet and answers `manifest unknown`; `pip install
tracepad` finds only the placeholder `0.0.1`, which is not the SDK; and `npm
install tracepad` resolves `latest`, the same placeholder. They become the right
commands with the first stable release, and the README and the quickstart say
what they will be then.

## With the install script

On Linux and macOS, one line downloads the archive for this machine and checks
it against `checksums.txt`, and against its attestation too when `gh` is
installed and logged in. It then installs `tracepad` into `~/.local/bin`, with
the agent skill:

```sh
curl -fsSL https://tracepad.github.io/tracepad/install.sh | sh
```

```
installed tracepad 0.1.0 at /home/you/.local/bin/tracepad
  verified  sha256 matches checksums.txt; build attestation verified (gh)
  skill     installed 0.1.0 to /home/you/.claude/skills/tracepad
```

Running it again upgrades to the newest stable release, or says that version
is already installed. A release candidate is installed only when you name it
(`… | TRACEPAD_VERSION=0.1.0-rc.1 sh`), and until 0.1.0 is out the line above
stops and prints that form. A check that fails stops it, with nothing
installed. It never uses `sudo` and edits no shell profile. When
`~/.local/bin` is not on your `PATH` it says so, and prints the line to add.
The skill goes where an agent on this machine reads skills: `~/.claude/skills`
for Claude Code, and `~/.agents/skills` when `~/.agents` or `~/.codex` exists.
When neither exists, the skill is not installed.
[configuration.md](configuration.md#the-install-script) lists the variables it
reads. The script is [`scripts/install.sh`](../scripts/install.sh), short
enough to read before you pipe it into a shell.

## With Homebrew

On macOS:

```sh
brew install tracepad/tap/tracepad
brew upgrade tracepad        # later
```

The formula in [`tracepad/homebrew-tap`](https://github.com/tracepad/homebrew-tap)
installs the release archive for your platform — the same bytes as
[above](#download), with the checksum Homebrew verifies written in by the release
workflow. Homebrew downloads it without the macOS quarantine mark, so the
Gatekeeper step under [Put it on the path](#put-it-on-the-path) does not apply. Only a stable release
reaches the tap: a pre-release, and a back-patch of an older line, leave it
where it is. The formula also lists the two Linux archives, but nobody has
installed it on Linux yet, so that half is generated, not verified; on Linux,
use an archive. To check provenance, download the archive and use
`gh attestation verify` as below.

## Verify what you downloaded

Two checks, for two different questions.

**Is it the file the release published?** The checksum:

```sh
sha256sum --ignore-missing -c checksums.txt        # Linux
shasum -a 256 --ignore-missing -c checksums.txt    # macOS
```

```
tracepad_0.1.0_linux_amd64.tar.gz: OK
```

(On Windows, `Get-FileHash <archive>` prints the value to compare with the line
in `checksums.txt`.) A checksum fetched from the same place as the archive tells
you the download was not damaged, not that it is genuine — which is the second
question.

**Was it built from this repository, by its release workflow?** Every archive
carries a signed build-provenance attestation, which
[`gh attestation verify`](https://cli.github.com/manual/gh_attestation_verify)
checks against GitHub, offline of the release page:

```sh
gh attestation verify tracepad_0.1.0_linux_amd64.tar.gz --repo tracepad/tracepad
```

**The answer is the exit code.** At a terminal the command prints what it
checked; with no terminal — in a script, in CI, behind a pipe — a verification
that succeeds prints *nothing*, and that silence is the pass. A failure exits
non-zero and says why on stderr. In a script, test the status and ask for the
details explicitly when you want them logged:

```sh
gh attestation verify tracepad_0.1.0_linux_amd64.tar.gz --repo tracepad/tracepad \
  --format json --jq '.[0].verificationResult.signature.certificate
                      | {buildSignerURI, sourceRepositoryDigest}'
```

```
{"buildSignerURI":"https://github.com/tracepad/tracepad/.github/workflows/release-server.yml@refs/tags/v0.1.0","sourceRepositoryDigest":"…"}
```

The signer is the release workflow at the tag you meant, and the digest is the
full commit the archive was built from; its first seven characters are the ones
in brackets on the first line of the server's log. Under `set -e` the plain command is enough; `--format json`
only changes what it prints, never whether it passes. The container image is
attested the same way:

```sh
gh attestation verify oci://ghcr.io/tracepad/tracepad:0.1.0 --repo tracepad/tracepad
```

The copy on Docker Hub has the same digest, so it verifies the same way (see
[Docker Hub](docker.md#docker-hub)).

## Put it on the path

```sh
tar xzf tracepad_0.1.0_linux_amd64.tar.gz
sudo install -m 0755 tracepad /usr/local/bin/tracepad
tracepad version
```

The archives are not code-signed or notarized, so the two desktop systems ask
before they run a downloaded program:

- **macOS** marks a file a browser saved as quarantined, and Gatekeeper refuses
  it until the mark is gone: `xattr -d com.apple.quarantine tracepad`. A file
  fetched by `curl` or `gh release download` carries no mark. The checks above
  are what stands in for the signature.
- **Windows** SmartScreen says the app is unrecognized; *More info → Run anyway*
  runs it once and remembers.

## First run

```sh
tracepad
```

The server listens on `localhost:4318`: this machine only, on both loopback
addresses, `127.0.0.1` and `::1`, so a client reaches it whichever one it tries
first (on a host without IPv6 it says so and serves `127.0.0.1`). It serves plain
HTTP, so reaching it from anywhere else — `--listen :4318` for every interface,
or one address of this host — is a choice to make with a TLS proxy in front,
and until there is one it warns at start while other machines can reach it.
Better still, leave it on loopback and run the proxy on the same machine:
[docker.md](docker.md#serving-over-tls) has the two proxies, and the same
advice holds off Docker. An `https://` `TRACEPAD_URL` does not quiet the
warning, since a client that connects to the port directly skips the proxy.

The first run prints a key for your application — it can send and not read —
and the setup link, once; the [quickstart](quickstart.md#1-run-the-server)
explains both.

### Where the data lives

One directory, chosen by `--data-dir` or `TRACEPAD_DATA_DIR`. The default is
`$XDG_DATA_HOME/tracepad` when that is set and `~/.local/share/tracepad`
otherwise — on Windows too, so the default there is
`%USERPROFILE%\.local\share\tracepad`. The directory is created `0700`.

Everything the server knows is in `tracepad.db` and its `-wal` and `-shm`
companions: traces, scores, prompts, the raw OTLP archive, images, accounts and
keys (hashed). There is no second store to keep in step with it. Beside it is
`tracepad.db.lock`, which a running server holds so that a second one on the same
directory refuses to start, and an
upgrade that applies a migration leaves `tracepad.db.pre-<NNNN>_<name>.bak`
([below](#upgrading)).

## As a service

Any supervisor that restarts a process will do; `SIGTERM` (or Ctrl-C) is the
graceful stop, and the server needs up to ten seconds to drain and close its
database, so give a stop that long. On Linux with systemd:

```ini
# /etc/systemd/system/tracepad.service
[Unit]
Description=Tracepad
After=network.target

[Service]
# Listens on localhost:4318, the default: this machine only. To move it, set
# TRACEPAD_LISTEN below rather than passing --listen, so that `tracepad
# health` finds it too.
ExecStart=/usr/local/bin/tracepad serve
Restart=on-failure

# The data directory: /var/lib/tracepad, owned by a user that exists only
# while the service runs.
DynamicUser=yes
StateDirectory=tracepad
StateDirectoryMode=0700
Environment=TRACEPAD_DATA_DIR=/var/lib/tracepad

# The admin token and any other TRACEPAD_* variable: a file only root reads.
EnvironmentFile=-/etc/tracepad/tracepad.env

TimeoutStopSec=15
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes

[Install]
WantedBy=multi-user.target
```

```sh
sudo install -d -m 0755 /etc/tracepad
( umask 077; printf 'TRACEPAD_ADMIN_TOKEN=%s\n' "$(openssl rand -hex 32)" \
  | sudo tee /etc/tracepad/tracepad.env >/dev/null )
sudo systemctl daemon-reload
sudo systemctl enable --now tracepad
journalctl -u tracepad          # the first run's keys and setup link are here
tracepad health --url http://127.0.0.1:4318   # prints the version and exits 0
```

The keys stay in the journal for as long as the journal does — the same caveat
[docker.md](docker.md#the-keys-are-printed-once-to-the-log) makes for the
container's log, and the same advice: mint a key of its own for each
application and revoke the printed one. `TRACEPAD_ADMIN_TOKEN` is what lets you
do that from a terminal ([admin.md](admin.md#keys)). Behind a proxy, set
`TRACEPAD_URL=https://tracepad.example.com` in the same file and the setup link
is printed at the address your people use.

## Upgrading

Schema migrations run on start, forward only: a new binary opens the existing
data directory and brings it up to date. **A downgrade is not supported** — an
older binary meeting a newer schema is not a case anything handles — so the
upgrade is only as safe as the copy you took before it.

**A server you started yourself** — `tracepad serve` from the binary the
install script put in `~/.local/bin`, listening on this machine only — is
upgraded by one command, or by your coding agent with one line:

```sh
tracepad upgrade --plan      # changes nothing: what it will do, and what is yours
tracepad upgrade             # does it
```

```text
Update Tracepad to the latest release: follow https://tracepad.github.io/tracepad/agent-upgrade.md
```

It archives the data directory into `~/tracepad-backups/<run>/` with the server
stopped, puts the new binary in place, starts it with the same arguments and
environment, and checks it; a version that does not answer is rolled back at
once, and `tracepad upgrade --back <run>` takes the way back later. Nothing is
deleted on the way: what a way back replaces is set aside as
`<data>.after-<run>`. The whole of it is [cli.md](cli.md#upgrade).

**A backup is personal data with no expiry.** A run directory, like any copy
you take, holds every prompt and completion and the server's environment,
secrets included, until someone deletes it; erasing traces or a user's data
(see [retention.md](retention.md)) reaches neither it nor a data directory a
way back set aside. Remove them once the new version has proved itself.

A service, or a server open beyond this machine, is upgraded by hand — the
command's plan says which, and these are the steps:

1. Read the release notes, and [CHANGELOG.md](../CHANGELOG.md) for anything
   marked *Changed*.
2. [Back up](#backing-up).
3. Download and [verify](#verify-what-you-downloaded) the new archive, stop the
   service, replace the binary, start it:

    ```sh
    sudo systemctl stop tracepad
    sudo install -m 0755 tracepad /usr/local/bin/tracepad
    sudo systemctl start tracepad
    tracepad version
    ```

4. Watch `journalctl -u tracepad` until it says it is serving. A migration on a
   large database takes a while and logs its progress.

**The server keeps a copy of its own, too.** Before every start that applies a
migration it writes `tracepad.db.pre-<NNNN>_<name>.bak` beside the database — a
full copy as it stood before, readable by its owner only, for rolling that
upgrade back by putting the file in place of `tracepad.db` with the server
stopped. The backups of earlier upgrades are removed once the new migrations have
committed, and the newest goes seven days after it was written; that week is the
window for a rollback, and the copy holds everything erased or swept since (see
[docker.md](docker.md#upgrading-and-backing-up-first) for removing it sooner).
The server deletes only files named exactly `pre-<NNNN>_<name>.bak` — a
migration's number and name — so a backup you take yourself is safe from it
under any other name, and best kept outside the data directory.

## Backing up

The data is one SQLite database, so a backup is a copy of it. Two ways, and
both must include the `-wal` file's contents — which is why neither one is a
plain `cp` of `tracepad.db` from under a running server.

**Stopped.** The simplest, and complete by construction:

```sh
sudo systemctl stop tracepad
sudo tar czf tracepad-$(date +%F).tar.gz -C /var/lib/tracepad .
sudo systemctl start tracepad
```

**Running.** SQLite's own online backup takes a consistent copy while the server
carries on, with the `sqlite3` command-line shell (any recent version):

```sh
sudo sqlite3 /var/lib/tracepad/tracepad.db ".backup '/backups/tracepad-$(date +%F).db'"
sudo chmod 600 /backups/tracepad-*.db
```

Put either one outside the data directory, or name it anything but
`tracepad.db.pre-<NNNN>_<name>.bak`, the shape the server's own take and
delete. Either file is the whole database — every prompt and completion your
applications sent, and the password hashes of your accounts — so keep it as
private as the directory it came from.

**Restoring** is the reverse, with the server stopped: put the file back as
`tracepad.db`, delete `tracepad.db-wal` and `tracepad.db-shm` if they exist,
and start. A restored copy that is older than the binary is migrated forward on
start, like any other.

Backups and erasure interact: a copy taken before a data-subject erasure still
holds the erased data, and the erasure does not reach it —
[retention.md](retention.md#what-this-means-for-a-data-subject-request) says
where an erasure's reach ends.
