package collector

// Ring is a fixed-capacity circular buffer: once full, each new element
// replaces the oldest one. It is not safe for concurrent use.
type Ring[T any] struct {
	buf   []T
	start int // index of the oldest element
	n     int // stored elements
}

// NewRing creates a buffer with the given capacity (at least 1).
func NewRing[T any](capacity int) *Ring[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring[T]{buf: make([]T, capacity)}
}

// Push adds an element, dropping the oldest one if the buffer is full.
func (r *Ring[T]) Push(v T) {
	if r.n < len(r.buf) {
		r.buf[(r.start+r.n)%len(r.buf)] = v
		r.n++
		return
	}
	r.buf[r.start] = v
	r.start = (r.start + 1) % len(r.buf)
}

// Len returns the number of stored elements.
func (r *Ring[T]) Len() int { return r.n }

// Last returns up to n elements, oldest first. With n <= 0 or n > Len it
// returns all of them.
func (r *Ring[T]) Last(n int) []T {
	if n <= 0 || n > r.n {
		n = r.n
	}
	out := make([]T, n)
	first := r.n - n
	for i := range out {
		out[i] = r.buf[(r.start+first+i)%len(r.buf)]
	}
	return out
}

// Resized returns a buffer of the given capacity holding the most recent
// elements of r that fit.
func (r *Ring[T]) Resized(capacity int) *Ring[T] {
	n := NewRing[T](capacity)
	for _, v := range r.Last(len(n.buf)) {
		n.Push(v)
	}
	return n
}
