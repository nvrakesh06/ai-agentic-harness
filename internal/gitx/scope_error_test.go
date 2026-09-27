package gitx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPendingMergeMarkerReadFailureIsOrdinaryError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "unmerged"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker, err := pendingMergePathHasMarker(dir, "unmerged")
	if err == nil || marker {
		t.Fatalf("directory read = marker %t, error %v", marker, err)
	}
}
