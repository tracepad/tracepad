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

It exits `0` and names the workflow and the commit the archive was built from,
or exits non-zero and says why not. The container image is attested the same
way:

```sh
gh attestation verify oci://ghcr.io/tracepad/tracepad:0.1.0 --repo tracepad/tracepad
```

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

The server listens on `127.0.0.1:4318`: this machine only. It serves plain
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
upgrade that applies a migration leaves `tracepad.db.pre-<migration>.bak`
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
ExecStart=/usr/local/bin/tracepad serve --listen 127.0.0.1:4318
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
tracepad health --url http://127.0.0.1:4318
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
migration it writes `tracepad.db.pre-<migration>.bak` beside the database — a
full copy as it stood before, readable by its owner only, for rolling that
upgrade back by putting the file in place of `tracepad.db` with the server
stopped. The backups of earlier upgrades are removed once the new migrations have
committed, and the newest goes seven days after it was written; that week is the
window for a rollback, and the copy holds everything erased or swept since (see
[docker.md](docker.md#upgrading-and-backing-up-first) for removing it sooner).

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

Either file is the whole database — every prompt and completion your
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
