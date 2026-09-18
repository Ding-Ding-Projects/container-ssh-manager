package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/pkg/sftp"
)

// Adoption only reads regular files and stores an encrypted initial revision.
// It never rewrites the host files or starts/stops the existing project.
type composeSnapshot struct {
	Files             composeFiles `json:"files"`
	ComposeExists     bool         `json:"composeExists"`
	EnvironmentExists bool         `json:"environmentExists"`
}

func (m *Manager) readComposeProject(ctx context.Context, hostID, dir string) (composeFiles, string, error) {
	snapshot, fileName, err := m.readComposeSnapshot(ctx, hostID, dir, "", false)
	return snapshot.Files, fileName, err
}

func (m *Manager) readComposeSnapshot(ctx context.Context, hostID, dir, selectedFile string, allowMissing bool) (composeSnapshot, string, error) {
	var out composeSnapshot
	type opener func(string) (io.ReadCloser, error)
	var open opener
	if hostID == "local" {
		open = func(name string) (io.ReadCloser, error) {
			target := filepath.Join(dir, name)
			info, err := os.Lstat(target)
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("not a regular file")
			}
			file, err := os.Open(target)
			if err != nil {
				return nil, err
			}
			current, err := file.Stat()
			if err != nil || !os.SameFile(info, current) {
				file.Close()
				return nil, fmt.Errorf("file changed during adoption")
			}
			return file, nil
		}
	} else {
		client, err := m.connections.Dial(ctx, hostID)
		if err != nil {
			return out, "", fmt.Errorf("cannot connect to compose host")
		}
		defer client.Close()
		stop := context.AfterFunc(ctx, func() { _ = client.Close() })
		defer stop()
		files, err := sftp.NewClient(client)
		if err != nil {
			return out, "", fmt.Errorf("cannot open compose host files")
		}
		defer files.Close()
		open = func(name string) (io.ReadCloser, error) {
			target := path.Join(dir, name)
			info, err := files.Lstat(target)
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("not a regular file")
			}
			return files.Open(target)
		}
	}
	read := func(name string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		file, err := open(name)
		if err != nil {
			return "", err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
		defer clearBytes(data)
		if err != nil {
			return "", err
		}
		if len(data) > 4<<20 {
			return "", fmt.Errorf("file exceeds 4 MiB")
		}
		return string(data), nil
	}
	fileName := "compose.yaml"
	if selectedFile != "" {
		fileName = selectedFile
	}
	content, err := read(fileName)
	if errors.Is(err, fs.ErrNotExist) && selectedFile == "" {
		fileName = "compose.yml"
		content, err = read(fileName)
	}
	if err != nil && !(allowMissing && errors.Is(err, fs.ErrNotExist)) {
		return out, "", fmt.Errorf("cannot adopt compose.yaml or compose.yml: file missing, unreadable, nonregular, or oversized")
	}
	out.ComposeExists = err == nil
	if content == "" && !allowMissing {
		return out, "", fmt.Errorf("cannot adopt an empty compose file")
	}
	environment, err := read(".env")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return out, "", fmt.Errorf("cannot adopt .env: file unreadable, nonregular, or oversized")
	}
	out.EnvironmentExists = err == nil
	out.Files.Compose = content
	out.Files.Environment = environment
	return out, fileName, nil
}
