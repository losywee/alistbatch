package pack

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestZipFolderPipe(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0644)
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "b.txt"), []byte("world"), 0644)

	pr, err := ZipFolderPipe(context.Background(), dir)
	if err != nil {
		t.Fatalf("ZipFolderPipe: %v", err)
	}
	defer pr.Close()

	b, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	names := make(map[string]bool)
	for _, f := range zr.File {
		names[f.Name] = true
	}
	base := filepath.Base(dir)
	if !names[base+"/a.txt"] {
		t.Errorf("missing %s/a.txt, got %v", base, names)
	}
	if !names[base+"/sub/b.txt"] {
		t.Errorf("missing %s/sub/b.txt", base)
	}
}

func TestZipFolderToWriter(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0644)

	var buf bytes.Buffer
	if err := ZipFolderToWriter(context.Background(), dir, &buf); err != nil {
		t.Fatalf("ZipFolderToWriter: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty zip")
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	if len(zr.File) == 0 {
		t.Fatal("no files in zip")
	}
}
