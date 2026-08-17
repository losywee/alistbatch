package alist

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLoginAndUpload(t *testing.T) {
	var gotLogin bool
	var gotPutPath string
	var gotPutBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			gotLogin = true
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			if req["username"] != "admin" || req["password"] != "123" {
				t.Errorf("login creds = %v", req)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"code": 200, "message": "success",
				"data": map[string]string{"token": "test-token"},
			})
		case "/api/fs/mkdir":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": nil})
		case "/api/fs/list":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"content": []any{}, "total": 0}})
		case "/api/fs/put":
			gotPutPath = r.Header.Get("File-Path")
			b, _ := io.ReadAll(r.Body)
			gotPutBody = string(b)
			if r.Header.Get("Authorization") != "test-token" {
				t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": map[string]any{"task": map[string]any{}}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	if err := c.Login("admin", "123"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if !gotLogin {
		t.Fatal("login not called")
	}
	if c.Token != "test-token" {
		t.Fatalf("token = %q", c.Token)
	}

	// create temp file
	tmp := filepath.Join(t.TempDir(), "hello.txt")
	os.WriteFile(tmp, []byte("hello world"), 0644)

	if err := c.UploadFile(tmp, "/a/b/hello.txt", false, true); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if gotPutPath != "/a/b/hello.txt" {
		t.Errorf("File-Path = %q want /a/b/hello.txt", gotPutPath)
	}
	if gotPutBody != "hello world" {
		t.Errorf("body = %q", gotPutBody)
	}
}

func TestEncodeFilePath(t *testing.T) {
	if got := encodeFilePath("/a/b c/d.txt"); got != "/a/b%20c/d.txt" {
		t.Errorf("encode = %q", got)
	}
}
