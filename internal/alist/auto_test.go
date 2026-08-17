package alist

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestAutoReloginOnList(t *testing.T) {
	var loginCount int32
	var listCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			atomic.AddInt32(&loginCount, 1)
			json.NewEncoder(w).Encode(map[string]any{
				"code": 200, "message": "success",
				"data": map[string]string{"token": "new-token"},
			})
		case "/api/fs/list":
			n := atomic.AddInt32(&listCount, 1)
			if n == 1 {
				// first call: simulate expired token
				json.NewEncoder(w).Encode(map[string]any{
					"code": 401, "message": "unauthorized",
					"data": nil,
				})
				return
			}
			// second call: success
			if r.Header.Get("Authorization") != "new-token" {
				t.Errorf("expected new-token, got %q", r.Header.Get("Authorization"))
			}
			json.NewEncoder(w).Encode(map[string]any{
				"code": 200, "message": "success",
				"data": map[string]any{
					"content": []map[string]any{
						{"name": "a.txt", "is_dir": false, "size": 10},
					},
					"total": 1,
				},
			})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "expired-token")
	c.SetCredentials("admin", "123")
	var refreshed string
	c.OnTokenRefresh = func(tok string) { refreshed = tok }

	resp, err := c.List("/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Name != "a.txt" {
		t.Fatalf("content = %+v", resp.Content)
	}
	if atomic.LoadInt32(&loginCount) != 1 {
		t.Fatalf("loginCount = %d want 1", loginCount)
	}
	if refreshed != "new-token" {
		t.Fatalf("refreshed = %q want new-token", refreshed)
	}
	if c.Token != "new-token" {
		t.Fatalf("token = %q want new-token", c.Token)
	}
}

func TestAutoReloginOnUpload(t *testing.T) {
	var loginCount int32
	var putCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			atomic.AddInt32(&loginCount, 1)
			json.NewEncoder(w).Encode(map[string]any{
				"code": 200, "message": "success",
				"data": map[string]string{"token": "new-token"},
			})
		case "/api/fs/mkdir":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": nil})
		case "/api/fs/list":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
		case "/api/fs/put":
			n := atomic.AddInt32(&putCount, 1)
			if n == 1 {
				// expired
				json.NewEncoder(w).Encode(map[string]any{"code": 401, "message": "token expired", "data": nil})
				return
			}
			if r.Header.Get("Authorization") != "new-token" {
				t.Errorf("expected new-token, got %q", r.Header.Get("Authorization"))
			}
			b, _ := io.ReadAll(r.Body)
			if string(b) != "hello" {
				t.Errorf("body = %q", string(b))
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{}})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "expired-token")
	c.SetCredentials("admin", "123")
	var refreshed string
	c.OnTokenRefresh = func(tok string) { refreshed = tok }

	tmp := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(tmp, []byte("hello"), 0644)

	if err := c.UploadFile(tmp, "/a/f.txt", false, true); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if atomic.LoadInt32(&loginCount) != 1 {
		t.Fatalf("loginCount = %d", loginCount)
	}
	if refreshed != "new-token" {
		t.Fatalf("refreshed = %q", refreshed)
	}
}

func TestNoReloginWithoutCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fs/list" {
			json.NewEncoder(w).Encode(map[string]any{"code": 401, "message": "unauthorized", "data": nil})
			return
		}
		t.Errorf("unexpected %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "expired")
	// no credentials set
	_, err := c.List("/")
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsUnauthorized(err) {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}
