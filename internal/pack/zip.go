package pack

import (
	"archive/zip"
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
		return addFileToZip(zw, src, filepath.Base(src))
	}

	// folder: walk and add
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
		// store with top-level folder name
		zipPath := filepath.ToSlash(filepath.Join(base, rel))
		if info.IsDir() {
			// ensure dir entry ends with /
			if !strings.HasSuffix(zipPath, "/") {
				zipPath += "/"
			}
			_, err := zw.CreateHeader(&zip.FileHeader{
				Name:   zipPath,
				Method: zip.Store,
			})
			return err
		}
		return addFileToZip(zw, path, zipPath)
	})
}

func addFileToZip(zw *zip.Writer, filePath, zipPath string) error {
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
