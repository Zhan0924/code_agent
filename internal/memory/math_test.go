package memory

import (
	"testing"
)

func TestFormatVector(t *testing.T) {
	tests := []struct {
		name string
		vec  []float32
		want string
	}{
		{"empty", nil, "[]"},
		{"single", []float32{0.5}, "[0.5]"},
		{"multiple", []float32{0.1, 0.2, 0.3}, "[0.1,0.2,0.3]"},
		{"negative", []float32{-0.5, 1.0}, "[-0.5,1]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatVector(tt.vec)
			if got != tt.want {
				t.Errorf("formatVector(%v) = %q, want %q", tt.vec, got, tt.want)
			}
		})
	}
}

func TestParseVector(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want []float32
	}{
		{"empty", "[]", []float32{}},
		{"single", "[0.5]", []float32{0.5}},
		{"multiple", "[0.1,0.2,0.3]", []float32{0.1, 0.2, 0.3}},
		{"negative", "[-0.5,1]", []float32{-0.5, 1.0}},
		{"whitespace", "[ 0.1 , 0.2 ]", []float32{0.1, 0.2}},
		{"invalid", "not a vector", nil},
		{"no brackets", "0.1,0.2", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseVector(tt.s)
			if tt.want == nil {
				if got != nil {
					t.Errorf("parseVector(%q) = %v, want nil", tt.s, got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("parseVector(%q) length = %d, want %d", tt.s, len(got), len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseVector(%q)[%d] = %f, want %f", tt.s, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float64
		tol  float64
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1.0, 0.001},
		{"orthogonal", []float32{1, 0, 0}, []float32{0, 1, 0}, 0.0, 0.001},
		{"opposite", []float32{1, 0, 0}, []float32{-1, 0, 0}, -1.0, 0.001},
		{"similar", []float32{1, 1, 0}, []float32{1, 0, 0}, 0.707, 0.01},
		{"empty", nil, nil, 0.0, 0.001},
		{"length mismatch", []float32{1, 0}, []float32{1, 0, 0}, 0.0, 0.001},
		{"zero vector", []float32{0, 0, 0}, []float32{1, 0, 0}, 0.0, 0.001},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CosineSimilarity(tt.a, tt.b)
			if diff := got - tt.want; diff > tt.tol || diff < -tt.tol {
				t.Errorf("CosineSimilarity(%v, %v) = %f, want %f (tol %f)", tt.a, tt.b, got, tt.want, tt.tol)
			}
		})
	}
}
