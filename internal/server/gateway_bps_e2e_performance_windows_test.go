package server

import (
	"syscall"
	"unsafe"
)

// Go's Windows monotonic clock can expose quantized interrupt-time ticks.
// QueryPerformanceCounter is required for sub-millisecond loopback TTFTs.
func init() {
	kernel := syscall.NewLazyDLL("kernel32.dll")
	frequencyProc := kernel.NewProc("QueryPerformanceFrequency")
	counterProc := kernel.NewProc("QueryPerformanceCounter")
	var frequency int64
	if ok, _, _ := frequencyProc.Call(uintptr(unsafe.Pointer(&frequency))); ok == 0 || frequency <= 0 {
		panic("QueryPerformanceFrequency unavailable for gateway performance clock")
	}
	bpsPerfNow = func() int64 {
		var counter int64
		if ok, _, _ := counterProc.Call(uintptr(unsafe.Pointer(&counter))); ok == 0 {
			panic("QueryPerformanceCounter failed")
		}
		return counter/frequency*1e9 + counter%frequency*1e9/frequency
	}
	bpsPerfClock = "Windows QueryPerformanceCounter"
}
