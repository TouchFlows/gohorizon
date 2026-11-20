package process

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

func jsonEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc
}

func jsonUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return dec.Decode(v)
}

// ProcInfo is the identity of a live process, independent of pid reuse.
type ProcInfo struct {
	PID        int
	StartTicks uint64
	State      byte
	Cmdline    []string
}

// HasFlag reports whether the command line contains the exact "flag=value" argument.
func (p ProcInfo) HasFlag(flag, value string) bool {
	want := "--" + flag + "=" + value
	for _, arg := range p.Cmdline {
		if arg == want {
			return true
		}
	}
	return false
}

// Flag returns the value of "--name=value" style arguments.
func (p ProcInfo) Flag(name string) string {
	prefix := "--" + name + "="
	for _, arg := range p.Cmdline {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	return ""
}

func parseProcStat(data []byte, pid int) (ProcInfo, error) {
	info := ProcInfo{PID: pid}
	// /proc/<pid>/stat: "pid (comm) state ppid ..." — comm may contain spaces and
	// parentheses, so split after the last ')'.
	close := bytes.LastIndexByte(data, ')')
	if close < 0 || close+2 > len(data) {
		return ProcInfo{}, strconv.ErrSyntax
	}
	fields := strings.Fields(string(data[close+2:]))
	if len(fields) < 20 {
		return ProcInfo{}, strconv.ErrSyntax
	}
	info.State = fields[0][0]
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return ProcInfo{}, err
	}
	info.StartTicks = start
	return info, nil
}
