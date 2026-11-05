package migrate

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
)

var testInfo = observability.BuildInfo{
	Version:   "1.2.3",
	Commit:    "abc123",
	BuiltAt:   "2026-01-02T03:04:05Z",
	GoVersion: "go1.27.0",
}

func TestRunVersion(t *testing.T) {
	t.Parallel()
	var out strings.Builder

	err := Run(t.Context(), []string{"-version"}, testInfo, &out)

	require.NoError(t, err)
	assert.Equal(t, "migrate 1.2.3 (commit abc123, built 2026-01-02T03:04:05Z, go1.27.0)\n", out.String())
}

// Bad arguments are reported before the configuration is read, so these
// cases need no DATABASE_URL.
func TestRunBadArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no command", args: nil, wantErr: "expected exactly one command"},
		{name: "two commands", args: []string{"up", "down"}, wantErr: "expected exactly one command"},
		{name: "unknown command", args: []string{"sideways"}, wantErr: `unknown command "sideways"`},
		{name: "unknown flag", args: []string{"-force", "up"}, wantErr: "flag provided but not defined"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := Run(t.Context(), tt.args, testInfo, io.Discard)

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestRunRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	err := Run(t.Context(), []string{"up"}, testInfo, io.Discard)

	require.ErrorContains(t, err, "DATABASE_URL")
}
