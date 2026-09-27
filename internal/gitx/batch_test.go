package gitx

import (
	"strings"
	"testing"
)

func batchBlob(value string) string {
	return strings.Repeat("a", 40) + " blob " + string(rune('0'+len(value))) + "\n" + value + "\n"
}

func TestParseBatchBlobsPreservesTextAndTerminalNewlines(t *testing.T) {
	value := "line\n"
	out := strings.Repeat("a", 40) + " blob 5\n" + value + "\n"
	got, err := parseBatchBlobs(out, 1)
	if err != nil || len(got) != 1 || got[0] != value {
		t.Fatalf("batch parse = %#v, %v", got, err)
	}
}

func TestParseBatchBlobsRejectsMalformedMissingTruncatedOversizedAndTrailing(t *testing.T) {
	valid := strings.Repeat("a", 40) + " blob 1\nx\n"
	for _, out := range []string{
		"missing\n",
		strings.Repeat("a", 40) + " missing\n",
		strings.Repeat("a", 40) + " tree 1\nx\n",
		strings.Repeat("a", 40) + " blob 2\nx\n",
		strings.Repeat("a", 40) + " blob 131073\n",
		valid + "extra",
	} {
		if _, err := parseBatchBlobs(out, 1); err == nil {
			t.Fatalf("accepted malformed batch %q", out)
		}
	}
}

func TestParseBatchBlobsRequiresEveryRequestedResult(t *testing.T) {
	out := strings.Repeat("a", 40) + " blob 1\nx\n"
	if _, err := parseBatchBlobs(out, 2); err == nil {
		t.Fatal("accepted missing requested blob")
	}
}
