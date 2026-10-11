//go:build !linux && !(darwin && cgo)

package discover

// nothing is the system where there is no way built in to ask what is
// listening (Windows, or a macOS build without cgo). Scans still find
// projects in folders and from `leadline run`.
type nothing struct{}

func hostSystem() system { return nothing{} }

func (nothing) sockets() ([]rawSocket, Limits) {
	return nil, Limits{Method: "none", Note: "Leadline can't see what is listening on this system."}
}

func (nothing) process(int) (procInfo, bool) { return procInfo{}, false }
