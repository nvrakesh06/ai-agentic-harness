package gitx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPendingMergeMarkersScanEveryPathBeforePreservation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("<<<<<<< task\n=======\n>>>>>>> main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "unreadable"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker, err := pendingMergePathsHaveMarker(dir, []string{"marker", "unreadable"})
	if err == nil || marker || errors.Is(err, ErrPendingMergeUnresolved) {
		t.Fatalf("mixed marker/read failure = marker %t, error %v", marker, err)
	}
}

func TestPendingMergeMarkersAcceptOnlyAllReadableMarkers(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("<<<<<<< task\n=======\n>>>>>>> main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	marker, err := pendingMergePathsHaveMarker(dir, []string{"first", "second"})
	if err != nil || !marker {
		t.Fatalf("readable markers = marker %t, error %v", marker, err)
	}
}
