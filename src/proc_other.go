//go:build !linux && !darwin && !windows

package bus

// Other systems get no process inspection: sessions there need AGENT_BUS_ID and
// never find a harness, so the bus stays read-only.

func procStart(pid int) uint64 { return 0 }

func procInfo(pid int) (*proc, bool) { return nil, false }
