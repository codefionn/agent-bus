//go:build !linux

package bus

import "fmt"

func readResourceMemory() (available, total uint64, err error) {
	return 0, 0, fmt.Errorf("RAM reservations are supported on Linux only")
}

func readResourceCgroupUsage(path string) (uint64, error) {
	return 0, fmt.Errorf("cgroup memory accounting is supported on Linux only")
}

func readResourceCgroupPopulated(path string) (bool, error) {
	return false, fmt.Errorf("cgroup inspection is supported on Linux only")
}

func readResourceUnitPopulated(unit string) (bool, error) {
	return false, nil
}
