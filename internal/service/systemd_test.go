package service

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestUnitEscapesPathsAndPreservesRecovery(t *testing.T) {
	base := t.TempDir()
	o := Options{Name: "example", Binary: filepath.Join(base, "bin with spaces%name$variable", "aih"), Repository: filepath.Join(base, "app with spaces%name$variable"), Home: filepath.Join(base, "state"), EnvironmentFile: filepath.Join(base, "env"), Path: "/usr/bin:/bin"}
	unit, err := Render(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"start\" \"--foreground", "%%name$$variable", "Restart=on-failure", "StartLimitIntervalSec=0", "KillMode=mixed", "TimeoutStopSec=180s", "UMask=0077", "WantedBy=default.target"} {
		if !strings.Contains(unit, want) {
			t.Fatal("missing", want, unit)
		}
	}
	if strings.Contains(unit, "User=root") || strings.Contains(unit, "force") {
		t.Fatal("unsafe service")
	}
	// Unlike ExecStart and Environment, WorkingDirectory is not unquoted by
	// systemd. Quoting makes it a relative path and prevents service startup.
	if !strings.Contains(unit, "WorkingDirectory="+strings.ReplaceAll(o.Repository, "%", "%%")+"\n") {
		t.Fatal("working directory must use literal path syntax", unit)
	}
	if !strings.Contains(unit, "ExecStart="+quote(o.Binary, false)+" ") {
		t.Fatal("executable path must not double dollar signs", unit)
	}
	for _, bad := range []string{"../escape", "line\nbreak", "", "a.service"} {
		o.Name = bad
		if _, err = Render(o); err == nil {
			t.Fatal("invalid name accepted", bad)
		}
	}
	o.Name = "example"
	o.Binary = ""
	if _, err = Render(o); err == nil {
		t.Fatal("empty binary accepted")
	}
}
