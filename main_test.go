package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExeAllowed(t *testing.T) {
	if !exeAllowed("linux", "/tmp/game") || !exeAllowed("windows", `C:\Games\A.EXE`) {
		t.Fatal("expected allowed")
	}
	if exeAllowed("windows", `C:\Games\a.bat`) || exeAllowed("windows", `C:\Games\a`) {
		t.Fatal("expected blocked")
	}
}

func TestSafeExe(t *testing.T) {
	if _, err := safeExe("  "); err == nil {
		t.Fatal("empty")
	}
	if _, err := safeExe("relative.exe"); err == nil {
		t.Fatal("relative")
	}
	if _, err := safeExe("/no/such/obi-game.exe"); err == nil {
		t.Fatal("missing")
	}
	dir := t.TempDir()
	if _, err := safeExe(dir); err == nil {
		t.Fatal("directory")
	}
	f := filepath.Join(dir, "game.exe")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := safeExe(` "` + f + `" `)
	if err != nil || got != f {
		t.Fatalf("quoted path: got %q %v", got, err)
	}
	if _, err := safeExe(f + "\n" + f); err == nil {
		t.Fatal("newline")
	}
}

func TestCatalogRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "games.json")
	in := []Game{{ID: "red-meridian", Title: "Red Meridian", Cover: []string{"#000000", "#fe4c3c"}}}
	if err := writeCatalog(path, in); err != nil {
		t.Fatal(err)
	}
	got, err := readCatalog(path, nil)
	if err != nil || len(got) != 1 || got[0].Title != "Red Meridian" {
		t.Fatalf("%+v %v", got, err)
	}
	fallback := []byte(`[{"id":"ash","title":"Ash Cathedral"}]`)
	got, err = readCatalog(filepath.Join(dir, "missing.json"), fallback)
	if err != nil || got[0].ID != "ash" {
		t.Fatalf("fallback %+v %v", got, err)
	}
	if err := os.WriteFile(path, []byte("{"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCatalog(path, fallback); err == nil {
		t.Fatal("corrupt file should not be replaced")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "{" {
		t.Fatal("corrupt games.json was overwritten")
	}
	if _, err := readCatalog(filepath.Join(dir, "nope.json"), []byte("not json")); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("bad fallback: %v", err)
	}
}
