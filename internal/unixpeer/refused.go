package unixpeer

// ConnectionRefused reports whether a dial failed because nothing listens on
// the socket, as with a stale socket file left by an exited daemon.
func ConnectionRefused(err error) bool { return connectionRefused(err) }
