// Package embed provides a deterministic, local, dependency-free embedding
// used as a fallback semantic ranker. It is intentionally simple: a hashed
// bag-of-words projection. Real semantic models can replace it behind the
// same Embed signature without changing callers.
package embed

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

const Dim = 256

// Embed projects text into a fixed-size vector. Deterministic and offline.
func Embed(text string) []float32 {
	v := make([]float32, Dim)
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	for _, f := range fields {
		h := fnv.New32a()
		h.Write([]byte(f))
		idx := int(h.Sum32()) % Dim
		v[idx] += 1
	}
	// L2 normalize.
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return v
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}

func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func Encode(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

func Decode(b []byte, dim int) []float32 {
	if len(b) < dim*4 {
		return nil
	}
	v := make([]float32, dim)
	for i := 0; i < dim; i++ {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}