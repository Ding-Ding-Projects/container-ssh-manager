package connection

import (
	"bytes"
	"context"
	"errors"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
	"github.com/pkg/sftp"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"
)

type FileEntry struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Directory bool      `json:"directory"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
}

// WriteComposeFile is intentionally narrower than a generic local writer. The
// engine supplies a project directory and one approved Compose basename.
func (m *Manager) WriteComposeFile(ctx context.Context, hostID, projectDir, base string, data []byte) error {
	if base != "compose.yaml" && base != "compose.yml" && base != ".env" {
		return errors.New("unsupported Compose file")
	}
	if len(data) > 16<<20 {
		return errors.New("file exceeds 16 MiB")
	}
	if hostID != "local" {
		if e := safeRemotePath(projectDir); e != nil {
			return e
		}
		return m.WriteFile(ctx, hostID, path.Join(projectDir, base), data)
	}
	if projectDir == "" || !filepath.IsAbs(projectDir) || filepath.Clean(projectDir) != projectDir {
		return errors.New("project directory required")
	}
	target := filepath.Join(projectDir, base)
	tmp, err := os.CreateTemp(projectDir, ".container-ssh-manager-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

func (m *Manager) withSFTP(ctx context.Context, hostID string, fn func(*sftp.Client) error) error {
	c, e := m.Dial(ctx, hostID)
	if e != nil {
		return e
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	s, e := sftp.NewClient(c)
	if e != nil {
		return e
	}
	defer s.Close()
	return fn(s)
}
func (m *Manager) ListFiles(ctx context.Context, id, dir string) ([]FileEntry, error) {
	if e := safeRemotePath(dir); e != nil {
		return nil, e
	}
	var out []FileEntry
	e := m.withSFTP(ctx, id, func(s *sftp.Client) error {
		items, e := s.ReadDir(dir)
		if e != nil {
			return e
		}
		for _, i := range items {
			out = append(out, FileEntry{Name: i.Name(), Path: path.Join(dir, i.Name()), Directory: i.IsDir(), Size: i.Size(), Modified: i.ModTime()})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, e
}
func (m *Manager) ReadText(ctx context.Context, id, p string) (string, string, error) {
	if e := safeRemotePath(p); e != nil {
		return "", "", e
	}
	var b []byte
	e := m.withSFTP(ctx, id, func(s *sftp.Client) error {
		f, e := s.Open(p)
		if e != nil {
			return e
		}
		defer f.Close()
		b, e = readBounded(f, 4<<20)
		return e
	})
	return string(b), HashText(b), e
}
func (m *Manager) WriteText(ctx context.Context, id, p, content, want string) (string, error) {
	if e := safeRemotePath(p); e != nil {
		return "", e
	}
	if len(content) > 4<<20 {
		return "", errors.New("text exceeds 4 MiB")
	}
	m.filesMu.Lock()
	defer m.filesMu.Unlock()
	var got string
	e := m.withSFTP(ctx, id, func(s *sftp.Client) error {
		f, e := s.Open(p)
		if e != nil {
			return e
		}
		b, e := readBounded(f, 4<<20)
		_ = f.Close()
		if e != nil {
			return e
		}
		got = HashText(b)
		if want == "" || want != got {
			return errors.New("file conflict")
		}
		return atomicRemoteWrite(s, p, bytes.NewBufferString(content), 4<<20)
	})
	if e != nil {
		return "", e
	}
	return HashText([]byte(content)), nil
}
func (m *Manager) Upload(ctx context.Context, id, p string, r io.Reader) (FileEntry, error) {
	m.filesMu.Lock()
	defer m.filesMu.Unlock()
	if e := safeRemotePath(p); e != nil {
		return FileEntry{}, e
	}
	var out FileEntry
	e := m.withSFTP(ctx, id, func(s *sftp.Client) error {
		if e := atomicRemoteWrite(s, p, r, 128<<20); e != nil {
			return e
		}
		i, e := s.Stat(p)
		if e == nil {
			out = FileEntry{Name: path.Base(p), Path: p, Directory: i.IsDir(), Size: i.Size(), Modified: i.ModTime()}
			return e
		}
		return e
	})
	return out, e
}
func (m *Manager) Download(ctx context.Context, id, p string, w io.Writer) error {
	if e := safeRemotePath(p); e != nil {
		return e
	}
	return m.withSFTP(ctx, id, func(s *sftp.Client) error {
		f, e := s.Open(p)
		if e != nil {
			return e
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil {
			return e
		}
		if info.Size() > 128<<20 {
			return errors.New("download exceeds 128 MiB")
		}
		n, e := io.Copy(w, io.LimitReader(f, 128<<20))
		if e == nil && n == 128<<20 {
			var extra [1]byte
			if count, readErr := f.Read(extra[:]); count > 0 {
				return errors.New("download grew beyond 128 MiB")
			} else if readErr != io.EOF {
				return readErr
			}
		}
		return e
	})
}

// WriteFile writes a bounded remote file through SFTP. Callers own any local
// path containment policy; this method accepts only the remote absolute path.
func (m *Manager) WriteFile(ctx context.Context, id, p string, data []byte) error {
	m.filesMu.Lock()
	defer m.filesMu.Unlock()
	if err := safeRemotePath(p); err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return errors.New("file exceeds 16 MiB")
	}
	return m.withSFTP(ctx, id, func(s *sftp.Client) error {
		return atomicRemoteWrite(s, p, bytes.NewReader(data), 16<<20)
	})
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e == nil && int64(len(b)) > limit {
		return nil, errors.New("file exceeds read limit")
	}
	return b, e
}

// POSIX rename is atomic even when replacing an existing destination. Never
// remove the destination as a fallback on servers that lack this extension.
func atomicRemoteWrite(s *sftp.Client, p string, r io.Reader, limit int64) error {
	tmp := path.Join(path.Dir(p), ".container-ssh-manager-"+core.ID()+".tmp")
	f, e := s.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if e != nil {
		return e
	}
	defer s.Remove(tmp)
	mode := os.FileMode(0600)
	if info, err := s.Stat(p); err == nil {
		mode = info.Mode().Perm()
	}
	if e = f.Chmod(mode); e != nil {
		_ = f.Close()
		return e
	}
	n, e := io.Copy(f, io.LimitReader(r, limit+1))
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if n > limit {
		return errors.New("file exceeds write limit")
	}
	if _, ok := s.HasExtension("posix-rename@openssh.com"); ok {
		return s.PosixRename(tmp, p)
	}
	if _, e = s.Lstat(p); !os.IsNotExist(e) {
		return errors.New("server lacks atomic replacement support")
	}
	return s.Rename(tmp, p)
}
