package connection

import (
	"bytes"
	"context"
	"errors"
	"github.com/pkg/sftp"
	"io"
	"path"
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

func (m *Manager) withSFTP(ctx context.Context, hostID string, fn func(*sftp.Client) error) error {
	c, e := m.Dial(ctx, hostID)
	if e != nil {
		return e
	}
	defer c.Close()
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
		b, e = io.ReadAll(io.LimitReader(f, 4<<20))
		return e
	})
	return string(b), HashText(b), e
}
func (m *Manager) WriteText(ctx context.Context, id, p, content, want string) (string, error) {
	if e := safeRemotePath(p); e != nil {
		return "", e
	}
	var got string
	e := m.withSFTP(ctx, id, func(s *sftp.Client) error {
		f, e := s.Open(p)
		if e != nil {
			return e
		}
		b, e := io.ReadAll(io.LimitReader(f, 4<<20))
		_ = f.Close()
		if e != nil {
			return e
		}
		got = HashText(b)
		if want == "" || want != got {
			return errors.New("file conflict")
		}
		tmp := p + ".container-ssh-manager.tmp"
		w, e := s.OpenFile(tmp, 0x241)
		if e != nil {
			return e
		}
		if _, e = io.Copy(w, bytes.NewBufferString(content)); e != nil {
			_ = w.Close()
			return e
		}
		if e = w.Close(); e != nil {
			return e
		}
		return s.Rename(tmp, p)
	})
	if e != nil {
		return "", e
	}
	return HashText([]byte(content)), nil
}
func (m *Manager) Upload(ctx context.Context, id, p string, r io.Reader) (FileEntry, error) {
	if e := safeRemotePath(p); e != nil {
		return FileEntry{}, e
	}
	var out FileEntry
	e := m.withSFTP(ctx, id, func(s *sftp.Client) error {
		f, e := s.OpenFile(p, 0x241)
		if e != nil {
			return e
		}
		_, e = io.Copy(f, io.LimitReader(r, 128<<20))
		if closeE := f.Close(); e == nil {
			e = closeE
		}
		if e != nil {
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
		_, e = io.Copy(w, io.LimitReader(f, 128<<20))
		return e
	})
}

// WriteFile writes a bounded remote file through SFTP. Callers own any local
// path containment policy; this method accepts only the remote absolute path.
func (m *Manager) WriteFile(ctx context.Context, id, p string, data []byte) error {
	if err := safeRemotePath(p); err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return errors.New("file exceeds 16 MiB")
	}
	return m.withSFTP(ctx, id, func(s *sftp.Client) error {
		tmp := p + ".container-ssh-manager-" + HashText(data)[:12] + ".tmp"
		defer s.Remove(tmp)
		f, err := s.OpenFile(tmp, 0x241)
		if err != nil {
			return err
		}
		if _, err = io.Copy(f, bytes.NewReader(data)); err != nil {
			_ = f.Close()
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
		return s.Rename(tmp, p)
	})
}
