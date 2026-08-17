package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"alistbatch/internal/alist"
	"alistbatch/internal/config"
	"alistbatch/internal/pack"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "upload":
		runUpload(args)
	case "put":
		runPut(args)
	case "pack":
		runPack(args)
	case "login":
		runLogin(args)
	case "logout":
		runLogout(args)
	case "config":
		runConfig(args)
	case "mkdir":
		runMkdir(args)
	case "ls", "list", "dir":
		runList(args)
	case "help", "--help", "-h":
		printUsage()
	default:
		if strings.HasPrefix(cmd, "-") {
			runUpload(os.Args[1:])
		} else {
			fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
			printUsage()
			os.Exit(1)
		}
	}
}

func printUsage() {
	fmt.Printf(`alistbatch v%s - Alist folder/file uploader

Usage:
  alistbatch <command> [options]

Commands:
  upload    Upload file or folder to Alist (folders are zipped by default)
  put       Stream upload to PUT /api/fs/put (file or stdin)
  pack      Pack a folder into zip without uploading
  login     Login and save credentials to local config
  logout    Clear saved token (or --all to clear everything)
  config    Show / set local config
  mkdir     Create remote directory
  ls/list   List remote folder (supports -R, --json, --long)

Config:
  Credentials are resolved in priority: flags > env vars > local config file.
  Config file: %s (override with $ALIST_CONFIG)
  Env vars: ALIST_HOST, ALIST_TOKEN, ALIST_USERNAME, ALIST_PASSWORD
  After login, host/user/token are saved to config (password saved with 0600).
  Subsequent commands auto-login using saved credentials if token is missing/expired.

Upload examples:
  # first time: login and save
  alistbatch login -H https://alist.example.com -u admin -p 123456

  # then upload without flags (uses saved config)
  alistbatch upload -s ./photo.jpg -r /photos/photo.jpg
  alistbatch upload -s ./mydir -r /backup/mydir.zip
  alistbatch upload -s ./mydir -r /backup/mydir --no-zip

  # concurrent upload (folder --no-zip only)
  alistbatch upload -s ./mydir -r /backup/mydir --no-zip --concurrency 8
  alistbatch upload -s ./mydir -r /backup/mydir --no-zip -c 8
  alistbatch upload -s ./mydir -r /backup/mydir --no-zip -c 8 --skip-errors

  # one-off with explicit host/token (no save)
  alistbatch upload -H https://alist.example.com -t $TOKEN -s ./a.txt -r /a.txt --save=false

Put examples (streaming PUT /api/fs/put):
  alistbatch put -r /remote/file.txt ./local.txt
  alistbatch put -r /remote/file.txt --stdin < file.txt
  cat file.txt | alistbatch put -r /remote/file.txt --stdin
  echo "hello" | alistbatch put -r /remote/hello.txt --stdin --content-type text/plain
  alistbatch put -r /remote/big.bin ./big.bin --as-task
  alistbatch put -r /remote/dir ./localdir --no-zip -c 8   # folder concurrent
  alistbatch put -r /remote/dir ./localdir --no-zip -c 8 --skip-errors
  alistbatch put -r /remote/dir.zip ./localdir --zip       # folder as zip stream

Pack examples:
  alistbatch pack ./mydir -o mydir.zip
  alistbatch pack ./mydir              # -> ./mydir.zip

List examples:
  alistbatch ls /photos
  alistbatch ls /photos --long --json
  alistbatch ls / --recursive --per-page 200

Flags (upload/login/mkdir/ls):
  -H, --host      Alist host (e.g. https://alist.example.com)
  -u, --username  Username
  -p, --password  Password
  -t, --token     Token (skip login if provided)
  -c, --concurrency  Concurrent uploads for folder --no-zip (default 1)
      --skip-errors  Skip failed files and continue (batch upload, exit 2 if any skipped)
`, version, config.Path())
}

// ---------- auth helpers ----------

type resolvedAuth struct {
	Host     string
	Token    string
	Username string
	Password string
	Cfg      *config.Config
}

func resolveAuth(hostFlag, tokenFlag, userFlag, passFlag string) (*resolvedAuth, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	host := firstNonEmpty(hostFlag, os.Getenv("ALIST_HOST"), cfg.Host)
	token := firstNonEmpty(tokenFlag, os.Getenv("ALIST_TOKEN"), cfg.Token)
	username := firstNonEmpty(userFlag, os.Getenv("ALIST_USERNAME"), cfg.Username)
	password := firstNonEmpty(passFlag, os.Getenv("ALIST_PASSWORD"), cfg.Password)
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	return &resolvedAuth{
		Host:     host,
		Token:    token,
		Username: username,
		Password: password,
		Cfg:      cfg,
	}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func ensureClient(a *resolvedAuth, save bool) (*alist.Client, error) {
	if a.Host == "" {
		return nil, fmt.Errorf("Alist host required (-H or ALIST_HOST or saved config). Run: alistbatch login -H <host> -u <user> -p <pass>")
	}
	c := alist.NewClient(a.Host, a.Token)
	c.SetCredentials(a.Username, a.Password)
	c.OnTokenRefresh = func(newToken string) {
		_ = config.Update(a.Host, "", "", newToken)
		fmt.Fprintf(os.Stderr, "token refreshed and saved to %s\n", config.Path())
	}
	if c.Token != "" {
		return c, nil
	}
	if a.Username == "" || a.Password == "" {
		return nil, fmt.Errorf("token or username/password required (no saved credentials). Run: alistbatch login -H %s -u <user> -p <pass>", a.Host)
	}
	fmt.Fprintf(os.Stderr, "logging in to %s as %s ...\n", a.Host, a.Username)
	if err := c.Login(a.Username, a.Password); err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "login ok")
	if save {
		if err := config.Update(a.Host, a.Username, a.Password, c.Token); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to save config: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "saved credentials to %s\n", config.Path())
		}
	}
	return c, nil
}

// ---------- upload ----------

func runUpload(args []string) {
	args = normalizeArgs(args, map[string]string{
		"--host":             "-H",
		"--username":         "-u",
		"--password":         "-p",
		"--token":            "-t",
		"--src":              "-s",
		"--remote":           "-r",
		"--concurrency":      "-c",
		"--skip-errors":      "-skip-errors",
		"--continue-on-error": "-skip-errors",
	})
	fs := flag.NewFlagSet("upload", flag.ExitOnError)
	hostF := fs.String("H", "", "Alist host")
	usernameF := fs.String("u", "", "username")
	passwordF := fs.String("p", "", "password")
	tokenF := fs.String("t", "", "token")
	src := fs.String("s", "", "local file/folder path")
	remote := fs.String("r", "", "remote path on Alist (e.g. /a/b.zip)")
	noZip := fs.Bool("no-zip", false, "do not zip folder, upload recursively")
	asTask := fs.Bool("as-task", false, "upload as Alist task (for large files)")
	overwrite := fs.Bool("overwrite", true, "overwrite if exists (Alist default)")
	save := fs.Bool("save", true, "save credentials to local config after login (use --save=false to disable)")
	concurrency := fs.Int("c", 1, "concurrent uploads for folder --no-zip (e.g. -c 8)")
	skipErrors := fs.Bool("skip-errors", false, "skip failed files and continue (batch upload only)")

	fs.Parse(args)
	rest := fs.Args()
	if *src == "" && len(rest) >= 1 {
		*src = rest[0]
		rest = rest[1:]
	}
	if *remote == "" && len(rest) >= 1 {
		*remote = rest[0]
	}

	if *src == "" {
		fmt.Fprintln(os.Stderr, "error: -s <local path> is required")
		fs.Usage()
		os.Exit(1)
	}

	fi, err := os.Stat(*src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stat %s: %v\n", *src, err)
		os.Exit(1)
	}

	if *remote == "" {
		base := filepath.Base(*src)
		if fi.IsDir() && !*noZip {
			if !strings.HasSuffix(strings.ToLower(base), ".zip") {
				base += ".zip"
			}
		}
		*remote = "/" + base
	}
	if !strings.HasPrefix(*remote, "/") {
		*remote = "/" + *remote
	}

	auth, err := resolveAuth(*hostF, *tokenF, *usernameF, *passwordF)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve auth: %v\n", err)
		os.Exit(1)
	}
	client, err := ensureClient(auth, *save)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth failed: %v\n", err)
		os.Exit(1)
	}

	if !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "uploading file %s -> %s\n", *src, *remote)
		if err := client.UploadFile(*src, *remote, *asTask, *overwrite); err != nil {
			fmt.Fprintf(os.Stderr, "upload failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "done")
		return
	}

	if *noZip {
		if *concurrency < 1 {
			*concurrency = 1
		}
		if *concurrency > 32 {
			fmt.Fprintln(os.Stderr, "warning: concurrency capped at 32")
			*concurrency = 32
		}
		dirOpts := alist.UploadDirOptions{AsTask: *asTask, Concurrency: *concurrency, SkipErrors: *skipErrors}
		if *concurrency > 1 {
			fmt.Fprintf(os.Stderr, "uploading folder recursively %s -> %s (concurrency=%d skip-errors=%v)\n", *src, *remote, *concurrency, *skipErrors)
			count, bytes, failed, err := client.UploadDirConcurrentWithOptions(nil, *src, *remote, dirOpts)
			if err != nil {
				fmt.Fprintf(os.Stderr, "upload dir failed: %v\n", err)
				os.Exit(1)
			}
			if len(failed) > 0 {
				fmt.Fprintf(os.Stderr, "done: %d files, %s, %d failed (skipped):\n", count, humanBytes(bytes), len(failed))
				for _, fe := range failed {
					fmt.Fprintf(os.Stderr, "  skip %s: %v\n", fe.Rel, fe.Err)
				}
				os.Exit(2)
			}
			fmt.Fprintf(os.Stderr, "done: %d files, %s\n", count, humanBytes(bytes))
			return
		}
		fmt.Fprintf(os.Stderr, "uploading folder recursively %s -> %s (skip-errors=%v)\n", *src, *remote, *skipErrors)
		count, bytes, failed, err := client.UploadDirConcurrentWithOptions(nil, *src, *remote, dirOpts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "upload dir failed: %v\n", err)
			os.Exit(1)
		}
		if len(failed) > 0 {
			fmt.Fprintf(os.Stderr, "done: %d files, %s, %d failed (skipped):\n", count, humanBytes(bytes), len(failed))
			for _, fe := range failed {
				fmt.Fprintf(os.Stderr, "  skip %s: %v\n", fe.Rel, fe.Err)
			}
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "done: %d files, %s\n", count, humanBytes(bytes))
		return
	}

	fmt.Fprintf(os.Stderr, "packing folder %s ...\n", *src)
	tmpZip, cleanup, err := pack.ZipFolderToTemp(*src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pack failed: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()
	if strings.HasSuffix(*remote, "/") {
		*remote = *remote + filepath.Base(*src) + ".zip"
	}
	if !strings.Contains(filepath.Base(*remote), ".") {
		*remote += ".zip"
	}
	fi2, _ := os.Stat(tmpZip)
	fmt.Fprintf(os.Stderr, "packed %s (%s) -> uploading to %s\n", tmpZip, humanBytes(fi2.Size()), *remote)
	if err := client.UploadFile(tmpZip, *remote, *asTask, *overwrite); err != nil {
		fmt.Fprintf(os.Stderr, "upload failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "done")
}

func runPut(args []string) {
	args = normalizeArgs(args, map[string]string{
		"--host":              "-H",
		"--username":          "-u",
		"--password":          "-p",
		"--token":             "-t",
		"--remote":            "-r",
		"--stdin":             "-stdin",
		"--content-type":      "-content-type",
		"--as-task":           "-as-task",
		"--concurrency":       "-c",
		"--zip":               "-zip",
		"--skip-errors":       "-skip-errors",
		"--continue-on-error": "-skip-errors",
	})
	fs := flag.NewFlagSet("put", flag.ExitOnError)
	hostF := fs.String("H", "", "Alist host")
	usernameF := fs.String("u", "", "username")
	passwordF := fs.String("p", "", "password")
	tokenF := fs.String("t", "", "token")
	remoteF := fs.String("r", "", "remote path (required, e.g. /a/b.txt)")
	useStdin := fs.Bool("stdin", false, "read from stdin instead of file")
	contentType := fs.String("content-type", "application/octet-stream", "Content-Type header")
	asTask := fs.Bool("as-task", false, "upload as Alist task")
	save := fs.Bool("save", true, "save credentials after login")
	concurrency := fs.Int("c", 4, "concurrent uploads for folder (default 4)")
	useZip := fs.Bool("zip", false, "zip folder before streaming (put folder as single zip)")
	noZip := fs.Bool("no-zip", false, "upload folder file-by-file (default for put folder)")
	skipErrors := fs.Bool("skip-errors", false, "skip failed files and continue (folder upload only)")
	fs.Parse(args)
	rest := fs.Args()

	if *remoteF == "" {
		if len(rest) >= 1 && *useStdin {
			*remoteF = rest[0]
		} else if len(rest) >= 2 {
			*remoteF = rest[1]
		} else if len(rest) == 1 && !*useStdin {
			if strings.HasPrefix(rest[0], "/") {
				*remoteF = rest[0]
				*useStdin = true
			}
		}
	}
	if *remoteF == "" {
		fmt.Fprintln(os.Stderr, "error: -r <remote path> is required")
		fmt.Fprintln(os.Stderr, "usage: alistbatch put -r /remote/file.txt [local-file]")
		fmt.Fprintln(os.Stderr, "       alistbatch put -r /remote/file.txt --stdin < file")
		fmt.Fprintln(os.Stderr, "       cat file | alistbatch put -r /remote/file.txt --stdin")
		fmt.Fprintln(os.Stderr, "       alistbatch put -r /remote/dir ./localdir --no-zip -c 8")
		fmt.Fprintln(os.Stderr, "       alistbatch put -r /remote/dir.zip ./localdir --zip")
		fs.Usage()
		os.Exit(1)
	}
	if !strings.HasPrefix(*remoteF, "/") {
		*remoteF = "/" + *remoteF
	}

	auth, err := resolveAuth(*hostF, *tokenF, *usernameF, *passwordF)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve auth: %v\n", err)
		os.Exit(1)
	}
	client, err := ensureClient(auth, *save)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth failed: %v\n", err)
		os.Exit(1)
	}

	opts := alist.PutStreamOptions{
		AsTask:      *asTask,
		ContentType: *contentType,
	}

	if *useStdin {
		fmt.Fprintf(os.Stderr, "streaming stdin -> %s\n", *remoteF)
		if err := client.PutStream(nil, os.Stdin, -1, *remoteF, opts); err != nil {
			fmt.Fprintf(os.Stderr, "put failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "done")
		return
	}

	local := ""
	if len(rest) >= 1 {
		local = rest[0]
		if len(rest) >= 2 && rest[1] == *remoteF {
			local = rest[0]
		}
	}
	if local == "" {
		fmt.Fprintln(os.Stderr, "error: local file required (or use --stdin)")
		fmt.Fprintln(os.Stderr, "usage: alistbatch put -r /remote/file.txt <local-file>")
		fmt.Fprintln(os.Stderr, "       alistbatch put <local-file> <remote-path>")
		fmt.Fprintln(os.Stderr, "       alistbatch put -r /remote/dir ./localdir [--zip|--no-zip] [-c 8]")
		os.Exit(1)
	}
	fi, err := os.Stat(local)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stat %s: %v\n", local, err)
		os.Exit(1)
	}

	// folder handling
	if fi.IsDir() {
		if *useZip && *noZip {
			fmt.Fprintln(os.Stderr, "error: --zip and --no-zip cannot be used together")
			os.Exit(1)
		}
		if *concurrency < 1 {
			*concurrency = 1
		}
		if *concurrency > 32 {
			fmt.Fprintln(os.Stderr, "warning: concurrency capped at 32")
			*concurrency = 32
		}
		if *useZip {
			fmt.Fprintf(os.Stderr, "zipping and streaming folder %s -> %s\n", local, *remoteF)
			if err := client.PutFolder(nil, local, *remoteF, opts, true, 0); err != nil {
				fmt.Fprintf(os.Stderr, "put folder (zip) failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintln(os.Stderr, "done")
			return
		}
		dirOpts := alist.UploadDirOptions{AsTask: *asTask, Concurrency: *concurrency, SkipErrors: *skipErrors}
		if *concurrency > 1 {
			fmt.Fprintf(os.Stderr, "put folder %s -> %s (concurrency=%d skip-errors=%v)\n", local, *remoteF, *concurrency, *skipErrors)
		} else {
			fmt.Fprintf(os.Stderr, "put folder %s -> %s (skip-errors=%v)\n", local, *remoteF, *skipErrors)
		}
		_, _, failed, err := client.UploadDirConcurrentWithOptions(nil, local, *remoteF, dirOpts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "put folder failed: %v\n", err)
			os.Exit(1)
		}
		if len(failed) > 0 {
			fmt.Fprintf(os.Stderr, "put folder completed with %d skipped errors:\n", len(failed))
			for _, fe := range failed {
				fmt.Fprintf(os.Stderr, "  skip %s: %v\n", fe.Rel, fe.Err)
			}
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "done")
		return
	}

	fmt.Fprintf(os.Stderr, "uploading %s -> %s (%s)\n", local, *remoteF, humanBytes(fi.Size()))
	if err := client.PutFile(nil, local, *remoteF, opts); err != nil {
		fmt.Fprintf(os.Stderr, "put failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "done")
}

func runPack(args []string) {
	fs := flag.NewFlagSet("pack", flag.ExitOnError)
	out := fs.String("o", "", "output zip path")
	fs.String("output", "", "output zip path (alias for -o)")
	fs.Parse(args)
	// normalize long form manually for pack (flag package handles -output but not --output)
	for i, a := range args {
		if a == "--output" && i+1 < len(args) && *out == "" {
			*out = args[i+1]
		}
		if strings.HasPrefix(a, "--output=") && *out == "" {
			*out = strings.TrimPrefix(a, "--output=")
		}
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: alistbatch pack <folder|file> [-o output.zip]")
		fs.Usage()
		os.Exit(1)
	}
	src := rest[0]
	if *out == "" {
		*out = filepath.Base(strings.TrimRight(src, "/")) + ".zip"
		if *out == ".zip" {
			*out = "pack.zip"
		}
	}
	fmt.Fprintf(os.Stderr, "packing %s -> %s\n", src, *out)
	if err := pack.ZipFolder(src, *out); err != nil {
		fmt.Fprintf(os.Stderr, "pack failed: %v\n", err)
		os.Exit(1)
	}
	fi, _ := os.Stat(*out)
	fmt.Fprintf(os.Stderr, "done: %s (%s)\n", *out, humanBytes(fi.Size()))
}

func runLogin(args []string) {
	args = normalizeArgs(args, map[string]string{
		"--host":     "-H",
		"--username": "-u",
		"--password": "-p",
		"--token":    "-t",
	})
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	hostF := fs.String("H", "", "host")
	usernameF := fs.String("u", "", "username")
	passwordF := fs.String("p", "", "password")
	tokenF := fs.String("t", "", "token (if you already have one, just save it)")
	noSave := fs.Bool("no-save", false, "do not save to config file, just print token")
	fs.Parse(args)
	rest := fs.Args()
	host := *hostF
	if host == "" && len(rest) >= 1 {
		host = rest[0]
	}
	if host == "" {
		host = os.Getenv("ALIST_HOST")
	}
	if host == "" {
		if cfg, _ := config.Load(); cfg != nil {
			host = cfg.Host
		}
	}
	if host == "" {
		fmt.Fprintln(os.Stderr, "host required (-H or ALIST_HOST)")
		os.Exit(1)
	}
	host = strings.TrimRight(host, "/")

	if *tokenF != "" {
		if !*noSave {
			if err := config.Update(host, *usernameF, *passwordF, *tokenF); err != nil {
				fmt.Fprintf(os.Stderr, "save config failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "saved token to %s\n", config.Path())
		}
		fmt.Printf("token: %s\n", *tokenF)
		return
	}

	username := *usernameF
	password := *passwordF
	if username == "" {
		username = os.Getenv("ALIST_USERNAME")
	}
	if password == "" {
		password = os.Getenv("ALIST_PASSWORD")
	}
	if username == "" || password == "" {
		if cfg, _ := config.Load(); cfg != nil {
			if username == "" {
				username = cfg.Username
			}
			if password == "" {
				password = cfg.Password
			}
		}
	}
	if username == "" || password == "" {
		fmt.Fprintln(os.Stderr, "username and password required (-u/-p or ALIST_USERNAME/ALIST_PASSWORD or saved config)")
		os.Exit(1)
	}

	c := alist.NewClient(host, "")
	if err := c.Login(username, password); err != nil {
		fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("token: %s\n", c.Token)
	if !*noSave {
		if err := config.Update(host, username, password, c.Token); err != nil {
			fmt.Fprintf(os.Stderr, "save config failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "saved to %s\n", config.Path())
		fmt.Fprintf(os.Stderr, "host=%s user=%s\n", host, username)
	} else {
		fmt.Printf("\nexport ALIST_HOST=%s\n", host)
		fmt.Printf("export ALIST_TOKEN=%s\n", c.Token)
	}
}

func runLogout(args []string) {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	all := fs.Bool("all", false, "clear all saved credentials (host/user/pass/token)")
	fs.Parse(args)
	if *all {
		if err := config.ClearAll(); err != nil {
			fmt.Fprintf(os.Stderr, "clear failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "cleared %s\n", config.Path())
		return
	}
	if err := config.ClearToken(); err != nil {
		fmt.Fprintf(os.Stderr, "clear token failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "cleared token in %s\n", config.Path())
}

func runConfig(args []string) {
	args = normalizeArgs(args, map[string]string{
		"--host":     "-H",
		"--username": "-u",
		"--password": "-p",
		"--token":    "-t",
		"--clear":    "-clear",
		"--path":     "-path",
		"--json":     "-json",
	})
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	hostF := fs.String("H", "", "set host")
	fs.String("host", "", "set host")
	usernameF := fs.String("u", "", "set username")
	fs.String("username", "", "set username")
	passwordF := fs.String("p", "", "set password")
	fs.String("password", "", "set password")
	tokenF := fs.String("t", "", "set token")
	fs.String("token", "", "set token")
	clear := fs.Bool("clear", false, "clear config file")
	showPath := fs.Bool("path", false, "print config file path")
	jsonOut := fs.Bool("json", false, "output as JSON")
	fs.Parse(args)

	if *showPath {
		fmt.Println(config.Path())
		return
	}
	if *clear {
		if err := config.ClearAll(); err != nil {
			fmt.Fprintf(os.Stderr, "clear failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "cleared %s\n", config.Path())
		return
	}
	if *hostF != "" || *usernameF != "" || *passwordF != "" || *tokenF != "" {
		if err := config.Update(*hostF, *usernameF, *passwordF, *tokenF); err != nil {
			fmt.Fprintf(os.Stderr, "update failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "updated %s\n", config.Path())
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(cfg.Sanitized())
		return
	}
	fmt.Printf("config: %s\n", config.Path())
	fmt.Printf("  host:     %s\n", cfg.Host)
	fmt.Printf("  username: %s\n", cfg.Username)
	if cfg.Password != "" {
		fmt.Printf("  password: *** (set)\n")
	} else {
		fmt.Printf("  password: (not set)\n")
	}
	if cfg.Token != "" {
		masked := cfg.Token
		if len(masked) > 12 {
			masked = masked[:4] + "..." + masked[len(masked)-4:]
		} else {
			masked = "***"
		}
		fmt.Printf("  token:    %s (%d chars)\n", masked, len(cfg.Token))
	} else {
		fmt.Printf("  token:    (not set)\n")
	}
}

func runMkdir(args []string) {
	args = normalizeArgs(args, map[string]string{
		"--host":     "-H",
		"--username": "-u",
		"--password": "-p",
		"--token":    "-t",
	})
	fs := flag.NewFlagSet("mkdir", flag.ExitOnError)
	hostF := fs.String("H", "", "host")
	tokenF := fs.String("t", "", "token")
	usernameF := fs.String("u", "", "username")
	passwordF := fs.String("p", "", "password")
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: alistbatch mkdir <remote_path>")
		os.Exit(1)
	}
	remote := rest[0]
	auth, err := resolveAuth(*hostF, *tokenF, *usernameF, *passwordF)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve auth: %v\n", err)
		os.Exit(1)
	}
	c, err := ensureClient(auth, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth failed: %v\n", err)
		os.Exit(1)
	}
	if err := c.EnsureDir(remote); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "ok")
}

func runList(args []string) {
	args = normalizeArgs(args, map[string]string{
		"--host":      "-H",
		"--username":  "-u",
		"--password":  "-p",
		"--token":     "-t",
		"--long":      "-l",
		"--recursive": "-R",
		"--refresh":   "-f",
		"--per_page":  "-per-page",
		"--per-page":  "-per-page",
	})
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	hostF := fs.String("H", "", "host")
	usernameF := fs.String("u", "", "username")
	passwordF := fs.String("p", "", "password")
	tokenF := fs.String("t", "", "token")
	long := fs.Bool("l", false, "long format (size, modified, type)")
	jsonOut := fs.Bool("json", false, "output as JSON")
	recursive := fs.Bool("R", false, "list recursively")
	refresh := fs.Bool("f", false, "force refresh from storage")
	page := fs.Int("page", 1, "page number (ignored with --recursive)")
	perPage := fs.Int("per-page", 100, "items per page")
	fs.Parse(args)
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	rest := fs.Args()
	remote := "/"
	if len(rest) >= 1 {
		remote = rest[0]
	}
	if !strings.HasPrefix(remote, "/") {
		remote = "/" + remote
	}
	auth, err := resolveAuth(*hostF, *tokenF, *usernameF, *passwordF)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve auth: %v\n", err)
		os.Exit(1)
	}
	if auth.Host == "" {
		fmt.Fprintln(os.Stderr, "error: Alist host required (-H or ALIST_HOST or saved config)")
		os.Exit(1)
	}
	c := alist.NewClient(auth.Host, auth.Token)
	c.SetCredentials(auth.Username, auth.Password)
	c.OnTokenRefresh = func(newToken string) {
		_ = config.Update(auth.Host, "", "", newToken)
		fmt.Fprintf(os.Stderr, "token refreshed and saved to %s\n", config.Path())
	}
	if c.Token == "" && c.Username != "" && c.Password != "" {
		fmt.Fprintf(os.Stderr, "logging in to %s as %s ...\n", auth.Host, c.Username)
		if err := c.Login(c.Username, c.Password); err != nil {
			fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
			os.Exit(1)
		}
		_ = config.Update(auth.Host, "", "", c.Token)
	}

	doList := func(cli *alist.Client) error {
		if *recursive {
			tree, err := cli.ListRecursive(remote, *perPage, *refresh)
			if err != nil {
				return err
			}
			if *jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(tree)
			}
			dirs := make([]string, 0, len(tree))
			for d := range tree {
				dirs = append(dirs, d)
			}
			sort.Strings(dirs)
			for _, d := range dirs {
				items := tree[d]
				fmt.Printf("%s (%d items):\n", d, len(items))
				printListItems(items, *long)
				fmt.Println()
			}
			return nil
		}

		if *jsonOut {
			if setFlags["page"] || setFlags["per-page"] {
				resp, err := cli.ListWithOptions(remote, alist.ListOptions{Page: *page, PerPage: *perPage, Refresh: *refresh})
				if err != nil {
					return err
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			items, err := cli.ListAll(remote, *perPage, *refresh)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(items)
		}

		var items []alist.ListItem
		var total int
		if setFlags["page"] || setFlags["per-page"] {
			resp, err := cli.ListWithOptions(remote, alist.ListOptions{Page: *page, PerPage: *perPage, Refresh: *refresh})
			if err != nil {
				return err
			}
			items = resp.Content
			total = resp.Total
			fmt.Printf("%s (page %d, %d/%d):\n", remote, *page, len(items), total)
		} else {
			all, err := cli.ListAll(remote, *perPage, *refresh)
			if err != nil {
				return err
			}
			items = all
			total = len(all)
			fmt.Printf("%s (%d items):\n", remote, total)
		}
		printListItems(items, *long)
		return nil
	}

	if err := doList(c); err != nil {
		fmt.Fprintf(os.Stderr, "list failed: %v\n", err)
		os.Exit(1)
	}
}

func printListItems(items []alist.ListItem, long bool) {
	for _, it := range items {
		if long {
			typ := "file"
			if it.IsDir {
				typ = "dir "
			}
			fmt.Printf("  %-4s %10s  %-19s  %s\n", typ, humanBytes(it.Size), it.Modified, it.Name)
		} else {
			suffix := ""
			if it.IsDir {
				suffix = "/"
			}
			fmt.Printf("  %s%s\n", it.Name, suffix)
		}
	}
}

func normalizeArgs(args []string, mapping map[string]string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if v, ok := mapping[a]; ok {
			out = append(out, v)
		} else if strings.Contains(a, "=") {
			parts := strings.SplitN(a, "=", 2)
			if v, ok := mapping[parts[0]]; ok {
				out = append(out, v+"="+parts[1])
			} else {
				out = append(out, a)
			}
		} else {
			out = append(out, a)
		}
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	div, exp := int64(unit), 0
	for exp < len(units)-1 && n >= div*unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), units[exp])
}
