package db

import (
	"encoding/json"
	"testing"
)

// The browser MCP migration moves only rows still on the old Chromium default
// (with or without capture proxy args) to Camoufox, runs once, and leaves edits alone.
func TestBrowserMCPCamoufoxMigration(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	browser := func() (string, string) {
		t.Helper()
		var cmd string
		var args json.RawMessage
		if err := d.QueryRow(`SELECT command, args FROM mcp_servers WHERE name = 'browser'`).Scan(&cmd, &args); err != nil {
			t.Fatal(err)
		}
		var norm []string
		if err := json.Unmarshal(args, &norm); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(norm)
		return cmd, string(b)
	}
	setBrowser := func(cmd, args string) {
		t.Helper()
		if _, err := d.Exec(`UPDATE mcp_servers SET command = $1, args = $2 WHERE name = 'browser'`, cmd, args); err != nil {
			t.Fatal(err)
		}
	}
	rerun := func() {
		t.Helper()
		if err := d.SetSetting("browser_mcp_camoufox_v1", ""); err != nil {
			t.Fatal(err)
		}
		if err := d.seedBuiltins(); err != nil {
			t.Fatal(err)
		}
	}

	// The suite shares one database: restore the default row whatever this test leaves.
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM mcp_servers WHERE name = 'browser'`)
		_ = d.seedBuiltins()
	})

	if _, err := d.Exec(`DELETE FROM mcp_servers WHERE name = 'browser'`); err != nil {
		t.Fatal(err)
	}
	rerun()
	if cmd, args := browser(); cmd != browserMCPCommand || args != browserMCPArgs {
		t.Fatalf("fresh seed: got %s %s", cmd, args)
	}

	for _, old := range []string{
		`["@playwright/mcp","--headless"]`,
		`["@playwright/mcp","--headless","--proxy-server","http://127.0.0.1:8788"]`,
	} {
		setBrowser("npx", old)
		rerun()
		if cmd, args := browser(); cmd != browserMCPCommand || args != browserMCPArgs {
			t.Fatalf("old default %s not migrated: got %s %s", old, cmd, args)
		}
	}

	custom := `["@playwright/mcp","--headless","--browser","firefox"]`
	setBrowser("npx", custom)
	rerun()
	if cmd, args := browser(); cmd != "npx" || args != custom {
		t.Fatalf("custom row changed: got %s %s", cmd, args)
	}

	// Once the flag is set, a deliberate switch back to Chromium survives restarts.
	setBrowser("npx", `["@playwright/mcp","--headless"]`)
	if err := d.seedBuiltins(); err != nil {
		t.Fatal(err)
	}
	if cmd, _ := browser(); cmd != "npx" {
		t.Fatalf("migration re-ran after flag set: got %s", cmd)
	}
}
