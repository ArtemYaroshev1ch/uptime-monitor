package app

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
)

func TestSelect(t *testing.T) {
	t.Parallel()
	noop := func(context.Context, []string, observability.BuildInfo, io.Writer) error { return nil }
	commands := map[string]RunFunc{"api": noop, "worker": noop, "migrate": noop}

	tests := []struct {
		name     string
		argv0    string
		args     []string
		wantName string
		wantArgs []string
		wantErr  string
	}{
		{name: "symlink", argv0: "/app/worker", args: []string{"-healthcheck"}, wantName: "worker", wantArgs: []string{"-healthcheck"}},
		// The path separator is the platform's own: tests run on Windows
		// locally and on Linux in CI.
		{name: "exe suffix", argv0: filepath.Join("bin", "api.exe"), args: nil, wantName: "api", wantArgs: nil},
		{name: "subcommand", argv0: "/app/uptime-monitor", args: []string{"migrate", "up"}, wantName: "migrate", wantArgs: []string{"up"}},
		{name: "relative path", argv0: "./uptime-monitor", args: []string{"api", "-version"}, wantName: "api", wantArgs: []string{"-version"}},
		{name: "no command", argv0: "/app/uptime-monitor", wantErr: "usage: uptime-monitor <api|migrate|worker> [flags]"},
		{name: "unknown command", argv0: "uptime-monitor", args: []string{"scheduler"}, wantErr: "usage:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			name, args, err := Select(tt.argv0, tt.args, commands)

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantName, name)
			assert.Equal(t, tt.wantArgs, args)
		})
	}
}
