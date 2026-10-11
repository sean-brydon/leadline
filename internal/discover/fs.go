package discover

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// files is every way this package touches the disk. It is a struct of
// functions so a test can stand in front of the real ones and note each
// path that is opened: "nothing outside the given folders is read" is
// checked there, not taken on trust.
type files struct {
	readDir  func(dir string) ([]fs.DirEntry, error)
	readFile func(path string, limit int64) ([]byte, error)
	lstat    func(path string) (fs.FileInfo, error)
}

func osFiles() files {
	return files{
		readDir: os.ReadDir,
		readFile: func(path string, limit int64) ([]byte, error) {
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
				// A FIFO named .env would block for ever.
				return nil, errors.New("not a regular file")
			}
			return io.ReadAll(io.LimitReader(f, limit))
		},
		lstat: os.Lstat,
	}
}

const (
	// A project file is small. One that is not is not read at all past
	// this, which keeps a scan from being slowed by a stray huge file.
	maxProjectFile = 1 << 20
	maxEnvFile     = 256 << 10
)

// exists reports whether path is there, without following a final link.
func (f files) exists(path string) bool {
	_, err := f.lstat(path)
	return err == nil
}

// isDir reports whether path is a real directory (not a link to one).
func (f files) isDir(path string) bool {
	st, err := f.lstat(path)
	return err == nil && st.IsDir()
}

// within reports whether path is dir or inside it.
func within(dir, path string) bool {
	if dir == path {
		return true
	}
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		dir += string(filepath.Separator)
	}
	return strings.HasPrefix(path, dir)
}
