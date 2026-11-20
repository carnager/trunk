// Command trunk is a self-hosted home for photos and videos.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/carnager/trunk/internal/server"
	"github.com/carnager/trunk/internal/takeout"
	"golang.org/x/term"
)

var version = "dev"

func usage() {
	fmt.Fprint(os.Stderr, `trunk: a home for your photos and videos

Usage:
  trunk serve [-listen ADDR] [-data DIR] [-trash-days N] [-trust-proxy] [-debug]
  trunk user add NAME [-admin]     create an account (asks for a password)
  trunk user passwd NAME           set a new password
  trunk user list                  list accounts
  trunk import takeout -user NAME [-dry-run] [-jobs N] ZIP|TGZ|DIR...
                                   import a Google Photos export from Google Takeout
  trunk version

Every command takes -data DIR (default $TRUNK_DATA or ./data).
For scripts, the password can come from $TRUNK_PASSWORD instead of a prompt.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "user":
		err = cmdUser(os.Args[2:])
	case "import":
		if len(os.Args) < 3 || os.Args[2] != "takeout" {
			usage()
			os.Exit(2)
		}
		err = cmdImportTakeout(os.Args[3:])
	case "version", "-version", "--version":
		fmt.Println("trunk", version)
	case "help", "-h", "-help", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "trunk:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dataFlag(fs *flag.FlagSet) *string {
	return fs.String("data", env("TRUNK_DATA", "./data"), "data directory (originals, thumbnails, database)")
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	data := dataFlag(fs)
	listen := fs.String("listen", env("TRUNK_LISTEN", ":8420"), "address to listen on")
	trashDays := fs.Int("trash-days", 30, "days before trashed items are deleted for good")
	trustProxy := fs.Bool("trust-proxy", os.Getenv("TRUNK_TRUST_PROXY") != "", "trust X-Forwarded-For from a reverse proxy")
	debug := fs.Bool("debug", false, "log every request")
	fs.Parse(args)

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	srv, err := server.New(server.Config{
		DataDir: *data, TrashDays: *trashDays, TrustProxy: *trustProxy, Version: version,
	}, log)
	if err != nil {
		return err
	}
	defer srv.Close()

	var users int
	srv.DB().QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)
	if users == 0 {
		log.Warn("no accounts yet; create one with: trunk user add NAME -admin -data " + *data)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.Run(ctx)

	hs := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No Read/WriteTimeout: uploads and video streams may legitimately run long.
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("trunk listening", "addr", *listen, "data", *data, "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}

func cmdUser(args []string) error {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	fs := flag.NewFlagSet("user "+args[0], flag.ExitOnError)
	data := dataFlag(fs)
	admin := fs.Bool("admin", false, "make the account an administrator")
	// Allow flags before and after the name.
	var pos []string
	rest := args[1:]
	for {
		fs.Parse(rest)
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}

	srv, err := server.New(server.Config{DataDir: *data}, slog.New(slog.DiscardHandler))
	if err != nil {
		return err
	}
	defer srv.Close()
	db := srv.DB()

	switch args[0] {
	case "add":
		if len(pos) != 1 {
			return errors.New("usage: trunk user add NAME [-admin]")
		}
		name := pos[0]
		if strings.ContainsAny(name, "/\\ ") || name == "." || name == ".." {
			return errors.New("user names cannot contain spaces or slashes")
		}
		pw, err := readPassword()
		if err != nil {
			return err
		}
		hash, err := server.HashPassword(pw)
		if err != nil {
			return err
		}
		_, err = db.Exec(`INSERT INTO users (name, pw_hash, is_admin, created_at) VALUES (?, ?, ?, ?)`,
			name, hash, *admin, time.Now().UnixMilli())
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("user %q already exists", name)
		}
		if err == nil {
			fmt.Printf("created %s\n", name)
		}
		return err
	case "passwd":
		if len(pos) != 1 {
			return errors.New("usage: trunk user passwd NAME")
		}
		pw, err := readPassword()
		if err != nil {
			return err
		}
		hash, err := server.HashPassword(pw)
		if err != nil {
			return err
		}
		res, err := db.Exec(`UPDATE users SET pw_hash = ? WHERE name = ?`, hash, pos[0])
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no user %q", pos[0])
		}
		fmt.Println("password changed; existing devices stay logged in (revoke them in the app if needed)")
		return nil
	case "list":
		return listUsers(db)
	}
	usage()
	os.Exit(2)
	return nil
}

func listUsers(db *sql.DB) error {
	rows, err := db.Query(`SELECT u.name, u.is_admin,
		(SELECT COUNT(*) FROM assets a WHERE a.user_id = u.id AND a.deleted_at IS NULL),
		(SELECT COUNT(*) FROM devices d WHERE d.user_id = u.id)
		FROM users u ORDER BY u.name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Printf("%-20s %-6s %8s %8s\n", "NAME", "ADMIN", "ASSETS", "DEVICES")
	for rows.Next() {
		var name string
		var admin bool
		var assets, devices int
		if err := rows.Scan(&name, &admin, &assets, &devices); err != nil {
			return err
		}
		a := ""
		if admin {
			a = "yes"
		}
		fmt.Printf("%-20s %-6s %8d %8d\n", name, a, assets, devices)
	}
	return rows.Err()
}

func readPassword() (string, error) {
	if pw := os.Getenv("TRUNK_PASSWORD"); pw != "" {
		return pw, nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no terminal to ask for a password: run this in a terminal, " +
				"pipe the password in, or set TRUNK_PASSWORD")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, "Password: ")
	a, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Again: ")
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(a) != string(b) {
		return "", errors.New("passwords do not match")
	}
	if len(a) < 8 {
		return "", errors.New("use at least 8 characters")
	}
	return string(a), nil
}

func cmdImportTakeout(args []string) error {
	fs := flag.NewFlagSet("import takeout", flag.ExitOnError)
	data := dataFlag(fs)
	user := fs.String("user", "", "account to import into (required)")
	dry := fs.Bool("dry-run", false, "only show what would be imported")
	jobs := fs.Int("jobs", min(8, max(2, runtime.NumCPU()/2)), "files to import at once")
	// Allow flags after the inputs too: trunk import takeout *.zip -user alice
	var inputs []string
	rest := args
	for {
		fs.Parse(rest)
		if fs.NArg() == 0 {
			break
		}
		inputs = append(inputs, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if *user == "" || len(inputs) == 0 {
		return errors.New("usage: trunk import takeout -user NAME [-dry-run] [-jobs N] ZIP|TGZ|DIR...")
	}

	entries, closer, err := takeout.Sources(inputs, os.Stdout)
	if err != nil {
		return err
	}
	defer closer.Close()
	fmt.Printf("reading %d files from %d inputs\n", len(entries), len(inputs))
	plan := takeout.NewPlan(entries)

	var edits, withSidecar int
	var bytes int64
	for _, it := range plan.Items {
		bytes += it.Size
		if it.EditOf != nil {
			edits++
		}
		if it.Sidecar != nil {
			withSidecar++
		}
	}
	fmt.Printf("%d photo and video files (%.1f GB; album folders repeat photos, which are stored once), "+
		"%d edits of another photo, %d with Google metadata\n",
		len(plan.Items)-edits, float64(bytes)/(1<<30), edits, withSidecar)
	fmt.Printf("%d albums", len(plan.Albums))
	if len(plan.Albums) > 0 {
		names := make([]string, 0, len(plan.Albums))
		for _, a := range plan.Albums {
			names = append(names, fmt.Sprintf("%s (%d)", a.Name, len(a.Items)))
		}
		fmt.Printf(": %s", strings.Join(names, ", "))
	}
	fmt.Println()
	if len(plan.Skipped) > 0 {
		fmt.Printf("%d other files skipped (HTML, unknown types, trash)\n", len(plan.Skipped))
	}
	if *dry {
		return nil
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	srv, err := server.New(server.Config{DataDir: *data}, log)
	if err != nil {
		return err
	}
	defer srv.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	im, err := srv.Importer(ctx, *user)
	if err != nil {
		return err
	}
	start := time.Now()
	res := takeout.Run(ctx, im, plan, *jobs, os.Stdout)
	fmt.Printf("done in %s: %d added, %d already there, %d edits, %d failed; albums: %d new, %d updated\n",
		time.Since(start).Round(time.Second), res.Added, res.Existing, res.Edits, res.Failed,
		res.AlbumsCreated, res.AlbumsUpdated)
	for _, e := range res.Errors {
		fmt.Fprintln(os.Stderr, "  "+e)
	}
	if ctx.Err() != nil {
		return errors.New("interrupted; run the same command again to continue")
	}
	if res.Failed > 0 {
		return fmt.Errorf("%d files failed", res.Failed)
	}
	return nil
}
