package alist

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Alist REST API docs: https://alist.nn.ci/guide/api.html
// Key endpoints:
//   POST /api/auth/login          -> {code, data:{token}}
//   PUT  /api/fs/put              -> stream upload, headers: Authorization, File-Path, As-Task, Content-Type
//   POST /api/fs/mkdir            -> {path}
//   POST /api/fs/list             -> {path, password, page, per_page, refresh}
//   GET  /api/fs/get              -> ?path=
//   DELETE /api/fs/remove         -> {names, dir}

type Client struct {
	Host       string
	Token      string
	Username   string
	Password   string
	HTTPClient *http.Client
	// OnTokenRefresh is called after a successful auto re-login with the new token.
	// Set it to persist the token (e.g. save to config file).
	OnTokenRefresh func(newToken string)
}

func NewClient(host, token string) *Client {
	return &Client{
		Host:  strings.TrimRight(host, "/"),
		Token: token,
		HTTPClient: &http.Client{
			Timeout: 0, // no timeout for large uploads; per-request context controls
		},
	}
}

// SetCredentials sets username/password for auto re-login on 401.
func (c *Client) SetCredentials(username, password string) {
	c.Username = username
	c.Password = password
}

func (c *Client) canRelogin() bool {
	return c.Username != "" && c.Password != ""
}

func (c *Client) relogin() error {
	if !c.canRelogin() {
		return fmt.Errorf("no credentials for re-login")
	}
	if err := c.Login(c.Username, c.Password); err != nil {
		return err
	}
	if c.OnTokenRefresh != nil {
		c.OnTokenRefresh(c.Token)
	}
	return nil
}

// ---------- auth ----------

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Token string `json:"token"`
	} `json:"data"`
}

func (c *Client) Login(username, password string) error {
	body, _ := json.Marshal(loginReq{Username: username, Password: password})
	req, err := http.NewRequest("POST", c.Host+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r loginResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decode login resp: %w", err)
	}
	if r.Code != 200 {
		return fmt.Errorf("login failed code=%d msg=%s", r.Code, r.Message)
	}
	if r.Data.Token == "" {
		return fmt.Errorf("login: empty token")
	}
	c.Token = r.Data.Token
	return nil
}

// ---------- generic api ----------

type apiResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (c *Client) doJSON(method, path string, payload any) (*apiResp, error) {
	return c.doJSONWithRetry(method, path, payload, true)
}

func (c *Client) doJSONWithRetry(method, path string, payload any, allowRetry bool) (*apiResp, error) {
	var body io.Reader
	var bodyBytes []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		bodyBytes = b
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.Host+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", c.Token)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var r apiResp
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w body=%s", method, path, err, string(b))
	}
	if r.Code != 200 {
		// auto re-login on 401 and retry once
		if allowRetry && isUnauthorizedCode(r.Code, r.Message) && c.canRelogin() {
			if err := c.relogin(); err != nil {
				return &r, fmt.Errorf("api %s %s code=%d msg=%s (re-login failed: %v)", method, path, r.Code, r.Message, err)
			}
			// retry with new token — need to rebuild body
			var retryBody io.Reader
			if bodyBytes != nil {
				retryBody = bytes.NewReader(bodyBytes)
			}
			retryReq, err := http.NewRequest(method, c.Host+path, retryBody)
			if err != nil {
				return nil, err
			}
			retryReq.Header.Set("Content-Type", "application/json")
			retryReq.Header.Set("Authorization", c.Token)
			retryResp, err := c.HTTPClient.Do(retryReq)
			if err != nil {
				return nil, err
			}
			defer retryResp.Body.Close()
			rb, err := io.ReadAll(retryResp.Body)
			if err != nil {
				return nil, err
			}
			var rr apiResp
			if err := json.Unmarshal(rb, &rr); err != nil {
				return nil, fmt.Errorf("decode %s %s (retry): %w body=%s", method, path, err, string(rb))
			}
			if rr.Code != 200 {
				return &rr, fmt.Errorf("api %s %s code=%d msg=%s", method, path, rr.Code, rr.Message)
			}
			return &rr, nil
		}
		return &r, fmt.Errorf("api %s %s code=%d msg=%s", method, path, r.Code, r.Message)
	}
	return &r, nil
}

func isUnauthorizedCode(code int, msg string) bool {
	if code == 401 {
		return true
	}
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "unauthorized") || strings.Contains(lower, "not login") || strings.Contains(lower, "token expired") || strings.Contains(lower, "please login")
}

// ---------- fs ops ----------

func (c *Client) Mkdir(path string) error {
	_, err := c.doJSON("POST", "/api/fs/mkdir", map[string]string{"path": path})
	return err
}

// EnsureDir creates all parent dirs (mkdir -p).
func (c *Client) EnsureDir(dir string) error {
	dir = filepath.ToSlash(dir)
	dir = strings.TrimRight(dir, "/")
	if dir == "" || dir == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	cur := ""
	for _, p := range parts {
		cur += "/" + p
		if err := c.Mkdir(cur); err != nil {
			if isAlreadyExistsErr(err) {
				continue
			}
			// Alist error messages vary by storage driver; verify via List.
			if _, lerr := c.List(cur); lerr == nil {
				continue
			}
			return err
		}
	}
	return nil
}

func isAlreadyExistsErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already exists") || strings.Contains(s, "already exist") || strings.Contains(s, "exists")
}

// IsUnauthorized reports whether err is a 401/unauthorized from Alist.
func IsUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "code=401") || strings.Contains(s, "unauthorized") || strings.Contains(s, "not login")
}

type ListItem struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	IsDir    bool   `json:"is_dir"`
	Modified string `json:"modified"`
	Created  string `json:"created"`
	Sign     string `json:"sign"`
	Thumb    string `json:"thumb"`
	Type     int    `json:"type"`
	HashInfo string `json:"hashinfo"`
}

type ListResp struct {
	Content []ListItem `json:"content"`
	Total   int        `json:"total"`
	Readme  string     `json:"readme"`
	Header  string     `json:"header"`
	Write   bool       `json:"write"`
	Provider string    `json:"provider"`
}

type ListOptions struct {
	Page     int
	PerPage  int
	Password string
	Refresh  bool
}

func (c *Client) List(path string) (*ListResp, error) {
	return c.ListWithOptions(path, ListOptions{Page: 1, PerPage: 100})
}

func (c *Client) ListWithOptions(path string, opts ListOptions) (*ListResp, error) {
	if opts.Page == 0 {
		opts.Page = 1
	}
	if opts.PerPage == 0 {
		opts.PerPage = 100
	}
	r, err := c.doJSON("POST", "/api/fs/list", map[string]any{
		"path":     path,
		"password": opts.Password,
		"page":     opts.Page,
		"per_page": opts.PerPage,
		"refresh":  opts.Refresh,
	})
	if err != nil {
		return nil, err
	}
	var data ListResp
	if err := json.Unmarshal(r.Data, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

// ListAll fetches all pages for a path.
func (c *Client) ListAll(path string, perPage int, refresh bool) ([]ListItem, error) {
	if perPage <= 0 {
		perPage = 100
	}
	var all []ListItem
	page := 1
	for {
		resp, err := c.ListWithOptions(path, ListOptions{Page: page, PerPage: perPage, Refresh: refresh})
		if err != nil {
			return nil, err
		}
		all = append(all, resp.Content...)
		if resp.Total > 0 && len(all) >= resp.Total {
			break
		}
		if len(resp.Content) == 0 || len(resp.Content) < perPage {
			break
		}
		page++
	}
	return all, nil
}

// ListRecursive walks remote tree depth-first, returning map dir -> items.
func (c *Client) ListRecursive(root string, perPage int, refresh bool) (map[string][]ListItem, error) {
	result := make(map[string][]ListItem)
	queue := []string{root}
	visited := make(map[string]bool)
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if visited[dir] {
			continue
		}
		visited[dir] = true
		items, err := c.ListAll(dir, perPage, refresh)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", dir, err)
		}
		result[dir] = items
		for _, it := range items {
			if it.IsDir {
				sub := strings.TrimRight(dir, "/") + "/" + it.Name
				queue = append(queue, sub)
			}
		}
	}
	return result, nil
}

// ---------- upload ----------

// UploadFile streams local file to remotePath via PUT /api/fs/put.
// Alist expects headers: Authorization, File-Path (url-encoded), As-Task, Content-Type.
// See: https://alist.nn.ci/guide/api.html#upload-file
// If the token is expired (401), it auto re-logins using Username/Password and retries once.
func (c *Client) UploadFile(localPath, remotePath string, asTask bool, overwrite bool) error {
	return c.uploadFileWithRetry(localPath, remotePath, asTask, overwrite, true)
}

func (c *Client) uploadFileWithRetry(localPath, remotePath string, asTask bool, overwrite bool, allowRetry bool) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}

	// ensure parent dir exists
	parent := filepath.ToSlash(filepath.Dir(remotePath))
	if parent != "/" && parent != "." {
		if err := c.EnsureDir(parent); err != nil {
			return fmt.Errorf("ensure dir %s: %w", parent, err)
		}
	}

	encodedPath := encodeFilePath(remotePath)

	req, err := http.NewRequest("PUT", c.Host+"/api/fs/put", f)
	if err != nil {
		return err
	}
	req.ContentLength = fi.Size()
	req.Header.Set("Authorization", c.Token)
	req.Header.Set("File-Path", encodedPath)
	req.Header.Set("Content-Type", "application/octet-stream")
	if asTask {
		req.Header.Set("As-Task", "true")
	} else {
		req.Header.Set("As-Task", "false")
	}

	httpClient := &http.Client{Timeout: 0}
	var body io.ReadCloser = f
	if !asTask && fi.Size() > 0 {
		pr := &progressReader{Reader: f, total: fi.Size(), name: filepath.Base(localPath), start: time.Now()}
		body = pr
	}
	req.Body = body

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var r apiResp
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("upload decode: %w body=%s status=%d", err, string(b), resp.StatusCode)
	}
	if r.Code != 200 {
		if allowRetry && isUnauthorizedCode(r.Code, r.Message) && c.canRelogin() {
			fmt.Println("token expired, re-logging in ...")
			if err := c.relogin(); err != nil {
				return fmt.Errorf("upload failed code=%d msg=%s (re-login failed: %v)", r.Code, r.Message, err)
			}
			// retry once — need to reopen file (previous reader consumed)
			f.Close()
			return c.uploadFileWithRetry(localPath, remotePath, asTask, overwrite, false)
		}
		return fmt.Errorf("upload failed code=%d msg=%s body=%s", r.Code, r.Message, string(b))
	}
	fmt.Printf("\n")
	return nil
}

// UploadDirRecursive uploads folder file-by-file preserving structure.
func (c *Client) UploadDirRecursive(localDir, remoteDir string, asTask bool) (int, int64, error) {
	var count int
	var total int64
	err := filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		remotePath := strings.TrimRight(remoteDir, "/") + "/" + rel
		fmt.Printf("  [%d] %s -> %s (%d bytes)\n", count+1, rel, remotePath, info.Size())
		if err := c.UploadFile(path, remotePath, asTask, true); err != nil {
			return fmt.Errorf("upload %s: %w", rel, err)
		}
		count++
		total += info.Size()
		return nil
	})
	return count, total, err
}

func encodeFilePath(p string) string {
	// encode each segment, keep "/" separators
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if s == "" {
			continue
		}
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// progressReader prints upload progress.
type progressReader struct {
	io.Reader
	total   int64
	read    int64
	name    string
	start   time.Time
	lastPct int
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.Reader.Read(p)
	pr.read += int64(n)
	if pr.total > 0 {
		pct := int(pr.read * 100 / pr.total)
		if pct != pr.lastPct && pct%5 == 0 {
			fmt.Printf("\r  %s: %d%% (%d/%d)", pr.name, pct, pr.read, pr.total)
			pr.lastPct = pct
		}
	}
	return n, err
}

func (pr *progressReader) Close() error {
	if c, ok := pr.Reader.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
