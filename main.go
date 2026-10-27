// Command data_organizer serves a browser UI for browsing and backing up the
// local filesystem.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/stefan-zemljic/data-organizer/internal/backupjob"
	"github.com/stefan-zemljic/data-organizer/internal/restorejob"
	"github.com/stefan-zemljic/data-organizer/internal/scan"
	"github.com/stefan-zemljic/data-organizer/internal/server"
	"github.com/stefan-zemljic/data-organizer/internal/store"
	"github.com/stefan-zemljic/data-organizer/web"
)

// version is set by release builds (-ldflags "-X main.version=v1.2.3").
var version = ""

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address; keep it on loopback, the API exposes the whole filesystem")
	dbPath := flag.String("db", "data_organizer.db", "SQLite database file for backups")
	dev := flag.Bool("dev", false, "serve web/static from disk instead of the embedded copy (for frontend work)")
	open := flag.Bool("open", true, "open the UI in the default browser")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("data_organizer", versionString())
		return
	}
	if err := run(*addr, *dbPath, *dev, *open); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func versionString() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version // "go install ...@v1.2.3"
	}
	return "(devel)"
}

func run(addr, dbPath string, dev, open bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath, err := filepath.Abs(dbPath)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	// Frees pack space left by a crash (bytes written but never indexed).
	if _, err := st.CompactContents(ctx); err != nil {
		return err
	}
	backups := backupjob.New(st, scan.Options{
		// Never hash the database or the content store while they are written.
		Exclude: []string{dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + "-journal", store.PackDir(dbPath)},
	})
	// Restores and backups both work the disks hard: never run them together.
	restores := restorejob.New(st, backups.Busy)
	backups.SetGuard(func() error {
		if restores.Busy() {
			return fmt.Errorf("%w: a restore is running or paused", backupjob.ErrBusy)
		}
		return nil
	})
	// Continues a backup the last run left unfinished (after the compaction,
	// which drops what it wrote but never indexed).
	if err := backups.Recover(ctx); err != nil {
		return err
	}
	// Likewise an unfinished restore (at most one of the two exists).
	if err := restores.Recover(ctx); err != nil {
		return err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	key, err := st.AccessKey(ctx)
	if err != nil {
		return err
	}
	access := server.NewAccess(key, port)

	allowedHosts := []string{
		net.JoinHostPort("127.0.0.1", port),
		net.JoinHostPort("localhost", port),
		net.JoinHostPort("::1", port),
	}
	if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
		allowedHosts = append(allowedHosts, net.JoinHostPort(host, port))
	}

	var static fs.FS = web.Static()
	if dev {
		static = os.DirFS("web/static")
	}

	base := "http://" + allowedHosts[0] + "/"
	fmt.Printf("data_organizer %s: open %s?key=%s (database %s)\n", versionString(), base, key, dbPath)
	if open {
		if err := openBrowser(base + "?code=" + access.LoginCode()); err != nil {
			slog.Warn("could not open the browser; open the link above", "err", err)
		}
	}
	sc := echo.StartConfig{
		Listener:   ln,
		HideBanner: true,
		HidePort:   true,
		BeforeServeFunc: func(s *http.Server) error {
			s.ReadHeaderTimeout = 10 * time.Second
			s.IdleTimeout = 2 * time.Minute
			return nil
		},
	}
	serveErr := sc.Start(ctx, server.New(static, allowedHosts, access, backups, restores))

	// Pause a running backup and save it; the next start resumes it.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	restores.Shutdown(shutdownCtx) // saves what was restored so far; the next start continues
	if err := backups.Shutdown(shutdownCtx); err != nil {
		slog.Error("stopping backup", "err", err)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}
