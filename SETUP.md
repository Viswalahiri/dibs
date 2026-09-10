# Setting up Dibs

Written for someone who has not used Go before. Every command below is meant to
be pasted as-is.

## What Go needs from you

Almost nothing. Go compiles the whole program, dependencies included, into one
file called `bin/dibs`. There is no virtualenv, no `node_modules`, no runtime to
install on the machine that runs it. You build the binary and you run the
binary.

Dependencies are declared in `go.mod` and pinned by checksum in `go.sum`. The
first build downloads them into `~/go/pkg/mod` and caches them there forever.
You never install them by hand.

## 1. Install Go

Check whether you already have it:

```bash
go version
```

If that prints something like `go version go1.26.7 linux/amd64`, skip ahead.
Dibs needs 1.22 or newer. Otherwise:

```bash
sudo snap install go --classic
```

## 2. Build

From the repository root:

```bash
make build
```

That writes `bin/dibs`. It takes about thirty seconds the first time, because Go
is downloading and compiling the SQLite driver, and a second or two after that.

Run `make test` any time you want to check that nothing is broken. It should
print `ok` for each package.

## 3. Create the config files

```bash
make setup
```

This creates three files under `~/.config/dibs/` and never overwrites one you
have already edited, so you can run it again safely.

| File | What it holds |
|---|---|
| `~/.config/dibs/env` | Your two tokens. Mode 0600. Never committed. |
| `~/.config/dibs/dibs.yaml` | Your login, timezone, and the poll intervals. |
| `~/.config/dibs/repos.yaml` | The repositories to watch. |

## 4. Paste your tokens

Open the env file:

```bash
$EDITOR ~/.config/dibs/env
```

Each line has a comment above it saying where to get the value. Paste to the
right of the `=` with no quotes and no spaces:

```
DIBS_GITHUB_TOKEN=github_pat_11ABCDEFG...
```

The GitHub token is a fine-grained personal access token from
`github.com/settings/personal-access-tokens/new`. Set repository access to
public repositories and grant nothing else. Dibs never writes to GitHub, so a
read-only token is not a precaution, it is the whole design.

Only `DIBS_GITHUB_TOKEN` is required to poll. The Slack bot token is checked
when you run without `--dry-run`.

The Slack app needs one scope, `chat:write`, and nothing else. Dibs only posts,
so leave interactivity switched off. There is no app-level token and no
inbound connection.

## 5. Set your GitHub login and your repositories

```bash
$EDITOR ~/.config/dibs/dibs.yaml     # profile.github_login
$EDITOR ~/.config/dibs/repos.yaml    # the repos to watch
```

`profile.github_login` must be the account the token belongs to. Dibs refuses to
start if the two disagree, because the filter uses your login to tell your own
comments from everyone else's, and getting that wrong would silently reject
issues you claimed yourself.

`repos.yaml` looks like this:

```yaml
repos:
  - slug: golang/go
  - slug: prometheus/prometheus
    notes: "maintainers reply within a day"
```

`notes` is for you. Dibs stores it and never reads it.

## 6. Run it

```bash
set -a; . ~/.config/dibs/env; set +a
./bin/dibs run
```

The first two commands load the tokens into your shell. `set -a` means "export
everything I define next", and the leading dot runs the file in the current
shell instead of a subshell, which is what makes the variables stick.

On the first run Dibs adopts each repository. It records the issues currently
open, draws a line under them, and surfaces none of them. That costs one request
per repository. Everything from then on is genuinely new.

Leave it running. `Ctrl-C` stops it.

To see where things stand:

```bash
./bin/dibs status --repos
```

## 7. Run it in the background, later

Once you trust it, install it as a user service so it survives logout and
reboot:

```bash
make service
```

That copies the binary to `~/.local/bin/dibs`, installs the systemd unit, starts
it, and enables lingering. Lingering is the part people forget. Without it your
user session is torn down at logout and Dibs dies with it.

```bash
make logs                          # follow the log
systemctl --user status dibs       # is it alive
systemctl --user restart dibs      # after editing dibs.yaml
kill -HUP $(pgrep -f 'dibs run')   # after editing repos.yaml only, no restart
```

## 8. Stop the laptop sleeping through the good issues

Dibs runs on your machine, so it stops when your machine does. Nothing here is
required and nothing in Dibs depends on it working. Missing an hour costs you
that hour's issues, which were going stale anyway, and Dibs reports the gap in
Slack when it happens.

Ubuntu suspends when the lid closes. In `/etc/systemd/logind.conf`:

```
HandleLidSwitch=ignore
HandleLidSwitchDocked=ignore
HandleLidSwitchExternalPower=ignore
```

Then `sudo systemctl restart systemd-logind`. Save your work first, because on
some Ubuntu versions that restarts the graphical session.

GNOME suspends on inactivity separately from the lid:

```bash
gsettings set org.gnome.settings-daemon.plugins.power sleep-inactive-ac-type 'nothing'
gsettings set org.gnome.settings-daemon.plugins.power sleep-inactive-battery-type 'suspend'
```

Leaving the battery case alone is deliberate. On battery the machine should
sleep.

If gap warnings show up in Slack several times a week anyway, the laptop is
costing you the thing you care most about, and it is time to move Dibs to a
small VPS. `make cross` builds the binary for that. Moving is an `scp` and a
`systemctl enable`, because the whole system is one static binary and one
SQLite file.

## Where everything lives

| Path | What |
|---|---|
| `~/.config/dibs/` | Config and secrets |
| `~/.local/share/dibs/dibs.db` | The database. This is the entire state. |
| `~/.local/bin/dibs` | The installed binary |
| `~/.local/state/dibs/` | Logs, when running under systemd |

To start completely over, delete `~/.local/share/dibs/dibs.db`. The next run
re-adopts every repository at a fresh waterline and surfaces none of what is
already open, so this is safe to do at any time.

## Upgrading from the scoring version

The schema changed and there is no migration. Stop the service, delete the
database, and start it again:

```bash
systemctl --user stop dibs
rm ~/.local/share/dibs/dibs.db*
make service
```

You lose the scored history, which nothing reads any more. You also want to
prune `dibs.yaml`, since the `scoring` and `triage` blocks are gone and Dibs
now refuses to start on an unknown field. `configs/dibs.example.yaml` is the
current shape. `DIBS_ANTHROPIC_KEY` and `DIBS_SLACK_APP_TOKEN` can come out of
your env file.

## When something goes wrong

**`missing required environment variables: ...`** means you started a new shell
and did not source the env file. Run the `set -a` line again.

**`token belongs to "x" but profile.github_login is "y"`** means `dibs.yaml`
disagrees with the token. Fix whichever one is wrong.

**`command not found: make`** means `sudo apt install make`.

**`no such file or directory: bin/dibs`** means you have not run `make build`.

**Nothing is printed for hours.** That is usually correct. Dibs only reports
issues opened in the last hour that it has not already seen and that nobody has
claimed, and on a quiet repository that is genuinely rare. Confirm it is working
with `./bin/dibs status --repos`, which shows the waterline and issue counts
per repository. Run with `--debug` to see every poll.
