//go:build !linux && !windows

package server

func readOperationsHostCounters() operationsHostCounters { return operationsHostCounters{} }
