package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/new-api-tools/backend/internal/toolstore"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: toolstorectl <backup|verify|restore> [options]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var metadata toolstore.BackupMetadata
	var err error
	switch os.Args[1] {
	case "backup":
		flags := flag.NewFlagSet("backup", flag.ContinueOnError)
		source := flags.String("source", "", "source Tool Store database")
		destination := flags.String("destination", "", "new backup destination")
		if parseErr := flags.Parse(os.Args[2:]); parseErr != nil {
			fatalf("parse backup options: %v", parseErr)
		}
		metadata, err = toolstore.OnlineBackup(ctx, *source, *destination)
	case "verify":
		flags := flag.NewFlagSet("verify", flag.ContinueOnError)
		path := flags.String("path", "", "backup path")
		if parseErr := flags.Parse(os.Args[2:]); parseErr != nil {
			fatalf("parse verify options: %v", parseErr)
		}
		metadata, err = toolstore.VerifyBackup(ctx, *path)
	case "restore":
		flags := flag.NewFlagSet("restore", flag.ContinueOnError)
		backup := flags.String("backup", "", "verified backup source")
		destination := flags.String("destination", "", "stopped Tool Store database to restore")
		if parseErr := flags.Parse(os.Args[2:]); parseErr != nil {
			fatalf("parse restore options: %v", parseErr)
		}
		metadata, err = toolstore.RestoreBackup(ctx, *backup, *destination)
	default:
		fatalf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatalf("%s failed: %v", os.Args[1], err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(metadata); err != nil {
		fatalf("encode result: %v", err)
	}
}

func fatalf(format string, arguments ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
