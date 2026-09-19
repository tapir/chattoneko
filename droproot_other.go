//go:build !linux

package main

// dropRoot does nothing off Linux: there is no container to drop out of root
// in, and no setuid to drop with.
func dropRoot(string) error { return nil }
