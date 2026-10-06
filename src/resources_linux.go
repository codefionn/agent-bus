//go:build linux

package bus

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func readResourceMemory() (available, total uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 3 || fields[2] != "kB" {
			continue
		}
		value, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil || value > ^uint64(0)/1024 {
			continue
		}
		switch fields[0] {
		case "MemAvailable:":
			available = value * 1024
		case "MemTotal:":
			total = value * 1024
		}
	}
	if err := s.Err(); err != nil {
		return 0, 0, err
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("MemTotal missing from /proc/meminfo")
	}
	return available, total, nil
}

func readResourceCgroupUsage(path string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(path, "memory.current"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

func readResourceCgroupPopulated(path string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" {
			return fields[1] == "1", nil
		}
	}
	return false, fmt.Errorf("populated missing from %s/cgroup.events", path)
}

// A scope may start before SetCgroup runs. If its wrapper has died, find the
// unit by basename so a still-running scope keeps its reservation.
func readResourceUnitPopulated(unit string) (bool, error) {
	if unit == "" || filepath.Base(unit) != unit {
		return false, nil
	}
	searchRoot := "/sys/fs/cgroup"
	userRoot := filepath.Join(searchRoot, "user.slice", fmt.Sprintf("user-%d.slice", os.Getuid()))
	if info, err := os.Stat(userRoot); err == nil && info.IsDir() {
		searchRoot = userRoot
	}
	var found bool
	err := filepath.WalkDir(searchRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) || errors.Is(walkErr, os.ErrPermission) {
				return nil
			}
			return walkErr
		}
		if !d.IsDir() || d.Name() != unit {
			return nil
		}
		populated, err := readResourceCgroupPopulated(path)
		if err != nil {
			return err
		}
		found = populated
		return fs.SkipAll
	})
	return found, err
}
