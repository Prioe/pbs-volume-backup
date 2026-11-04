package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantOut    string
		wantErr    error
		wantErrMsg string
	}{
		{name: "no command", wantErr: errUsage},
		{name: "unknown command", args: []string{"frobnicate"}, wantErr: errUsage},
		{name: "version", args: []string{"version"}, wantOut: "dev\n"},
		{name: "help", args: []string{"--help"}, wantOut: usage},
		{name: "restore without source", args: []string{"restore"}, wantErrMsg: "--from is required"},
		{name: "restore without config", args: []string{"restore", "--from", "app/data", "--namespace", "pbs"}, wantErrMsg: "repository is required"},
		{name: "backup without config", args: []string{"backup", "--namespace", "pbs"}, wantErrMsg: "repository is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(t.Context(), tt.args, &out, io.Discard)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.wantErrMsg != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Fatalf("err = %v, want %q", err, tt.wantErrMsg)
				}
			case err != nil:
				t.Fatalf("unexpected error: %v", err)
			}
			if got := out.String(); got != tt.wantOut {
				t.Errorf("output = %q, want %q", got, tt.wantOut)
			}
		})
	}
}

func TestBackupFlags(t *testing.T) {
	t.Setenv("PBS_REPOSITORY", "u@pbs!t@pbs.example:ds")
	t.Setenv("PVB_SNAPSHOT_CLASSES", "a=class-a, b=class-b")
	t.Setenv("PVB_BACKUP_TIMEOUT", "1h")
	cfg, verbose, err := backupFlags([]string{"--snapshot-class", "c=class-c", "--skip-label", "backup=no", "-v"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !verbose || cfg.Repository != "u@pbs!t@pbs.example:ds" || cfg.BackupTimeout != time.Hour ||
		cfg.SkipLabel != "backup" || cfg.SkipValue != "no" || cfg.TokenSecretKey != "password" {
		t.Errorf("cfg = %+v", cfg)
	}
	want := map[string]string{"a": "class-a", "b": "class-b", "c": "class-c"}
	if len(cfg.SnapshotClasses) != len(want) {
		t.Errorf("snapshot classes = %v", cfg.SnapshotClasses)
	}
	for k, v := range want {
		if cfg.SnapshotClasses[k] != v {
			t.Errorf("snapshot classes = %v", cfg.SnapshotClasses)
		}
	}
}

func TestBackupFlagsM3(t *testing.T) {
	t.Setenv("PVB_CLIENT_ARGS", "--rate=50MiB --burst=100MiB")
	t.Setenv("PVB_CONCURRENCY", "3")
	t.Setenv("PVB_ENCRYPTION_KEY_SECRET", "pbs-key")
	cfg, _, err := backupFlags([]string{"--client-arg", "--crypt-mode=encrypt"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.ExtraArgs, " ") != "--rate=50MiB --burst=100MiB --crypt-mode=encrypt" {
		t.Errorf("extra args = %q", cfg.ExtraArgs)
	}
	if cfg.Concurrency != 3 || cfg.KeySecret != "pbs-key" || cfg.KeySecretKey != "key" {
		t.Errorf("cfg = %+v", cfg)
	}

	t.Setenv("PVB_CONCURRENCY", "many")
	if _, _, err := backupFlags(nil, io.Discard); err == nil || !strings.Contains(err.Error(), "PVB_CONCURRENCY") {
		t.Errorf("err = %v", err)
	}
}

func TestRestoreFlags(t *testing.T) {
	cfg, _, err := restoreFlags([]string{"--from", "app/data", "--snapshot", "2026-11-04T18:04:09Z", "--into-existing"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SourceNamespace != "app" || cfg.SourcePVC != "data" || cfg.TargetNamespace != "app" || cfg.TargetPVC != "data" ||
		!cfg.IntoExisting || cfg.Snapshot != "2026-11-04T18:04:09Z" {
		t.Errorf("cfg = %+v", cfg)
	}
	cfg, _, err = restoreFlags([]string{"--from", "app/data", "--to", "other/copy"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SourcePVC != "data" || cfg.TargetNamespace != "other" || cfg.TargetPVC != "copy" {
		t.Errorf("cfg = %+v", cfg)
	}
	for _, bad := range []string{"data", "app/", "/data", "a/b/c"} {
		if _, _, err := restoreFlags([]string{"--from", bad}, io.Discard); err == nil {
			t.Errorf("--from %q accepted", bad)
		}
	}
}

func TestBackupFlagErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{name: "skip label without value", args: []string{"--skip-label", "skip"}, want: "not key=value"},
		{name: "bad mapping", args: []string{"--snapshot-class", "nope"}, want: "invalid usage"},
		{name: "bad duration env", env: map[string]string{"PVB_CLEANUP_TIMEOUT": "soon"}, want: "PVB_CLEANUP_TIMEOUT"},
		{name: "extra argument", args: []string{"extra"}, want: "unexpected arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, _, err := backupFlags(tt.args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
