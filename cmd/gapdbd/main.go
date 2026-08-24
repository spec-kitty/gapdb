package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/server"
)

const toolVersion = "gapdbd-v1"

func main() {
	if err := run(os.Args[1:]); err != nil {
		writeFailure(err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("gapdbd", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	directory := flags.String("db", "", "database directory")
	socket := flags.String("socket", "", "Unix socket path (default: <db>/gapdb.sock)")
	jsonOutput := flags.Bool("json", true, "emit machine-readable readiness")
	readTimeout := flags.Duration("read-timeout", 30*time.Second, "request read timeout")
	writeTimeout := flags.Duration("write-timeout", 30*time.Second, "response write timeout")
	idleTimeout := flags.Duration("idle-timeout", 2*time.Minute, "idle connection timeout")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *directory == "" {
		return errors.New("--db is required")
	}
	owner, err := server.Open(server.Config{Directory: *directory, SocketPath: *socket, Options: gapdb.DefaultOptions(), ToolVersion: toolVersion, ReadTimeout: *readTimeout, WriteTimeout: *writeTimeout, IdleTimeout: *idleTimeout, FS: evidenceFS()})
	if err != nil {
		return err
	}
	// Install shutdown handling before publishing readiness so an operator or
	// test can act on the readiness frame immediately without a signal race.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	if *jsonOutput {
		ready := struct {
			SchemaVersion uint16 `json:"schema_version"`
			Ready         bool   `json:"ready"`
			SocketPath    string `json:"socket_path"`
		}{1, true, owner.SocketPath()}
		if err := json.NewEncoder(os.Stdout).Encode(ready); err != nil {
			_ = owner.Close(context.Background())
			return err
		}
	} else {
		fmt.Fprintf(os.Stderr, "gapdbd ready on %s\n", owner.SocketPath())
	}
	<-signals
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return owner.Close(ctx)
}

func writeFailure(err error) {
	var structured *gapdb.Error
	if errors.As(err, &structured) {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			SchemaVersion uint16       `json:"schema_version"`
			OK            bool         `json:"ok"`
			Error         *gapdb.Error `json:"error"`
		}{1, false, structured.Clone()})
		return
	}
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		SchemaVersion uint16 `json:"schema_version"`
		OK            bool   `json:"ok"`
		Error         string `json:"error"`
	}{1, false, err.Error()})
}
