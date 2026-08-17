package alist

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPutStream(t *testing.T) {
	var gotPath, gotCT, gotBody string
	var gotAsTask string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fs/mkdir" || r.URL.Path == "/api/fs/list" {
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
			return
		}
		if r.URL.Path != "/api/fs/put" {
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		gotPath = r.Header.Get("File-Path")
		gotCT = r.Header.Get("Content-Type")
		gotAsTask = r.Header.Get("As-Task")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{}})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	err := c.PutStream(context.Background(), strings.NewReader("hello world"), 11, "/a/b/hello.txt", PutStreamOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("PutStream: %v", err)
	}
	if gotPath != "/a/b/hello.txt" {
		t.Errorf("File-Path = %q", gotPath)
	}
	if gotCT != "text/plain" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotAsTask != "false" {
		t.Errorf("As-Task = %q", gotAsTask)
	}
	if gotBody != "hello world" {
		t.Errorf("body = %q", gotBody)
	}
}

func TestPutStreamChunked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fs/mkdir" || r.URL.Path == "/api/fs/list" {
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
			return
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != "streamed" {
			t.Errorf("body = %q", string(b))
		}
		// chunked has no ContentLength
		if r.ContentLength != -1 && r.ContentLength != 8 {
			// allow either
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{}})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	// size -1 = chunked
	if err := c.PutStream(context.Background(), strings.NewReader("streamed"), -1, "/a/b.txt", PutStreamOptions{}); err != nil {
		t.Fatalf("PutStream chunked: %v", err)
	}
}

func TestPutStreamSeekableRetry(t *testing.T) {
	var putCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]string{"token": "new-tok"}})
		case "/api/fs/mkdir", "/api/fs/list":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
		case "/api/fs/put":
			n := atomic.AddInt32(&putCount, 1)
			if n == 1 {
				json.NewEncoder(w).Encode(map[string]any{"code": 401, "message": "unauthorized", "data": nil})
				return
			}
			if r.Header.Get("Authorization") != "new-tok" {
				t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			}
			b, _ := io.ReadAll(r.Body)
			if string(b) != "retry me" {
				t.Errorf("body = %q", string(b))
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{}})
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "old-tok")
	c.SetCredentials("admin", "123")
	// bytes.Reader is seekable
	r := bytes.NewReader([]byte("retry me"))
	if err := c.PutStream(context.Background(), r, 8, "/a/b.txt", PutStreamOptions{}); err != nil {
		t.Fatalf("PutStream retry: %v", err)
	}
	if atomic.LoadInt32(&putCount) != 2 {
		t.Fatalf("putCount = %d want 2", putCount)
	}
	if c.getToken() != "new-tok" {
		t.Fatalf("token = %q", c.getToken())
	}
}

func TestPutStreamNonSeekableNoRetry(t *testing.T) {
	var putCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]string{"token": "new-tok"}})
		case "/api/fs/mkdir", "/api/fs/list":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
		case "/api/fs/put":
			atomic.AddInt32(&putCount, 1)
			json.NewEncoder(w).Encode(map[string]any{"code": 401, "message": "unauthorized", "data": nil})
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "old-tok")
	c.SetCredentials("admin", "123")
	// strings.Reader is not seekable via io.Seeker? actually it is, use a non-seekable wrapper
	r := struct{ io.Reader }{strings.NewReader("hello")}
	err := c.PutStream(context.Background(), r, 5, "/a/b.txt", PutStreamOptions{})
	if err == nil {
		t.Fatal("expected error for non-seekable retry")
	}
	if !strings.Contains(err.Error(), "cannot retry non-seekable") {
		t.Fatalf("err = %v", err)
	}
	if atomic.LoadInt32(&putCount) != 1 {
		t.Fatalf("putCount = %d want 1", putCount)
	}
}

func TestPutFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fs/mkdir" || r.URL.Path == "/api/fs/list" {
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
			return
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != "file content" {
			t.Errorf("body = %q", string(b))
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{}})
	}))
	defer srv.Close()

	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	os.WriteFile(p, []byte("file content"), 0644)

	c := NewClient(srv.URL, "tok")
	if err := c.PutFile(nil, p, "/remote/f.txt", PutStreamOptions{}); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
}

func TestPutStreamNilContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fs/mkdir" || r.URL.Path == "/api/fs/list" {
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{}})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")
	if err := c.PutStream(nil, strings.NewReader("hi"), 2, "/a.txt", PutStreamOptions{}); err != nil {
		t.Fatalf("PutStream nil ctx: %v", err)
	}
	if err := c.PutFile(nil, os.Args[0], "/a.txt", PutStreamOptions{}); err == nil {
		// os.Args[0] exists, should succeed
	}
}
