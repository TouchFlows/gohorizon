//go:build linux

package process

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// Probe reads /proc/<pid>/stat and /proc/<pid>/cmdline.
func Probe(pid int) (ProcInfo, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ProcInfo{}, ErrProcessGone
		}
		return ProcInfo{}, fmt.Errorf("read process stat: %w", err)
	}
	info, err := parseProcStat(data, pid)
	if err != nil {
		return ProcInfo{}, fmt.Errorf("parse process stat for %d: %w", pid, err)
	}
	// A zombie has exited; only its parent has not reaped it yet. It must never be
	// adopted as a running worker, and it counts as gone for supervision.
	if info.State == 'Z' {
		return ProcInfo{}, ErrProcessGone
	}
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		for _, part := range strings.Split(string(raw), "\x00") {
			if part != "" {
				info.Cmdline = append(info.Cmdline, part)
			}
		}
	}
	return info, nil
}

// Scan lists every process whose command line could be read.
func Scan() ([]ProcInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	var out []ProcInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		info, err := Probe(pid)
		if err != nil || len(info.Cmdline) == 0 {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}
