# alistbatch — Go client to pack & upload to Alist REST API

Upload a single file or a whole folder to [Alist](https://alist.nn.ci) via its REST API (`PUT /api/fs/put`). Folders are zipped automatically (or uploaded recursively with `--no-zip`). Credentials are saved locally and token is auto-refreshed on expiry.

## Install

```bash
go build -o alistbatch .
# or
go install ./...
```

Requires Go 1.22+ (`toolchain go1.22.0`).

## Quick start

```bash
# 1. login once — saves host/user/password/token to local config
./alistbatch login -H https://alist.example.com -u admin -p 123456
# saved to ~/Library/Application Support/alistbatch/config.json (0600)
# or $XDG_CONFIG_HOME/alistbatch/config.json on Linux

# 2. subsequent commands use saved config — no flags needed
./alistbatch ls /photos
./alistbatch upload -s ./photo.jpg -r /photos/photo.jpg
./alistbatch upload -s ./mydir -r /backup/mydir.zip
./alistbatch upload -s ./mydir -r /backup/mydir --no-zip

# 3. pack only (no upload)
./alistbatch pack ./mydir -o mydir.zip
```

## Auto-login & token refresh

- `login` saves `host`, `username`, `password`, `token` to config file (0600, via `internal/config`).
- Every command resolves credentials as **flags > env vars > config file**.
- If `token` is missing, the client auto-logs in with `username`/`password` and saves the new token.
- If any API call returns **401** (token expired), the client auto re-logs in, updates the config file via `OnTokenRefresh`, and **retries once** — transparent to the caller. Works for `ls`, `mkdir`, `upload` (including `PUT /api/fs/put` which reopens the file on retry; `put --zip` re-generates the zip stream on retry).

Override config path with `$ALIST_CONFIG`:

```bash
ALIST_CONFIG=/tmp/my.json ./alistbatch login -H https://example.com -u admin -p 123
```

## Commands

| Command | Description |
|---------|-------------|
| `upload` | Upload file or folder (single file or `--no-zip` batch) |
| `put` | Stream upload via `PUT /api/fs/put` (file, stdin, or folder) |
| `pack`   | Zip a folder/file locally (`-o` / `--output`) |
| `login`  | `POST /api/auth/login` → save to config |
| `logout` | Clear token (`--all` clears everything) |
| `config` | Show / set config (`--json`, `--path`, `--clear`) |
| `mkdir`  | `POST /api/fs/mkdir` (with `-p` behavior) |
| `ls`/`list`/`dir` | `POST /api/fs/list` (supports `-R`, `--json`, `-l`) |
| `help`   | Show help |

## Flags

```
-H, --host       Alist host, e.g. https://alist.example.com (or $ALIST_HOST)
-u, --username   Username (or $ALIST_USERNAME)
-p, --password   Password (or $ALIST_PASSWORD)
-t, --token      Token, skips login (or $ALIST_TOKEN)
-s, --src        Local file/folder path (or 1st positional arg)
-r, --remote     Remote Alist path, e.g. /a/b.zip (or 2nd positional arg)
    --no-zip     Upload folder recursively instead of zipping
    --zip        (put only) Zip folder and stream as single zip
    --as-task    Set header As-Task:true (for large files)
    --save       Save credentials after login (default true, use --save=false to disable)
-c, --concurrency  Concurrent uploads for folder --no-zip (default 1 for upload, 4 for put, capped at 32)
    --skip-errors    Skip failed files and continue (batch upload, exit 2 if any skipped)
    --skip-existing  Skip files that already exist on remote (checked via List, batch + single file)
                     Aliases: --skip-exists, --exists-skip, --continue-on-error (for skip-errors)
```

`ls` flags: `-l` (long), `-R` (recursive), `--json`, `-f` (refresh), `--page`, `--per-page`.

`pack` flags: `-o` / `--output` output zip path.

## Examples

```bash
# one-off with explicit token (no save)
./alistbatch upload -H https://alist.example.com -t $TOKEN -s ./a.txt -r /a.txt --save=false

# folder with spaces
./alistbatch upload -s "./My Photos" -r "/backup/My Photos.zip"

# large file as background task
./alistbatch upload -s ./big.iso -r /iso/big.iso --as-task

# batch upload: skip existing + skip errors, concurrent
./alistbatch upload -s ./mydir -r /backup/mydir --no-zip -c 8 --skip-existing --skip-errors
./alistbatch put -r /remote/dir ./localdir --no-zip -c 8 --skip-existing --skip-errors

# single file: skip if exists
./alistbatch upload -s ./a.txt -r /a.txt --skip-existing
./alistbatch put -r /remote/a.txt ./a.txt --skip-existing

# stdin streaming
./alistbatch put -r /remote/file.txt --stdin < file.txt
cat file.txt | ./alistbatch put -r /remote/file.txt --stdin
echo "hello" | ./alistbatch put -r /remote/hello.txt --stdin --content-type text/plain

# folder as zip stream (no temp file)
./alistbatch put -r /remote/dir.zip ./localdir --zip

# list
./alistbatch ls /photos --long
./alistbatch ls / --recursive --json | jq .

# config
./alistbatch config              # show (password/token masked)
./alistbatch config --json       # JSON (masked)
./alistbatch config --path       # print path
./alistbatch config --clear      # delete file
./alistbatch logout              # clear token only
./alistbatch logout --all        # clear all
```

Exit codes: `0` success, `1` fatal error, `2` batch completed with skipped failures (`--skip-errors`).

## Alist API used

- `POST /api/auth/login` — login
- `PUT  /api/fs/put` — stream upload (headers: `Authorization`, `File-Path`, `As-Task`)
- `POST /api/fs/mkdir` — create remote dirs
- `POST /api/fs/list` — list / check existence (used for `--skip-existing` and `EnsureDir`)

`File-Path` is URL-encoded per-segment, `Authorization` is the raw token (no `Bearer` prefix) per Alist docs.

## Project layout

```
.
├── main.go                 # CLI (upload/put/pack/login/logout/config/mkdir/ls)
├── internal/
│   ├── alist/client.go     # Alist REST client (auto re-login on 401, batch upload, exists check)
│   ├── config/config.go    # Local config (host/user/pass/token, 0600, atomic write)
│   └── pack/zip.go         # Zip folder/file (context-aware, streaming via io.Pipe)
└── go.mod
```
