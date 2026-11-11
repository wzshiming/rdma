package rdma

// direction is what a wait on a stream waits for.
type direction int

const (
	readable direction = iota // data, or the end of it, to read
	writable                  // room to write
)
