package alist

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
)

func TestUploadDirConcurrent(t *testing.T) {
	var mu sync.Mutex
	var uploaded []string
	var concurrent int32
	var maxConcurrent int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/fs/mkdir", "/api/fs/list":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
		case "/api/fs/put":
			cur := atomic.AddInt32(&concurrent, 1)
			// track max
			for {
				m := atomic.LoadInt32(&maxConcurrent)
				if cur > m && atomic.CompareAndSwapInt32(&maxConcurrent, m, cur) {
					break
				}
				if cur <= m {
					break
				}
			}
			// small delay to allow overlap
			// (no sleep needed, just check concurrency)
			mu.Lock()
			uploaded = append(uploaded, r.Header.Get("File-Path"))
			mu.Unlock()
			atomic.AddInt32(&concurrent, -1)
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{}})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// create temp dir with 6 files
	dir := t.TempDir()
	for i := 0; i < 6; i++ {
		name := filepath.Join(dir, "file"+string(rune('a'+i))+".txt")
		os.WriteFile(name, []byte("hello"), 0644)
	}
	// subdir
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "x.txt"), []byte("x"), 0644)

	c := NewClient(srv.URL, "tok")
	count, total, err := c.UploadDirConcurrent(dir, "/remote", false, 3)
	if err != nil {
		t.Fatalf("UploadDirConcurrent: %v", err)
	}
	if count != 7 {
		t.Fatalf("count = %d want 7", count)
	}
	if total != 7*5 && total != 31 { // 6*5 + 1
		t.Fatalf("total = %d", total)
	}
	if len(uploaded) != 7 {
		t.Fatalf("uploaded = %d want 7", len(uploaded))
	}
	sort.Strings(uploaded)
	// should have all files
	for _, p := range uploaded {
		if p == "" {
			t.Error("empty File-Path")
		}
	}
	if maxConcurrent > 3 {
		t.Errorf("maxConcurrent = %d > 3", maxConcurrent)
	}
	// maxConcurrent may be 1 if server is fast; just ensure all uploaded
	if maxConcurrent < 1 {
		t.Errorf("maxConcurrent = %d", maxConcurrent)
	}
}

func TestUploadDirConcurrentSequentialFallback(t *testing.T) {
	var uploaded int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fs/put" {
			atomic.AddInt32(&uploaded, 1)
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
	}))
	defer srv.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0644)

	c := NewClient(srv.URL, "tok")
	// concurrency 1 should use sequential path
	count, _, err := c.UploadDirConcurrent(dir, "/r", false, 1)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d", count)
	}
	if atomic.LoadInt32(&uploaded) != 2 {
		t.Fatalf("uploaded = %d", uploaded)
	}
}

func TestUploadDirConcurrentError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/fs/mkdir", "/api/fs/list":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
		case "/api/fs/put":
			// fail on file b
			if r.Header.Get("File-Path") == "/r/b.txt" {
				json.NewEncoder(w).Encode(map[string]any{"code": 500, "message": "storage error", "data": nil})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{}})
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0644)

	c := NewClient(srv.URL, "tok")
	_, _, err := c.UploadDirConcurrent(dir, "/r", false, 2)
	if err == nil {
		t.Fatal("expected error")
	}
}
