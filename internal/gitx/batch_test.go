package gitx

import (
	"context"
	"strconv"
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

func TestShowManyAcceptsGitObjectIDLengths(t *testing.T) {
	for _, ref := range []string{strings.Repeat("a", 40), strings.Repeat("b", 64)} {
		got, err := (Git{}).ShowMany(context.Background(), ref, nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("ShowMany(%d hex) = %#v, %v", len(ref), got, err)
		}
		out := ref + " blob 1\nx\n"
		if got, err := parseBatchBlobs(out, 1); err != nil || got[0] != "x" {
			t.Fatalf("%d-hex batch header = %#v, %v", len(ref), got, err)
		}
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

func TestParseBatchBlobsPreservesArbitraryTerminalCRLF(t *testing.T) {
	for _, value := range []string{
		strings.Repeat("x", showManyFileLimit) + "\n\n\n",
		strings.Repeat("x", showManyFileLimit) + strings.Repeat("\r\n", 128),
	} {
		out := strings.Repeat("a", 40) + " blob " + strconv.Itoa(len(value)) + "\n" + value + "\n"
		if got, err := parseBatchBlobs(out, 1); err != nil || got[0] != value {
			t.Fatalf("terminal newline payload rejected: len=%d got=%d err=%v", len(value), len(got), err)
		}
	}
}

func TestParseBatchBlobsRejectsDeclaredLengthBeyondCapturedOutput(t *testing.T) {
	out := strings.Repeat("a", 40) + " blob " + strconv.Itoa(int(^uint(0)>>1)) + "\n"
	if _, err := parseBatchBlobs(out, 1); err == nil {
		t.Fatal("accepted declared length beyond captured output")
	}
}
