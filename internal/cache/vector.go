package cache

import (
	"encoding/binary"
	"errors"
	"math"
)

func ValidateVector(vector []float32) error {
	if len(vector) < 1 || len(vector) > 65536 {
		return errors.New("invalid embedding dimensions")
	}
	var norm float64
	for _, v := range vector {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return errors.New("non-finite embedding")
		}
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		return errors.New("zero embedding")
	}
	return nil
}
func vectorBytes(v []float32) string {
	b := make([]byte, len(v)*4)
	for i, n := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(n))
	}
	return string(b)
}
