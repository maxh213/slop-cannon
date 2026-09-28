//go:build !unix

package proc

// Renice does nothing where Unix niceness does not exist.
func Renice(int) error { return nil }
