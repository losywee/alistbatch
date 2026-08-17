package alist

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestPutFolderAsZip(t *testing.T) {
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/fs/mkdir", "/api/fs/list":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
		case "/api/fs/put":
			gotPath = r.Header.Get("File-Path")
			b, _ := io.ReadAll(r.Body)
			gotBody = b
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{}})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0644)
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "b.txt"), []byte("world"), 0644)

	c := NewClient(srv.URL, "tok")
	if err := c.PutFolder(context.Background(), dir, "/remote/archive.zip", PutStreamOptions{}, true, 0); err != nil {
		t.Fatalf("PutFolder zip: %v", err)
	}
	if gotPath != "/remote/archive.zip" {
		t.Errorf("File-Path = %q want /remote/archive.zip", gotPath)
	}
	// verify zip content
	zr, err := zip.NewReader(bytes.NewReader(gotBody), int64(len(gotBody)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	if len(zr.File) < 2 {
		t.Fatalf("zip files = %d want >=2", len(zr.File))
	}
}

func TestPutFolderConcurrent(t *testing.T) {
	var putCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/fs/mkdir", "/api/fs/list":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
		case "/api/fs/put":
			atomic.AddInt32(&putCount, 1)
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{}})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(dir, "f"+string(rune('a'+i))+".txt"), []byte("x"), 0644)
	}

	c := NewClient(srv.URL, "tok")
	if err := c.PutFolder(context.Background(), dir, "/remote/dir", PutStreamOptions{}, false, 3); err != nil {
		t.Fatalf("PutFolder concurrent: %v", err)
	}
	if atomic.LoadInt32(&putCount) != 5 {
		t.Fatalf("putCount = %d want 5", putCount)
	}
}

func TestPutFolderSingleFile(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fs/mkdir" || r.URL.Path == "/api/fs/list" {
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
			return
		}
		gotPath = r.Header.Get("File-Path")
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{}})
	}))
	defer srv.Close()

	dir := t.TempDir()
	p := filepath.Join(dir, "single.txt")
	os.WriteFile(p, []byte("hi"), 0644)

	c := NewClient(srv.URL, "tok")
	if err := c.PutFolder(context.Background(), p, "/remote/single.txt", PutStreamOptions{}, false, 4); err != nil {
		t.Fatalf("PutFolder single: %v", err)
	}
	if gotPath != "/remote/single.txt" {
		t.Errorf("File-Path = %q", gotPath)
	}
}
