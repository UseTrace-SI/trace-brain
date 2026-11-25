package embed

import (
	"hash/fnv"
	"math"
	"testing"
)

// highHashWord returns a word whose FNV-1a hash exceeds MaxInt32, the case
// that produced a negative bucket on 32-bit platforms.
func highHashWord(t *testing.T) (string, uint32) {
	t.Helper()
	for _, word := range []string{"a", "b", "c", "deploy", "timeout", "database", "cache", "retry"} {
		h := fnv.New32a()
		h.Write([]byte(word))
		if sum := h.Sum32(); sum > math.MaxInt32 {
			return word, sum
		}
	}
	t.Fatal("no fixture word hashes above MaxInt32")
	return "", 0
}

func TestEmbedBucketsHighHashesWithinRange(t *testing.T) {
	word, sum := highHashWord(t)
	v := Embed(word)
	if len(v) != Dim {
		t.Fatalf("dimension %d", len(v))
	}
	bucket := int(sum % Dim)
	if v[bucket] != 1 {
		t.Fatalf("%q (hash %d) not in bucket %d: %v", word, sum, bucket, v[bucket])
	}
}

func TestEmbedIsDeterministicNormalizedAndCaseInsensitive(t *testing.T) {
	a := Embed("Deploy the release, then deploy again!")
	b := Embed("deploy THE release then DEPLOY again")
	var norm float64
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("not case/punctuation insensitive at %d", i)
		}
		if a[i] < 0 {
			t.Fatalf("negative component at %d", i)
		}
		norm += float64(a[i]) * float64(a[i])
	}
	if math.Abs(norm-1) > 1e-6 {
		t.Fatalf("not unit length: %f", norm)
	}
	for _, empty := range []string{"", "  !!! ---"} {
		for _, x := range Embed(empty) {
			if x != 0 {
				t.Fatalf("%q produced a non-zero vector", empty)
			}
		}
	}
}

func TestCosine(t *testing.T) {
	v := Embed("cache invalidation")
	if c := Cosine(v, v); math.Abs(c-1) > 1e-6 {
		t.Fatalf("self similarity %f", c)
	}
	if c := Cosine([]float32{1, 0}, []float32{0, 1}); c != 0 {
		t.Fatalf("orthogonal %f", c)
	}
	if c := Cosine([]float32{1, 0}, []float32{-1, 0}); c != -1 {
		t.Fatalf("opposite %f", c)
	}
	if Cosine([]float32{1}, []float32{1, 0}) != 0 || Cosine([]float32{0, 0}, []float32{1, 0}) != 0 {
		t.Fatal("mismatched or zero vectors must score 0")
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	v := Embed("round trip")
	decoded := Decode(Encode(v), Dim)
	if len(decoded) != Dim {
		t.Fatalf("decoded %d values", len(decoded))
	}
	for i := range v {
		if decoded[i] != v[i] {
			t.Fatalf("value %d changed", i)
		}
	}
	if Decode(Encode(v)[:Dim*4-1], Dim) != nil {
		t.Fatal("short buffer decoded")
	}
}
