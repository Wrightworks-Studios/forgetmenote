package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"forgetmenote/internal/capture"
)

// Exit codes of admin capture, which the external backup worker reads.
const (
	captureOK      = 0
	captureFailed  = 1
	captureUsage   = 2
	captureCommand = "forgetmenote admin capture"
)

// runCaptureAdmin runs admin capture -out DIR on the running instance, with
// its configuration, and returns the exit code: it copies the instance into
// DIR while it keeps serving, prints "snapshot <ID> purge <ID or ->" and
// waits for done on stdin while the caller dumps the database with that
// snapshot (internal/capture, docs/backups.md). Errors go to stderr; nothing
// it prints is secret.
func runCaptureAdmin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(code int, err error) int {
		fmt.Fprintf(stderr, "%s: %v\n", captureCommand, err)
		return code
	}
	flags := flag.NewFlagSet("admin capture", flag.ContinueOnError)
	flags.SetOutput(stderr)
	data := flags.String("data", env("DATA_DIR", "/data"), "instance data directory")
	out := flags.String("out", "", "new directory for the capture")
	if err := flags.Parse(args); err != nil {
		return captureUsage
	}
	if flags.NArg() != 0 || *out == "" {
		return fail(captureUsage, errors.New("usage: forgetmenote admin capture -out NEW_DIRECTORY [-data DIR]"))
	}
	if _, err := os.Lstat(*out); err == nil {
		return fail(captureUsage, fmt.Errorf("%s exists; the capture needs a new directory", *out))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fail(captureUsage, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Git in the capture's copies creates files too; none is for others.
	defer syscall.Umask(syscall.Umask(0o077))
	db, dir, config, err := stateDatabase(ctx, flags, *data)
	if err != nil {
		return fail(captureFailed, err)
	}
	defer db.Close()
	blobs, err := openBlobs(ctx, config.Server.Blobs, dir)
	if err != nil {
		return fail(captureFailed, err)
	}
	defer closeBlobs(blobs)
	err = capture.Run(ctx, capture.Options{DB: db, Blobs: blobs, Data: dir, Out: *out, Version: buildVersion()}, stdin, stdout)
	if err != nil {
		return fail(captureFailed, err)
	}
	return captureOK
}

// buildVersion names this binary by the commit it was built from, when the
// build recorded it.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "forgetmenote"
	}
	version := info.Main.Version
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			version = s.Value
		}
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.modified" && s.Value == "true" {
			version += "+modified"
		}
	}
	return "forgetmenote " + version
}
