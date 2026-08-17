package alist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"alistbatch/internal/pack"
)

// Alist REST API docs: https://alist.nn.ci/guide/api.html
// Key endpoints:
//   POST /api/auth/login          -> {code, data:{token}}
//   PUT  /api/fs/put              -> stream upload, headers: Authorization, File-Path, As-Task, Content-Type
//   POST /api/fs/mkdir            -> {path}
//   POST /api/fs/list             -> {path, password, page, per_page, refresh}
//   GET  /api/fs/get              -> ?path=
//   DELETE /api/fs/remove         -> {names, dir}

const (
	defaultTimeout = 30 * time.Second
	uploadTimeout  = 0 // no timeout for streaming upload
)

type Client struct {
	Host       string
	Token      string
	Username   string
	Password   string
	HTTPClient *http.Client
	// OnTokenRefresh is called after a successful auto re-login with the new token.
	// Set it to persist the token (e.g. save to config file).
	OnTokenRefresh func(newToken string)

	mu sync.RWMutex // protects Token
}

var stderrMu sync.Mutex

func logf(format string, args ...any) {
	stderrMu.Lock()
	defer stderrMu.Unlock()
	fmt.Fprintf(os.Stderr, format, args...)
}

// uploadClient returns an http.Client with no timeout but shared Transport for pooling.
func (c *Client) uploadClient() *http.Client {
	if c.HTTPClient != nil && c.HTTPClient.Transport != nil {
		return &http.Client{Timeout: uploadTimeout, Transport: c.HTTPClient.Transport}
	}
	// share default transport for connection pooling
	return &http.Client{Timeout: uploadTimeout, Transport: http.DefaultTransport}
}

func NewClient(host, token string) *Client {
	return &Client{
		Host:  strings.TrimRight(host, "/"),
		Token: token,
		HTTPClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

// UploadError records a per-file failure when skipErrors is enabled.
type UploadError struct {
	Rel string
	Err error
}

func (e UploadError) Error() string { return fmt.Sprintf("%s: %v", e.Rel, e.Err) }

// UploadDirOptions controls batch folder upload.
type UploadDirOptions struct {
	AsTask       bool
	Concurrency  int
	SkipErrors   bool
	SkipExisting bool // skip files that already exist on remote (checked via List)
}

type job struct {
	localPath  string
	remotePath string
	size       int64
	rel        string
}

// SetCredentials sets username/password for auto re-login on 401.
func (c *Client) SetCredentials(username, password string) {
	c.Username = username
	c.Password = password
}

func (c *Client) canRelogin() bool {
	return c.Username != "" && c.Password != ""
}

func (c *Client) getToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Token
}

func (c *Client) setToken(tok string) {
	c.mu.Lock()
	c.Token = tok
	c.mu.Unlock()
}

func (c *Client) relogin(ctx context.Context) error {
	if !c.canRelogin() {
		return fmt.Errorf("no credentials for re-login")
	}
	// LoginWithContext will set Token under lock
	if err := c.LoginWithContext(ctx, c.Username, c.Password); err != nil {
		return err
	}
	if c.OnTokenRefresh != nil {
		c.OnTokenRefresh(c.getToken())
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
	return c.LoginWithContext(context.Background(), username, password)
}

func (c *Client) LoginWithContext(ctx context.Context, username, password string) error {
	body, _ := json.Marshal(loginReq{Username: username, Password: password})
	req, err := http.NewRequestWithContext(ctx, "POST", c.Host+"/api/auth/login", bytes.NewReader(body))
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
	c.setToken(r.Data.Token)
	return nil
}

// ---------- generic api ----------

type apiResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (c *Client) doJSON(method, path string, payload any) (*apiResp, error) {
	return c.doJSONWithContext(context.Background(), method, path, payload, true)
}

func (c *Client) doJSONWithContext(ctx context.Context, method, path string, payload any, allowRetry bool) (*apiResp, error) {
	var bodyBytes []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		bodyBytes = b
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.Host+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	if method != "POST" {
		req.Method = method
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := c.getToken(); tok != "" {
		req.Header.Set("Authorization", tok)
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
		if allowRetry && isUnauthorizedCode(r.Code, r.Message) && c.canRelogin() {
			if err := c.relogin(ctx); err != nil {
				return &r, fmt.Errorf("api %s %s code=%d msg=%s (re-login failed: %v)", method, path, r.Code, r.Message, err)
			}
			retryReq, err := http.NewRequestWithContext(ctx, "POST", c.Host+path, bytes.NewReader(bodyBytes))
			if err != nil {
				return nil, err
			}
			if method != "POST" {
				retryReq.Method = method
			}
			retryReq.Header.Set("Content-Type", "application/json")
			retryReq.Header.Set("Authorization", c.getToken())
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

// IsUnauthorized reports whether err is a 401/unauthorized from Alist.
// Kept for backward compat; prefer isUnauthorizedCode internally.
func IsUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "code=401") || strings.Contains(s, "unauthorized") || strings.Contains(s, "not login") || strings.Contains(s, "token expired") || strings.Contains(s, "please login")
}

// ---------- fs ops ----------

func (c *Client) Mkdir(path string) error {
	_, err := c.doJSON("POST", "/api/fs/mkdir", map[string]string{"path": path})
	return err
}

// EnsureDir creates all parent dirs (mkdir -p).
func (c *Client) EnsureDir(dir string) error {
	return c.EnsureDirWithContext(context.Background(), dir)
}

// EnsureDirWithContext is like EnsureDir but respects ctx.
func (c *Client) EnsureDirWithContext(ctx context.Context, dir string) error {
	dir = filepath.ToSlash(dir)
	dir = strings.TrimRight(dir, "/")
	if dir == "" || dir == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	cur := ""
	for _, p := range parts {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		cur += "/" + p
		if err := c.MkdirWithContext(ctx, cur); err != nil {
			if isAlreadyExistsErr(err) {
				continue
			}
			if _, lerr := c.ListWithContext(ctx, cur); lerr == nil {
				continue
			}
			return err
		}
	}
	return nil
}

func isAlreadyExistsErr(err error) bool {
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "already exists") || strings.Contains(s, "already exist") {
		return true
	}
	if strings.Contains(s, "已存在") {
		return true
	}
	return false
}

// MkdirWithContext is like Mkdir but with ctx.
func (c *Client) MkdirWithContext(ctx context.Context, path string) error {
	_, err := c.doJSONWithContext(ctx, "POST", "/api/fs/mkdir", map[string]string{"path": path}, true)
	return err
}

// ListWithContext is like ListWithOptions but with explicit ctx.
func (c *Client) ListWithContext(ctx context.Context, path string) (*ListResp, error) {
	return c.ListWithOptionsAndContext(ctx, path, ListOptions{Page: 1, PerPage: 100})
}

// ListWithOptionsAndContext is like ListWithOptions but with ctx.
func (c *Client) ListWithOptionsAndContext(ctx context.Context, path string, opts ListOptions) (*ListResp, error) {
	if opts.Page == 0 {
		opts.Page = 1
	}
	if opts.PerPage == 0 {
		opts.PerPage = 100
	}
	r, err := c.doJSONWithContext(ctx, "POST", "/api/fs/list", map[string]any{
		"path":     path,
		"password": opts.Password,
		"page":     opts.Page,
		"per_page": opts.PerPage,
		"refresh":  opts.Refresh,
	}, true)
	if err != nil {
		return nil, err
	}
	var data ListResp
	if err := json.Unmarshal(r.Data, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

// Exists checks if remotePath exists (file or dir) via List of parent.
func (c *Client) Exists(remotePath string) (bool, error) {
	return c.ExistsWithContext(context.Background(), remotePath)
}

// ExistsWithContext checks existence with ctx.
func (c *Client) ExistsWithContext(ctx context.Context, remotePath string) (bool, error) {
	remotePath = filepath.ToSlash(remotePath)
	remotePath = strings.TrimRight(remotePath, "/")
	if remotePath == "" || remotePath == "/" {
		return true, nil
	}
	dir := filepath.ToSlash(filepath.Dir(remotePath))
	base := filepath.Base(remotePath)
	// List parent dir and look for base
	resp, err := c.ListWithContext(ctx, dir)
	if err != nil {
		// if parent doesn't exist, then file doesn't exist
		if strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(strings.ToLower(err.Error()), "object not found") {
			return false, nil
		}
		return false, err
	}
	for _, it := range resp.Content {
		if it.Name == base {
			return true, nil
		}
	}
	return false, nil
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
	Content  []ListItem `json:"content"`
	Total    int        `json:"total"`
	Readme   string     `json:"readme"`
	Header   string     `json:"header"`
	Write    bool       `json:"write"`
	Provider string     `json:"provider"`
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
	return c.ListAllWithContext(context.Background(), path, perPage, refresh)
}

// ListAllWithContext is like ListAll but with ctx.
func (c *Client) ListAllWithContext(ctx context.Context, path string, perPage int, refresh bool) ([]ListItem, error) {
	if perPage <= 0 {
		perPage = 100
	}
	var all []ListItem
	page := 1
	for {
		resp, err := c.ListWithOptionsAndContext(ctx, path, ListOptions{Page: page, PerPage: perPage, Refresh: refresh})
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
// maxDepth <=0 means unlimited.
func (c *Client) ListRecursive(root string, perPage int, refresh bool) (map[string][]ListItem, error) {
	return c.ListRecursiveWithDepth(root, perPage, refresh, 0)
}

func (c *Client) ListRecursiveWithDepth(root string, perPage int, refresh bool, maxDepth int) (map[string][]ListItem, error) {
	type entry struct {
		dir   string
		depth int
	}
	result := make(map[string][]ListItem)
	queue := []entry{{dir: root, depth: 0}}
	visited := make(map[string]bool)
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if visited[e.dir] {
			continue
		}
		visited[e.dir] = true
		items, err := c.ListAll(e.dir, perPage, refresh)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", e.dir, err)
		}
		result[e.dir] = items
		if maxDepth > 0 && e.depth >= maxDepth {
			continue
		}
		for _, it := range items {
			if it.IsDir {
				sub := strings.TrimRight(e.dir, "/") + "/" + it.Name
				queue = append(queue, entry{dir: sub, depth: e.depth + 1})
			}
		}
	}
	return result, nil
}

// ---------- put stream ----------

// PutStreamOptions controls PUT /api/fs/put streaming upload.
type PutStreamOptions struct {
	AsTask      bool
	ContentType string // default application/octet-stream
	Overwrite   bool   // reserved, Alist always overwrites
}

// PutStream streams r to remotePath via PUT /api/fs/put.
// size <0 means unknown (chunked).
// For non-seekable readers, 401 retry is not possible — caller must handle re-login.
// For seekable readers (io.Seeker), 401 auto-retry rewinds and retries once.
// ctx may be nil (uses Background).
func (c *Client) PutStream(ctx context.Context, r io.Reader, size int64, remotePath string, opts PutStreamOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return c.putStreamWithRetry(ctx, r, size, remotePath, opts, true)
}

func (c *Client) putStreamWithRetry(ctx context.Context, r io.Reader, size int64, remotePath string, opts PutStreamOptions, allowRetry bool) error {
	if opts.ContentType == "" {
		opts.ContentType = "application/octet-stream"
	}
	parent := filepath.ToSlash(filepath.Dir(remotePath))
	if parent != "/" && parent != "." {
		if err := c.EnsureDirWithContext(ctx, parent); err != nil {
			return fmt.Errorf("ensure dir %s: %w", parent, err)
		}
	}
	encodedPath := encodeFilePath(remotePath)

	// wrap reader for Close if needed
	var rc io.ReadCloser
	if closer, ok := r.(io.ReadCloser); ok {
		rc = closer
	} else {
		rc = io.NopCloser(r)
	}

	req, err := http.NewRequestWithContext(ctx, "PUT", c.Host+"/api/fs/put", rc)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", c.getToken())
	req.Header.Set("File-Path", encodedPath)
	req.Header.Set("Content-Type", opts.ContentType)
	if opts.AsTask {
		req.Header.Set("As-Task", "true")
	} else {
		req.Header.Set("As-Task", "false")
	}

	resp, err := c.uploadClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("upload read body: %w status=%d", err, resp.StatusCode)
	}
	var ar apiResp
	if err := json.Unmarshal(b, &ar); err != nil {
		return fmt.Errorf("upload decode: %w body=%s status=%d", err, string(b), resp.StatusCode)
	}
	if ar.Code != 200 {
		if allowRetry && isUnauthorizedCode(ar.Code, ar.Message) && c.canRelogin() {
			logf("token expired, re-logging in ...\n")
			if err := c.relogin(ctx); err != nil {
				return fmt.Errorf("upload failed code=%d msg=%s (re-login failed: %v)", ar.Code, ar.Message, err)
			}
			if seeker, ok := r.(io.Seeker); ok {
				if _, err := seeker.Seek(0, io.SeekStart); err != nil {
					return fmt.Errorf("upload failed code=%d msg=%s (seek for retry failed: %v)", ar.Code, ar.Message, err)
				}
				return c.putStreamWithRetry(ctx, r, size, remotePath, opts, false)
			}
			return fmt.Errorf("upload failed code=%d msg=%s (cannot retry non-seekable stream, re-login succeeded — retry manually)", ar.Code, ar.Message)
		}
		return fmt.Errorf("upload failed code=%d msg=%s body=%s", ar.Code, ar.Message, string(b))
	}
	return nil
}

// PutFile is a convenience wrapper for PutStream with a local file.
// ctx may be nil (uses Background).
func (c *Client) PutFile(ctx context.Context, localPath, remotePath string, opts PutStreamOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return c.PutStream(ctx, f, fi.Size(), remotePath, opts)
}

// PutFolder uploads a local folder to remotePath.
// If asZip is true, the folder is zipped and streamed via PutStream (chunked, no temp file).
// Otherwise it uploads file-by-file via UploadDirConcurrent with concurrency.
func (c *Client) PutFolder(ctx context.Context, localDir, remotePath string, opts PutStreamOptions, asZip bool, concurrency int) error {
	return c.PutFolderWithOptions(ctx, localDir, remotePath, opts, asZip, UploadDirOptions{Concurrency: concurrency})
}

// PutFolderWithOptions is like PutFolder but supports SkipErrors for file-by-file mode.
func (c *Client) PutFolderWithOptions(ctx context.Context, localDir, remotePath string, opts PutStreamOptions, asZip bool, dirOpts UploadDirOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	fi, err := os.Stat(localDir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return c.PutFile(ctx, localDir, remotePath, opts)
	}
	if asZip {
		if !strings.HasSuffix(strings.ToLower(remotePath), ".zip") {
			if strings.HasSuffix(remotePath, "/") {
				remotePath = remotePath + filepath.Base(strings.TrimRight(localDir, "/")) + ".zip"
			} else if !strings.Contains(filepath.Base(remotePath), ".") {
				remotePath += ".zip"
			}
		}
		return c.putFolderAsZipStream(ctx, localDir, remotePath, opts)
	}
	_, _, failed, err := c.UploadDirConcurrentWithOptions(ctx, localDir, remotePath, dirOpts)
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("put folder completed with %d errors (skipped)", len(failed))
	}
	return nil
}

func (c *Client) putFolderAsZipStream(ctx context.Context, localDir, remotePath string, opts PutStreamOptions) error {
	// For 401 retry, pipe is non-seekable. So we handle retry at this level
	// by re-generating the zip stream.
	return c.putFolderAsZipStreamWithRetry(ctx, localDir, remotePath, opts, true)
}

func (c *Client) putFolderAsZipStreamWithRetry(ctx context.Context, localDir, remotePath string, opts PutStreamOptions, allowRetry bool) error {
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		err := pack.ZipFolderToWriter(ctx, localDir, pw)
		if err != nil {
			pw.CloseWithError(err)
			errCh <- err
			return
		}
		pw.Close()
		errCh <- nil
	}()

	putErr := c.putStreamWithRetry(ctx, pr, -1, remotePath, opts, allowRetry)
	if putErr != nil {
		pr.Close()
	}
	zipErr := <-errCh
	// Pipe is non-seekable: putStreamWithRetry already re-logged in and returned
	// "cannot retry non-seekable" on 401. Retry whole zip with fresh pipe.
	if putErr != nil && allowRetry && strings.Contains(putErr.Error(), "cannot retry non-seekable") {
		return c.putFolderAsZipStreamWithRetry(ctx, localDir, remotePath, opts, false)
	}
	if putErr != nil {
		return putErr
	}
	return zipErr
}

// ---------- upload (file/folder) ----------

// UploadFile streams local file to remotePath via PUT /api/fs/put.
// Alist expects headers: Authorization, File-Path (url-encoded), As-Task, Content-Type.
// If the token is expired (401), it auto re-logins using Username/Password and retries once.
func (c *Client) UploadFile(localPath, remotePath string, asTask bool, overwrite bool) error {
	return c.UploadFileWithContext(context.Background(), localPath, remotePath, asTask, overwrite)
}

func (c *Client) UploadFileWithContext(ctx context.Context, localPath, remotePath string, asTask bool, overwrite bool) error {
	return c.uploadFileWithRetry(ctx, localPath, remotePath, asTask, overwrite, true)
}

func (c *Client) uploadFileWithRetry(ctx context.Context, localPath, remotePath string, asTask bool, overwrite bool, allowRetry bool) error {
	fi, err := os.Stat(localPath)
	if err != nil {
		return err
	}

	parent := filepath.ToSlash(filepath.Dir(remotePath))
	if parent != "/" && parent != "." {
		if err := c.EnsureDirWithContext(ctx, parent); err != nil {
			return fmt.Errorf("ensure dir %s: %w", parent, err)
		}
	}

	encodedPath := encodeFilePath(remotePath)

	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	shouldClose := true
	defer func() {
		if shouldClose {
			f.Close()
		}
	}()

	req, err := http.NewRequestWithContext(ctx, "PUT", c.Host+"/api/fs/put", f)
	if err != nil {
		return err
	}
	req.ContentLength = fi.Size()
	req.Header.Set("Authorization", c.getToken())
	req.Header.Set("File-Path", encodedPath)
	req.Header.Set("Content-Type", "application/octet-stream")
	if asTask {
		req.Header.Set("As-Task", "true")
	} else {
		req.Header.Set("As-Task", "false")
	}

	var body io.ReadCloser = f
	if !asTask && fi.Size() > 0 {
		pr := &progressReader{Reader: f, total: fi.Size(), name: filepath.Base(localPath), start: time.Now()}
		body = pr
	}
	req.Body = body

	resp, err := c.uploadClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("upload read body: %w status=%d", err, resp.StatusCode)
	}
	var r apiResp
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("upload decode: %w body=%s status=%d", err, string(b), resp.StatusCode)
	}
	if r.Code != 200 {
		if allowRetry && isUnauthorizedCode(r.Code, r.Message) && c.canRelogin() {
			logf("token expired, re-logging in ...\n")
			if err := c.relogin(ctx); err != nil {
				return fmt.Errorf("upload failed code=%d msg=%s (re-login failed: %v)", r.Code, r.Message, err)
			}
			shouldClose = false
			f.Close()
			return c.uploadFileWithRetry(ctx, localPath, remotePath, asTask, overwrite, false)
		}
		return fmt.Errorf("upload failed code=%d msg=%s body=%s", r.Code, r.Message, string(b))
	}
	logf("\n")
	return nil
}

// UploadDirRecursive uploads folder file-by-file preserving structure.
func (c *Client) UploadDirRecursive(localDir, remoteDir string, asTask bool) (int, int64, error) {
	return c.UploadDirRecursiveWithContext(context.Background(), localDir, remoteDir, asTask)
}

func (c *Client) UploadDirRecursiveWithContext(ctx context.Context, localDir, remoteDir string, asTask bool) (int, int64, error) {
	return c.UploadDirConcurrentWithContext(ctx, localDir, remoteDir, asTask, 1)
}

// UploadDirConcurrent uploads folder file-by-file with concurrency.
// concurrency <=1 means sequential.
func (c *Client) UploadDirConcurrent(localDir, remoteDir string, asTask bool, concurrency int) (int, int64, error) {
	return c.UploadDirConcurrentWithContext(context.Background(), localDir, remoteDir, asTask, concurrency)
}

func (c *Client) UploadDirConcurrentWithContext(ctx context.Context, localDir, remoteDir string, asTask bool, concurrency int) (int, int64, error) {
	count, total, failed, err := c.UploadDirConcurrentWithOptions(ctx, localDir, remoteDir, UploadDirOptions{AsTask: asTask, Concurrency: concurrency})
	if err != nil {
		return count, total, err
	}
	if len(failed) > 0 {
		return count, total, failed[0].Err
	}
	return count, total, nil
}

// UploadDirConcurrentWithOptions uploads folder with full options including SkipErrors.
// Returns count, total bytes, failed files, and fatal error (nil if SkipErrors and only per-file failures).
func (c *Client) UploadDirConcurrentWithOptions(ctx context.Context, localDir, remoteDir string, opts UploadDirOptions) (int, int64, []UploadError, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	concurrency := opts.Concurrency
	if concurrency <= 1 {
		return c.uploadDirSequentialWithOptions(ctx, localDir, remoteDir, opts)
	}

	var jobs []job
	if err := filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
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
		jobs = append(jobs, job{localPath: path, remotePath: remotePath, size: info.Size(), rel: rel})
		return nil
	}); err != nil {
		return 0, 0, nil, err
	}
	if len(jobs) == 0 {
		return 0, 0, nil, nil
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].rel < jobs[j].rel })

	if err := c.EnsureDirWithContext(ctx, remoteDir); err != nil {
		return 0, 0, nil, fmt.Errorf("ensure dir %s: %w", remoteDir, err)
	}
	// Collect unique parent dirs, including all ancestors to dedup EnsureDir calls.
	seen := make(map[string]bool)
	for _, j := range jobs {
		dir := filepath.ToSlash(filepath.Dir(j.remotePath))
		for dir != "/" && dir != "." && dir != remoteDir {
			if !seen[dir] {
				seen[dir] = true
			}
			dir = filepath.ToSlash(filepath.Dir(dir))
		}
	}
	// Also ensure remoteDir itself already done; seen contains subdirs only.
	var dirs []string
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.Count(dirs[i], "/") < strings.Count(dirs[j], "/") })
	for _, d := range dirs {
		if err := c.EnsureDirWithContext(ctx, d); err != nil {
			return 0, 0, nil, fmt.Errorf("ensure dir %s: %w", d, err)
		}
	}

	// Filter SkipExisting after EnsureDir so parent dirs exist and List is authoritative.
	// Track which parent dirs were successfully listed to avoid redundant per-file Exists.
	var okDirs map[string]bool
	if opts.SkipExisting {
		origCount := len(jobs)
		var skipped int
		jobs, skipped, okDirs = c.filterExistingJobs(ctx, jobs)
		if len(jobs) == 0 {
			logf("all %d files already exist, skipping\n", origCount)
			return 0, 0, nil, nil
		}
		if skipped > 0 {
			logf("skip-existing: %d/%d files already exist, %d remaining\n", skipped, origCount, len(jobs))
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobCh := make(chan job)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	var count atomic.Int64
	var total atomic.Int64
	var completed atomic.Int64
	var skipped atomic.Int64
	var failedMu sync.Mutex
	var failed []UploadError

	worker := func() {
		defer wg.Done()
		for j := range jobCh {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// SkipExisting fallback per file: only when batch List for this parent dir failed.
			if opts.SkipExisting && okDirs != nil {
				dir := filepath.ToSlash(filepath.Dir(j.remotePath))
				if !okDirs[dir] {
					if exists, err := c.ExistsWithContext(ctx, j.remotePath); err == nil && exists {
						n := completed.Add(1)
						skipped.Add(1)
						logf("  [%d/%d] %s -> %s exists, skipped\n", n, len(jobs), j.rel, j.remotePath)
						continue
					}
				}
			}
			if err := c.uploadFileDirect(ctx, j.localPath, j.remotePath, opts.AsTask); err != nil {
				if opts.SkipErrors {
					failedMu.Lock()
					failed = append(failed, UploadError{Rel: j.rel, Err: err})
					failedMu.Unlock()
					n := completed.Add(1)
					logf("  [%d/%d] %s -> %s FAILED: %v (skipped)\n", n, len(jobs), j.rel, j.remotePath, err)
					continue
				}
				select {
				case errCh <- fmt.Errorf("upload %s: %w", j.rel, err):
				default:
				}
				cancel()
				return
			}
			count.Add(1)
			total.Add(j.size)
			n := completed.Add(1)
			logf("  [%d/%d] %s -> %s (%d bytes) done\n", n, len(jobs), j.rel, j.remotePath, j.size)
		}
	}

	if concurrency > len(jobs) {
		concurrency = len(jobs)
	}
	if concurrency > 32 {
		concurrency = 32
	}
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go worker()
	}

	go func() {
		defer close(jobCh)
		for _, j := range jobs {
			select {
			case <-ctx.Done():
				return
			case jobCh <- j:
			}
		}
	}()

	wg.Wait()
	if err := ctx.Err(); err != nil && err != context.Canceled {
		return int(count.Load()), total.Load(), failed, err
	}
	select {
	case err := <-errCh:
		return int(count.Load()), total.Load(), failed, err
	default:
		return int(count.Load()), total.Load(), failed, nil
	}
}

// filterExistingJobs batches List calls per parent dir to filter out existing files.
// Returns filtered jobs, number skipped, and set of dirs that were successfully listed.
func (c *Client) filterExistingJobs(ctx context.Context, jobs []job) ([]job, int, map[string]bool) {
	byDir := make(map[string][]int)
	for i, j := range jobs {
		dir := filepath.ToSlash(filepath.Dir(j.remotePath))
		byDir[dir] = append(byDir[dir], i)
	}
	existsSet := make(map[int]bool)
	okDirs := make(map[string]bool)
	for dir, idxs := range byDir {
		select {
		case <-ctx.Done():
			return jobs, 0, nil
		default:
		}
		// Use ListAll to handle pagination (perPage 100 may miss files)
		items, err := c.ListAllWithContext(ctx, dir, 200, false)
		if err != nil {
			continue
		}
		okDirs[dir] = true
		nameSet := make(map[string]bool, len(items))
		for _, it := range items {
			nameSet[it.Name] = true
		}
		for _, idx := range idxs {
			base := filepath.Base(jobs[idx].remotePath)
			if nameSet[base] {
				existsSet[idx] = true
			}
		}
	}
	if len(existsSet) == 0 {
		return jobs, 0, okDirs
	}
	filtered := make([]job, 0, len(jobs)-len(existsSet))
	for i, j := range jobs {
		if existsSet[i] {
			logf("  %s -> %s exists, skipped\n", j.rel, j.remotePath)
			continue
		}
		filtered = append(filtered, j)
	}
	return filtered, len(existsSet), okDirs
}

func (c *Client) uploadDirSequential(ctx context.Context, localDir, remoteDir string, asTask bool) (int, int64, error) {
	count, total, failed, err := c.uploadDirSequentialWithOptions(ctx, localDir, remoteDir, UploadDirOptions{AsTask: asTask})
	if err != nil {
		return count, total, err
	}
	if len(failed) > 0 {
		return count, total, failed[0].Err
	}
	return count, total, nil
}

func (c *Client) uploadDirSequentialWithOptions(ctx context.Context, localDir, remoteDir string, opts UploadDirOptions) (int, int64, []UploadError, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var count int
	var total int64
	var failed []UploadError
	var skipped int
	err := filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		remotePath := strings.TrimRight(remoteDir, "/") + "/" + rel
		if opts.SkipExisting {
			exists, err := c.ExistsWithContext(ctx, remotePath)
			if err == nil && exists {
				skipped++
				logf("  [%d] %s -> %s exists, skipped\n", count+len(failed)+skipped, rel, remotePath)
				return nil
			}
		}
		logf("  [%d] %s -> %s (%d bytes)\n", count+len(failed)+skipped+1, rel, remotePath, info.Size())
		if err := c.UploadFileWithContext(ctx, path, remotePath, opts.AsTask, true); err != nil {
			if opts.SkipErrors {
				logf("  %s FAILED: %v (skipped)\n", rel, err)
				failed = append(failed, UploadError{Rel: rel, Err: err})
				return nil
			}
			return fmt.Errorf("upload %s: %w", rel, err)
		}
		count++
		total += info.Size()
		return nil
	})
	return count, total, failed, err
}

// uploadFileDirect uploads without EnsureDir (caller ensures dirs).
func (c *Client) uploadFileDirect(ctx context.Context, localPath, remotePath string, asTask bool) error {
	fi, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	encodedPath := encodeFilePath(remotePath)
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	shouldClose := true
	defer func() {
		if shouldClose {
			f.Close()
		}
	}()
	req, err := http.NewRequestWithContext(ctx, "PUT", c.Host+"/api/fs/put", f)
	if err != nil {
		return err
	}
	req.ContentLength = fi.Size()
	req.Header.Set("Authorization", c.getToken())
	req.Header.Set("File-Path", encodedPath)
	req.Header.Set("Content-Type", "application/octet-stream")
	if asTask {
		req.Header.Set("As-Task", "true")
	} else {
		req.Header.Set("As-Task", "false")
	}
	var body io.ReadCloser = f
	if !asTask && fi.Size() > 0 {
		body = f
	}
	req.Body = body
	resp, err := c.uploadClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("upload read body: %w status=%d", err, resp.StatusCode)
	}
	var r apiResp
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("upload decode: %w body=%s status=%d", err, string(b), resp.StatusCode)
	}
	if r.Code != 200 {
		if isUnauthorizedCode(r.Code, r.Message) && c.canRelogin() {
			logf("token expired, re-logging in ...\n")
			if err := c.relogin(ctx); err != nil {
				return fmt.Errorf("upload failed code=%d msg=%s (re-login failed: %v)", r.Code, r.Message, err)
			}
			shouldClose = false
			f.Close()
			return c.uploadFileDirect(ctx, localPath, remotePath, asTask)
		}
		return fmt.Errorf("upload failed code=%d msg=%s body=%s", r.Code, r.Message, string(b))
	}
	return nil
}

func encodeFilePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if s == "" {
			continue
		}
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// progressReader prints upload progress to stderr.
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
			stderrMu.Lock()
			fmt.Fprintf(os.Stderr, "\r  %s: %d%% (%d/%d)", pr.name, pct, pr.read, pr.total)
			stderrMu.Unlock()
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
