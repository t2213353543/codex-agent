//go:build windows

package server

import (
	"syscall"
	"unsafe"
)

var operationsKernel32 = syscall.NewLazyDLL("kernel32.dll")
var operationsGetSystemTimes = operationsKernel32.NewProc("GetSystemTimes")
var operationsGlobalMemoryStatus = operationsKernel32.NewProc("GlobalMemoryStatusEx")

func readOperationsHostCounters() operationsHostCounters {
	out := operationsHostCounters{Supported: true}
	var idle, kernel, user syscall.Filetime
	if result, _, _ := operationsGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); result != 0 {
		ticks := func(t syscall.Filetime) uint64 { return uint64(t.HighDateTime)<<32 | uint64(t.LowDateTime) }
		out.Idle, out.Total, out.CPUAvailable = ticks(idle), ticks(kernel)+ticks(user), true
	}
	var memory struct {
		Length, Load                                                                                                                 uint32
		TotalPhysical, AvailablePhysical, TotalPageFile, AvailablePageFile, TotalVirtual, AvailableVirtual, AvailableExtendedVirtual uint64
	}
	memory.Length = uint32(unsafe.Sizeof(memory))
	if result, _, _ := operationsGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&memory))); result != 0 {
		out.MemoryTotal, out.MemoryAvailable = &memory.TotalPhysical, &memory.AvailablePhysical
	}
	return out
}
