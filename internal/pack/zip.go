package pack

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ZipFolder zips src (file or folder) into dst zip file.
func ZipFolder(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	zw := zip.NewWriter(out)
	defer zw.Close()

	if !fi.IsDir() {
		return AddFileToZip(zw, src, filepath.Base(src))
	}

	base := filepath.Base(strings.TrimRight(src, "/"))
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		zipPath := filepath.ToSlash(filepath.Join(base, rel))
		if info.IsDir() {
			if !strings.HasSuffix(zipPath, "/") {
				zipPath += "/"
			}
			_, err := zw.CreateHeader(&zip.FileHeader{
				Name:   zipPath,
				Method: zip.Store,
			})
			return err
		}
		return AddFileToZip(zw, path, zipPath)
	})
}

// AddFileToZip adds a single file to zw. Exported for reuse by alist client.
func AddFileToZip(zw *zip.Writer, filePath, zipPath string) error {
	fi, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	header, err := zip.FileInfoHeader(fi)
	if err != nil {
		return err
	}
	header.Name = zipPath
	header.Method = zip.Deflate
	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// ZipFolderToTemp zips folder to a temp file, returns path and cleanup func.
func ZipFolderToTemp(src string) (string, func(), error) {
	tmp, err := os.CreateTemp("", "alistbatch-*.zip")
	if err != nil {
		return "", nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	os.Remove(tmpPath)
	if err := ZipFolder(src, tmpPath); err != nil {
		os.Remove(tmpPath)
		return "", nil, err
	}
	cleanup := func() { os.Remove(tmpPath) }
	return tmpPath, cleanup, nil
}

// ZipFolderToWriter streams zip of src (file or folder) to w.
// It respects ctx cancellation.
func ZipFolderToWriter(ctx context.Context, src string, w io.Writer) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	defer zw.Close()

	if !fi.IsDir() {
		return AddFileToZip(zw, src, filepath.Base(src))
	}

	base := filepath.Base(strings.TrimRight(src, "/"))
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		zipPath := filepath.ToSlash(filepath.Join(base, rel))
		if info.IsDir() {
			if !strings.HasSuffix(zipPath, "/") {
				zipPath += "/"
			}
			_, err := zw.CreateHeader(&zip.FileHeader{
				Name:   zipPath,
				Method: zip.Store,
			})
			return err
		}
		return AddFileToZip(zw, path, zipPath)
	})
}

// ZipFolderPipe returns a reader that streams zip of src.
// The zip is generated in a goroutine via io.Pipe. Caller must read to EOF or close.
// ctx controls cancellation.
func ZipFolderPipe(ctx context.Context, src string) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	zw := zip.NewWriter(pw)

	go func() {
		// zw.Close must happen before pw.Close to flush central directory.
		// On error, CloseWithError propagates to reader.
		var walkErr error
		fi, err := os.Stat(src)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if !fi.IsDir() {
			walkErr = AddFileToZip(zw, src, filepath.Base(src))
		} else {
			base := filepath.Base(strings.TrimRight(src, "/"))
			walkErr = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				rel, err := filepath.Rel(src, path)
				if err != nil {
					return err
				}
				if rel == "." {
					return nil
				}
				zipPath := filepath.ToSlash(filepath.Join(base, rel))
				if info.IsDir() {
					if !strings.HasSuffix(zipPath, "/") {
						zipPath += "/"
					}
					_, err := zw.CreateHeader(&zip.FileHeader{
						Name:   zipPath,
						Method: zip.Store,
					})
					return err
				}
				return AddFileToZip(zw, path, zipPath)
			})
		}
		if err := zw.Close(); err != nil && walkErr == nil {
			walkErr = err
		}
		if walkErr != nil {
			pw.CloseWithError(walkErr)
			return
		}
		pw.Close()
	}()

	return pr, nil
}
