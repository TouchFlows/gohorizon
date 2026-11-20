//go:build !linux

package process

// Probe and Scan are unavailable off Linux. Adoption is disabled rather than
// approximated with something that could adopt the wrong process.
func Probe(int) (ProcInfo, error) { return ProcInfo{}, ErrProbeUnsupported }

func Scan() ([]ProcInfo, error) { return nil, ErrProbeUnsupported }
