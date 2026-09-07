package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/auth"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/connection"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/engine"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/jobs"
	assets "github.com/Ding-Ding-Projects/container-ssh-manager/web"
	"golang.org/x/term"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var version = "development"
var updatedAt = "unavailable"

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func main() {
	if e := run(); e != nil {
		log.Fatal(e)
	}
}
func run() error {
	data := env("DATA_DIR", "data")
	keyPath := env("VAULT_KEY_FILE", "secrets/vault.key")
	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		if e := os.MkdirAll(filepath.Dir(keyPath), 0700); e != nil {
			return e
		}
		f, e := os.CreateTemp(filepath.Dir(keyPath), ".key-init-*")
		if e != nil {
			return e
		}
		defer f.Close()
		defer os.Remove(f.Name())
		key := make([]byte, 32)
		defer clear(key)
		if _, e = rand.Read(key); e != nil {
			return e
		}
		_, e = f.Write(key)
		if e != nil {
			return e
		}
		if e = f.Sync(); e != nil {
			return e
		}
		if e = f.Close(); e != nil {
			return e
		}
		if e = os.Link(f.Name(), keyPath); e != nil {
			return e
		}
		if e == nil {
			fmt.Println("Vault key created. Preserve it separately from database backups.")
		}
		return e
	}
	s, e := core.NewStore(filepath.Join(data, "manager.db"))
	if e != nil {
		return e
	}
	defer s.Close()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "bootstrap":
			fmt.Fprintln(os.Stderr, "Read owner password from standard input (12-72 bytes). Never pass it as an argument.")
			var line []byte
			var e error
			if term.IsTerminal(int(os.Stdin.Fd())) {
				line, e = term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Fprintln(os.Stderr)
			} else {
				reader := bufio.NewReaderSize(os.Stdin, 128)
				var prefix bool
				line, prefix, e = reader.ReadLine()
				if prefix {
					return errors.New("password input too long")
				}
			}
			if e != nil {
				return e
			}
			if len(line) < 12 || len(line) > 72 || strings.ContainsAny(string(line), "\x00\r\n") {
				return errors.New("password must contain 12-72 bytes")
			}
			defer clear(line)
			if e = auth.Bootstrap(s, line); e != nil {
				return errors.New("owner setup failed; an owner may already exist")
			}
			fmt.Println("Owner created.")
			return nil
		case "backup":
			if len(os.Args) != 3 {
				return errors.New("usage: manager backup DESTINATION")
			}
			return s.Backup(os.Args[2])
		case "healthcheck":
			client := http.Client{Timeout: 3 * time.Second}
			r, e := client.Get("http://127.0.0.1:8080/api/v1/health")
			if e != nil {
				return e
			}
			defer r.Body.Close()
			if r.StatusCode != 200 {
				return errors.New("health check failed")
			}
			return nil
		default:
			return errors.New("unknown command")
		}
	}
	vault, e := core.LoadVault(keyPath)
	if e != nil {
		return e
	}
	origin := env("PUBLIC_ORIGIN", "https://localhost:8443")
	a := auth.New(s, origin)
	if cidr := os.Getenv("TRUSTED_PROXY_CIDR"); cidr != "" {
		_, a.TrustedProxy, e = net.ParseCIDR(cidr)
		if e != nil {
			return errors.New("invalid trusted proxy network")
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) { core.JSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		core.JSON(w, 200, map[string]string{"version": version, "updatedAt": updatedAt})
	})
	mux.HandleFunc("POST /api/v1/login", a.Login)
	mux.HandleFunc("POST /api/v1/logout", a.Logout)
	mux.HandleFunc("GET /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		core.JSON(w, 200, map[string]bool{"authenticated": true})
	})
	c := connection.New(s, vault)
	c.Register(mux)
	engine.New(s, c, vault).Register(mux)
	j := jobs.New(s, c, vault)
	j.Register(mux)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	j.Start(ctx)
	frontend, e := fs.Sub(assets.Files, "dist")
	if e != nil {
		return e
	}
	files := http.FileServer(http.FS(frontend))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			core.Error(w, 404, "Unknown endpoint")
			return
		}
		files.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: env("LISTEN_ADDR", ":8080"), Handler: a.Wrap(mux), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute, WriteTimeout: 15 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("Container SSH Manager %s listening on %s", version, server.Addr)
	e = server.ListenAndServe()
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
