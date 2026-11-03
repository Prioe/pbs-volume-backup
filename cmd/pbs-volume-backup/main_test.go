package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
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
		{name: "backup stub", args: []string{"backup"}, wantErrMsg: "backup: not implemented"},
		{name: "restore stub", args: []string{"restore"}, wantErrMsg: "restore: not implemented"},
		{name: "run-client stub", args: []string{"run-client"}, wantErrMsg: "run-client: not implemented"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(tt.args, &out)
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
