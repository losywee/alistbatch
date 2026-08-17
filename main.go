package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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

  # one-off with explicit host/token (no save)
  alistbatch upload -H https://alist.example.com -t $TOKEN -s ./a.txt -r /a.txt

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

// resolveAuth merges flags > env > config file.
// hostFlag/tokenFlag/etc are pointers to flag values (may be empty).
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

// ensureClient returns an authenticated client, auto-logging in if needed.
// It also wires auto re-login on 401: if any subsequent API call gets 401,
// the client will re-login with Username/Password and retry once, persisting the new token.
func ensureClient(a *resolvedAuth, save bool) (*alist.Client, error) {
	if a.Host == "" {
		return nil, fmt.Errorf("Alist host required (-H or ALIST_HOST or saved config). Run: alistbatch login -H <host> -u <user> -p <pass>")
	}
	c := alist.NewClient(a.Host, a.Token)
	c.SetCredentials(a.Username, a.Password)
	c.OnTokenRefresh = func(newToken string) {
		_ = config.Update(a.Host, "", "", newToken)
		fmt.Printf("token refreshed and saved to %s\n", config.Path())
	}
	if c.Token != "" {
		return c, nil
	}
	if a.Username == "" || a.Password == "" {
		return nil, fmt.Errorf("token or username/password required (no saved credentials). Run: alistbatch login -H %s -u <user> -p <pass>", a.Host)
	}
	fmt.Printf("logging in to %s as %s ...\n", a.Host, a.Username)
	if err := c.Login(a.Username, a.Password); err != nil {
		return nil, err
	}
	fmt.Println("login ok")
	if save {
		if err := config.Update(a.Host, a.Username, a.Password, c.Token); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to save config: %v\n", err)
		} else {
			fmt.Printf("saved credentials to %s\n", config.Path())
		}
	} else {
		if a.Cfg.Host == a.Host || a.Cfg.Host == "" {
			_ = config.Update(a.Host, "", "", c.Token)
		}
	}
	return c, nil
}

// ---------- upload ----------

func runUpload(args []string) {
	args = normalizeArgs(args, map[string]string{
		"--host":     "-H",
		"--username": "-u",
		"--password": "-p",
		"--token":    "-t",
		"--src":      "-s",
		"--remote":   "-r",
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
		fmt.Printf("uploading file %s -> %s\n", *src, *remote)
		if err := client.UploadFile(*src, *remote, *asTask, *overwrite); err != nil {
			fmt.Fprintf(os.Stderr, "upload failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("done")
		return
	}

	if *noZip {
		fmt.Printf("uploading folder recursively %s -> %s\n", *src, *remote)
		if err := client.EnsureDir(*remote); err != nil {
			fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", *remote, err)
			os.Exit(1)
		}
		count, bytes, err := client.UploadDirRecursive(*src, *remote, *asTask)
		if err != nil {
			fmt.Fprintf(os.Stderr, "upload dir failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("done: %d files, %s\n", count, humanBytes(bytes))
		return
	}

	fmt.Printf("packing folder %s ...\n", *src)
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
	fmt.Printf("packed %s (%s) -> uploading to %s\n", tmpZip, humanBytes(fi2.Size()), *remote)
	if err := client.UploadFile(tmpZip, *remote, *asTask, *overwrite); err != nil {
		fmt.Fprintf(os.Stderr, "upload failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("done")
}

func runPack(args []string) {
	var out string
	var src string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-o" || a == "--output" || a == "-output" {
			if i+1 < len(args) {
				out = args[i+1]
				i++
			}
		} else if strings.HasPrefix(a, "-") {
			fmt.Fprintf(os.Stderr, "unknown flag %s\n", a)
			os.Exit(1)
		} else {
			if src == "" {
				src = a
			}
		}
	}
	if src == "" {
		fmt.Fprintln(os.Stderr, "usage: alistbatch pack <folder|file> [-o output.zip]")
		os.Exit(1)
	}
	if out == "" {
		out = filepath.Base(strings.TrimRight(src, "/")) + ".zip"
		if out == ".zip" {
			out = "pack.zip"
		}
	}
	fmt.Printf("packing %s -> %s\n", src, out)
	if err := pack.ZipFolder(src, out); err != nil {
		fmt.Fprintf(os.Stderr, "pack failed: %v\n", err)
		os.Exit(1)
	}
	fi, _ := os.Stat(out)
	fmt.Printf("done: %s (%s)\n", out, humanBytes(fi.Size()))
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
	// also allow env/config fallback for host
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

	// if token provided directly, just save it
	if *tokenF != "" {
		if !*noSave {
			if err := config.Update(host, *usernameF, *passwordF, *tokenF); err != nil {
				fmt.Fprintf(os.Stderr, "save config failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("saved token to %s\n", config.Path())
		}
		fmt.Printf("token: %s\n", *tokenF)
		return
	}

	username := *usernameF
	password := *passwordF
	// fallback to env/config for username/password if not flagged
	if username == "" {
		username = os.Getenv("ALIST_USERNAME")
	}
	if password == "" {
		password = os.Getenv("ALIST_PASSWORD")
	}
	if (username == "" || password == "") {
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
		fmt.Printf("saved to %s\n", config.Path())
		fmt.Printf("host=%s user=%s\n", host, username)
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
		fmt.Printf("cleared %s\n", config.Path())
		return
	}
	if err := config.ClearToken(); err != nil {
		fmt.Fprintf(os.Stderr, "clear token failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("cleared token in %s\n", config.Path())
}

func runConfig(args []string) {
	// alistbatch config              -> show
	// alistbatch config --host X     -> set host
	// alistbatch config --clear      -> clear all
	// alistbatch config --path       -> print path
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	hostF := fs.String("H", "", "set host")
	_ = fs.String("host", "", "")
	usernameF := fs.String("u", "", "set username")
	_ = fs.String("username", "", "")
	passwordF := fs.String("p", "", "set password")
	_ = fs.String("password", "", "")
	tokenF := fs.String("t", "", "set token")
	_ = fs.String("token", "", "")
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
		fmt.Printf("cleared %s\n", config.Path())
		return
	}
	// if any set flag provided, update
	if *hostF != "" || *usernameF != "" || *passwordF != "" || *tokenF != "" {
		// also handle long forms via normalize
		for i, a := range os.Args {
			if a == "--host" && i+1 < len(os.Args) {
				*hostF = os.Args[i+1]
			}
			if a == "--username" && i+1 < len(os.Args) {
				*usernameF = os.Args[i+1]
			}
			if a == "--password" && i+1 < len(os.Args) {
				*passwordF = os.Args[i+1]
			}
			if a == "--token" && i+1 < len(os.Args) {
				*tokenF = os.Args[i+1]
			}
		}
		if err := config.Update(*hostF, *usernameF, *passwordF, *tokenF); err != nil {
			fmt.Fprintf(os.Stderr, "update failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("updated %s\n", config.Path())
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
	fmt.Println("ok")
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
		fmt.Printf("token refreshed and saved to %s\n", config.Path())
	}
	// if no token but have credentials, login now
	if c.Token == "" && c.Username != "" && c.Password != "" {
		fmt.Printf("logging in to %s as %s ...\n", auth.Host, c.Username)
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
			for i := 0; i < len(dirs); i++ {
				for j := i + 1; j < len(dirs); j++ {
					if dirs[j] < dirs[i] {
						dirs[i], dirs[j] = dirs[j], dirs[i]
					}
				}
			}
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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n >= div*unit && exp < 4 {
		div *= unit
		exp++
	}
	units := []string{"KB", "MB", "GB", "TB"}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), units[exp])
}
