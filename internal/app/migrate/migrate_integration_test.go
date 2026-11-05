//go:build integration

package migrate

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/postgres/pgtest"
)

type logRecord struct {
	Level            string `json:"level"`
	Msg              string `json:"msg"`
	Service          string `json:"service"`
	MigrationVersion int64  `json:"migration_version"`
	State            string `json:"state"`
	DBVersion        int64  `json:"db_version"`
}

// migrate runs the command the way `make migrate-<command>` does and returns
// its log records.
func migrate(t *testing.T, command string) []logRecord {
	t.Helper()
	var out strings.Builder

	err := Run(t.Context(), []string{command}, testInfo, &out)
	require.NoError(t, err, "migrate %s", command)

	var records []logRecord
	scanner := bufio.NewScanner(strings.NewReader(out.String()))
	for scanner.Scan() {
		var r logRecord
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &r), "log line: %s", scanner.Text())
		assert.Equal(t, "migrate", r.Service)
		records = append(records, r)
	}
	return records
}

// TestCommands walks through a typical session: apply everything, inspect,
// roll back one step at a time until nothing is left.
func TestCommands(t *testing.T) {
	t.Setenv("APP_ENV", "prod") // JSON logs
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("DATABASE_URL", pgtest.Start(t))

	up := migrate(t, "up")
	require.NotEmpty(t, up)
	assert.Equal(t, "migration up", up[0].Msg)
	assert.Equal(t, int64(1), up[0].MigrationVersion)
	last := up[len(up)-1]
	assert.Equal(t, "migrations up to date", last.Msg)
	applied := len(up) - 1
	assert.Equal(t, int64(applied), last.DBVersion)

	again := migrate(t, "up")
	require.Len(t, again, 1, "a second up applies nothing")
	assert.Equal(t, "migrations up to date", again[0].Msg)

	status := migrate(t, "status")
	require.Len(t, status, applied)
	for _, s := range status {
		assert.Equal(t, "applied", s.State, "migration %d", s.MigrationVersion)
	}

	for v := applied; v >= 1; v-- {
		down := migrate(t, "down")
		require.Len(t, down, 1)
		assert.Equal(t, "migration down", down[0].Msg)
		assert.Equal(t, int64(v), down[0].MigrationVersion)
	}

	nothing := migrate(t, "down")
	require.Len(t, nothing, 1)
	assert.Equal(t, "nothing to roll back", nothing[0].Msg)

	for _, s := range migrate(t, "status") {
		assert.Equal(t, "pending", s.State, "migration %d", s.MigrationVersion)
	}
}
