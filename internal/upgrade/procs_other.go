//go:build !linux && !darwin

package upgrade

import (
	"errors"
	"syscall"
)

// noSystem is a platform whose processes the command cannot read (Windows,
// the BSDs): it finds no server, and says it could not look.
type noSystem struct{}

func newSystem() System { return noSystem{} }

var errNoProcesses = errors.New("reading processes is not supported on this system")

func (noSystem) Candidates() ([]Process, int, error) { return nil, 0, errNoProcesses }
func (noSystem) Inspect(int) (Process, error)        { return Process{}, errNoProcesses }
func (noSystem) Alive(pid int) bool                  { return false }
func (noSystem) Signal(int, syscall.Signal) error    { return errNoProcesses }
func (noSystem) Start(StartSpec) (Started, error)    { return nil, errNoProcesses }

// isZombie: no process table to read here.
func isZombie(int) bool { return false }
