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
| `~/.config/dibs/env` | Your four tokens. Mode 0600. Never committed. |
| `~/.config/dibs/dibs.yaml` | Tuning: score floor, poll interval, model, budgets. |
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

Only `DIBS_GITHUB_TOKEN` is required to poll. The other three are checked when
the stages that need them are running.

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
    receptivity: normal
  - slug: prometheus/prometheus
    receptivity: high
    stacks: [go]
    notes: "maintainers reply within a day"
```

`receptivity` is your judgment about how open the maintainers are to outside
contributions. It is `high`, `normal`, or `cautious`, and it multiplies the
score.

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
per repository and no model calls at all. Everything from then on is genuinely
new.

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

## Where everything lives

| Path | What |
|---|---|
| `~/.config/dibs/` | Config and secrets |
| `~/.local/share/dibs/dibs.db` | The database. This is the entire state. |
| `~/.local/bin/dibs` | The installed binary |
| `~/.local/state/dibs/` | Logs, when running under systemd |

To start completely over, delete `~/.local/share/dibs/dibs.db`. The next run
re-adopts every repository at a fresh waterline.

## When something goes wrong

**`missing required environment variables: ...`** means you started a new shell
and did not source the env file. Run the `set -a` line again.

**`token belongs to "x" but profile.github_login is "y"`** means `dibs.yaml`
disagrees with the token. Fix whichever one is wrong.

**`command not found: make`** means `sudo apt install make`.

**`no such file or directory: bin/dibs`** means you have not run `make build`.

**Nothing is printed for hours.** That is usually correct. Dibs only reports
issues opened in the last fifteen minutes that it has not already seen, and on a
quiet repository that is genuinely rare. Confirm it is working with
`./bin/dibs status --repos`, which shows the waterline and issue counts per
repository. Run with `--debug` to see every poll.
