// Command eventally runs the Eventally web server.
package main

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/config"
	"github.com/WanderingLesserPanda/eventally/internal/db"
	"github.com/WanderingLesserPanda/eventally/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	dropPrivileges(cfg.DataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DSN)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer database.Close()

	app, err := web.New(database.Pool, "internal/web/templates", cfg.DataDir)
	if err != nil {
		log.Fatalf("templates: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("eventally listening on %s", cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
	log.Printf("shut down cleanly")
}

// dropPrivileges makes the container self-sufficient for bind mounts:
// when started as root inside a container (the default for docker
// deployments), it fixes ownership of the data directory to the
// unprivileged "eventally" user (matching the image's /etc/passwd
// entry) and drops privileges to it. No host-side chown is needed for
// bind-mounted directories.
//
// Outside a container — local dev as a normal user, or an explicit
// user: override in compose — it is a no-op and ownership stays the
// operator's concern.
func dropPrivileges(dataDir string) {
	// The drop is a container-runtime concern (bind-mount ownership).
	// Only attempt it when running as root inside a container: local dev
	// runs as a normal user (no-op either way), and a root host process
	// without the "eventally" user should be left alone.
	if os.Geteuid() != 0 {
		return
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return
	}
	u, err := user.Lookup("eventally")
	if err != nil {
		log.Fatalf("privileges: cannot look up runtime user 'eventally': %v", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		log.Fatalf("privileges: invalid uid %q: %v", u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		log.Fatalf("privileges: invalid gid %q: %v", u.Gid, err)
	}

	// Fix the data directory. On first boot a bind-mount point is created
	// by the runtime as root:root — chown the whole tree. On later boots
	// the top-level directory is already owned by the runtime user (new
	// files are created by it), so the walk is skipped.
	if st, err := os.Stat(dataDir); err != nil {
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			log.Fatalf("privileges: mkdir %s: %v", dataDir, err)
		}
	} else if st.Sys().(*syscall.Stat_t).Uid != uint32(uid) {
		if err := chownTree(dataDir, uid, gid); err != nil {
			log.Fatalf("privileges: chown %s: %v", dataDir, err)
		}
	}

	if err := syscall.Setgroups(nil); err != nil {
		log.Fatalf("privileges: setgroups: %v", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		log.Fatalf("privileges: setgid: %v", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		log.Fatalf("privileges: setuid: %v", err)
	}
	if os.Geteuid() != uid {
		log.Fatalf("privileges: expected uid %d after drop, got %d", uid, os.Geteuid())
	}
	log.Printf("dropped privileges to uid=%d gid=%d", uid, gid)
}

// chownTree sets uid/gid on every entry under root (first-boot repair of
// a pre-existing, host-owned bind mount).
func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(p, uid, gid)
	})
}
