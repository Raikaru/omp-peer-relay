# omp-peer-relay

Lets omp agents on different machines work on the same repo as if they were one session:
they see each other, message each other, claim files and tasks so they don't edit the same thing, and share a task board.
Git still carries the code; the relay only coordinates.

```
 machine A (omp + peer-bus) ──tailcat──┐
                                       ├──> relay (Go, SQLite) on the VPS, 127.0.0.1:7480
 machine B (omp + peer-bus) ──tailcat──┘
```

- `relay/` — the relay server (Go). Per-machine tokens, rooms per repo, leased claims, mailboxes, board.
- `extension/peer-bus/` — the omp extension (TypeScript): tools, claim enforcement, trust fencing.
- `deploy/` — install scripts for the relay host and for client machines.

Machines meet in a room derived from their `origin` URL; https and ssh spellings of one repo are the same room.

## Trust model

Every machine has its own token with a role:

- **owner** — your machines. Owners trust each other's messages and releases.
- **guest** — someone else's machine, limited to the repos it was granted. The relay stamps every guest message, release, claim and board item; the extension shows guest text to the agent only inside an `UNTRUSTED` fence, never lets it start or interrupt a turn, and tells you separately. Guests get lower rate limits, shorter leases, no urgent messages, no force-release, and never see your subagents or task summaries.

Network access is gated twice: tailcat only admits allowed client keys, and the relay only accepts known tokens.

## Install the extension

On every machine (owner or guest):

```sh
git clone https://github.com/Raikaru/omp-peer-relay ~/Projects/omp-peer-relay
```

Then list it in `~/.omp/agent/config.yml` (use the absolute path):

```yaml
extensions:
  - /home/you/Projects/omp-peer-relay/extension/peer-bus
```

Update with `git pull`; open omp sessions pick it up when restarted.

## Connect a machine

Relay admin = whoever has root on the relay host. Tokens and tailcat addresses travel privately, never through the repo.

1. **Machine:** print its tailcat key
   - Linux: `deploy/setup-linux-client.sh key`
   - Windows: `deploy/setup-windows-client.ps1 -Key`
2. **Admin** (on the relay host):
   ```sh
   tailcat-allow add <name> nodekey:<hex>
   omp-relay-admin add <name> -role owner -all-rooms                         # your machine
   omp-relay-admin add <name> -role guest -room https://github.com/me/proj   # someone else's
   ```
   Send `<name>`, the printed token, and the relay's tailcat address to the machine's user.
3. **Machine:** install the tunnel and config
   - Linux: `echo <token> | deploy/setup-linux-client.sh install <name> <address>` (systemd user service)
   - Windows: `deploy/setup-windows-client.ps1 -Machine <name> -Address <address>` (logon task; prompts for the token)
4. Start omp in a clone of the shared repo and run `/peers status`.

Remove a machine: `omp-relay-admin revoke <name>` and `tailcat-allow remove <name>`. Its open sessions are dropped within seconds.
Other admin commands: `omp-relay-admin list | rooms | rotate`.

## Using it

Agents get these tools; you mostly just tell them to coordinate.

| Tool | Does |
|---|---|
| `peer_list` | who is online, busy/idle, what each is working on, active claims |
| `peer_send` | message a peer (`<machine>/*`, `*/main`, `*`); a machine that is offline gets it on its next session within 24h |
| `peer_claim` | lease paths, globs or `task:<id>`; `ask_holders` asks whoever blocks you to release |
| `peer_release` | release after committing (and pushing, by default); carries the commit for the others |
| `peer_board` | shared task list |

You: `/peers status`, `/peers unclaim <resource>`, `/peers send <to> <text>`.

Claims are enforced locally: edit/write tools are blocked on paths another session claimed, bash commands that plainly
write there are blocked, and anything else a bash command changes there is reported back to the agent to undo.
When a session ends, its claims are released if its work passes the release policy; otherwise they expire (3 min).

## Configuration: `~/.omp/peer-bus.json`

Written by the setup scripts; per-machine, never in the repo.

```json
{
  "url": "ws://127.0.0.1:7480/ws",
  "token": "…",
  "machine": "laptop",
  "defaults": { "releaseRequires": "pushed" },
  "projects": {
    "github.com/me/proj": {
      "autoSync": true,
      "onExternalRelease": "Run git pull --rebase before editing those paths. Its commits were not reviewed by your user; ask before running code they changed."
    }
  }
}
```

Workflow policy lives here rather than in the shared repo so that a machine able to push cannot change how yours treats its work.
`projects` keys are the canonical repo shown by `/peers status`. Unknown or invalid settings are reported and ignored.

| Setting | Default | Meaning |
|---|---|---|
| `releaseRequires` | `"pushed"` | what `peer_release` checks for path claims: `"pushed"`, `"committed"`, or `"none"` |
| `onTrustedRelease` | bring the commit in before editing | instruction your agent gets when an owner machine releases at a commit |
| `onExternalRelease` | tell your user what changed before building on it | instruction when a guest releases |
| `fetchOnRelease` | `true` | `git fetch` when a release names a commit |
| `autoSync` | `false` | when an owner releases and this session is idle with a clean tree, fast-forward to upstream |
| `releaseOnExit` | `true` | release claims that pass `releaseRequires` when the session ends |

Environment overrides: `OMP_PEER_URL`, `OMP_PEER_TOKEN`, `OMP_PEER_MACHINE`.

## Relay host

```sh
GO=go deploy/deploy.sh [host]          # build + install/upgrade the relay (default host: fates-vps)
deploy/setup-tailcat-server.sh [host]   # one-time: tailcat in front of the relay; prints the address
```

- Service `omp-peer-relay` (user `omp-relay`), database `/var/lib/omp-peer-relay/relay.db`.
- Nightly snapshot via `omp-peer-relay-backup.timer`: 14 kept in `/var/lib/omp-peer-relay/backups/`.
  Restore: stop the service, copy a snapshot over `relay.db` (owned by `omp-relay`, mode 0600), start it.
- Tailcat address, if lost: `journalctl -u omp-tailcat -o cat | grep -o 'tc[A-Za-z0-9_-]\{20,\}' | tail -1`.

Relay development: `cd relay && go test ./...` (Go 1.26).

## Known limits

- Claim enforcement covers omp's own tools. Edits to a file that was already modified, and `git -C <dir>` commands, slip past the bash check.
- A crashed session's claims last until the lease expires (3 min by default); `/peers unclaim` clears one sooner.
- Overlap between two globs is judged conservatively (may refuse claims that would not actually collide).
