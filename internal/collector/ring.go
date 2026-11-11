package collector

// Ring es un búfer circular de capacidad fija: al llenarse, cada elemento
// nuevo sustituye al más antiguo. No es seguro para uso concurrente.
type Ring[T any] struct {
	buf   []T
	start int // índice del elemento más antiguo
	n     int // elementos almacenados
}

// NewRing crea un búfer con la capacidad indicada (mínimo 1).
func NewRing[T any](capacity int) *Ring[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring[T]{buf: make([]T, capacity)}
}

// Push añade un elemento, descartando el más antiguo si está lleno.
func (r *Ring[T]) Push(v T) {
	if r.n < len(r.buf) {
		r.buf[(r.start+r.n)%len(r.buf)] = v
		r.n++
		return
	}
	r.buf[r.start] = v
	r.start = (r.start + 1) % len(r.buf)
}

// Len devuelve el número de elementos almacenados.
func (r *Ring[T]) Len() int { return r.n }

// Last devuelve hasta n elementos, del más antiguo al más reciente.
// Con n <= 0 o mayor que Len devuelve todos.
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
