package update

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestCompatibilityAndChecksum(t *testing.T) {
	for _, v := range []string{"1.0.1", "v1.2.0"} {
		if !Compatible(v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"2.0.0", "1.1.0-rc1", "main", "../1.0.0"} {
		if Compatible(v) {
			t.Fatal(v)
		}
	}
	data := []byte("binary")
	manifest := fmt.Sprintf("%x  aih_windows_amd64.exe\n", sha256.Sum256(data))
	if e := Verify(data, manifest, "aih_windows_amd64.exe"); e != nil {
		t.Fatal(e)
	}
	if e := Verify([]byte("tampered"), manifest, "aih_windows_amd64.exe"); e == nil {
		t.Fatal("corruption accepted")
	}
}
