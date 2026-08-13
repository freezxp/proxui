package httpapi

import "runtime"

// stack captures the current goroutine stack for panic logging.
func stack() []byte {
	buf := make([]byte, 8<<10)
	n := runtime.Stack(buf, false)
	return buf[:n]
}
