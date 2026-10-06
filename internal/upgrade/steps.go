package upgrade

import (
	"fmt"
	"slices"
)

// Steps a run records, in the order they happen. The way back reads them to
// know what to undo (Decision 11), and they are a state machine (spec 054
// #31): each kind of run has a table of the steps it records and, for each,
// the steps it may follow. j.step records along the table; a state whose
// steps do not follow it is not a run's; the fault matrix takes its cells
// from the table.
const (
	stepPrepared        = "prepared"         // nothing changed yet
	stepStopSent        = "stop_sent"        // SIGTERM sent
	stepStopped         = "stopped"          // the old server is down
	stepArchived        = "archived"         // data.tar.gz read back whole
	stepBinaryReplacing = "binary_replacing" // the new binary may be at the install path from here
	stepBinaryReplaced  = "binary_replaced"  // the installed binary is the new one
	stepStarted         = "started"          // the new server runs
	stepChecked         = "checked"          // the verdict is in `verdict`
	stepSkill           = "skill"            // the skill's copies reinstalled
	stepBackBegun       = "back_begun"       // the way back changes what runs from here
	stepBackRestored    = "back_restored"    // the archive restored beside the data, and checked
	stepBackCleared     = "back_cleared"     // the install path holds the old version or nothing
	stepBackAside       = "back_set_aside"   // what the new version left is set aside
	stepBackMoved       = "back_moved"       // the restore is in the data's place
	stepBackBinary      = "back_binary"      // the old binary is back
	stepBackStarted     = "back_started"     // the old version runs again
	stepBackDone        = "back_done"        // the way back finished, and the old version is healthy
)

// machine is one kind of run's table: each step, and the steps it may follow;
// "" is a run's beginning.
type machine map[string][]string

// swapSteps are the steps after which a way back may begin: everything from
// the stop on.
func swapSteps(m machine) []string {
	var out []string
	for _, s := range []string{stepStopSent, stepStopped, stepArchived, stepBinaryReplacing, stepBinaryReplaced, stepStarted, stepChecked, stepSkill} {
		if _, ok := m[s]; ok {
			out = append(out, s)
		}
	}
	return out
}

var machines = func() map[string]machine {
	process := machine{
		stepPrepared:        {""},
		stepStopSent:        {stepPrepared},
		stepStopped:         {stepStopSent},
		stepArchived:        {stepStopped},
		stepBinaryReplacing: {stepArchived},
		stepBinaryReplaced:  {stepBinaryReplacing},
		stepStarted:         {stepArchived, stepBinaryReplaced},
		stepChecked:         {stepStarted},
		stepSkill:           {stepChecked},
		stepBackRestored:    {stepBackBegun},
		// After a kept server (#37 (c)) that has exited since: the way back
		// that kept it cleared nothing, and the next one clears the path
		// before it starts the old version again (#39).
		stepBackCleared: {stepBackBegun, stepBackRestored, stepBackStarted, stepBackDone},
		stepBackAside:   {stepBackCleared},
		stepBackMoved:   {stepBackAside},
		stepBackBinary:  {stepBackCleared, stepBackMoved},
		// Again after it is done: a --back repeated finds the old version
		// stopped since, and starts it (#31).
		stepBackStarted: {stepBackBegun, stepBackBinary, stepBackStarted, stepBackDone},
		// Nothing to undo — a run cut short before its stop, or before its
		// first step — is a way back done (#34).
		stepBackDone: {"", stepPrepared, stepBackStarted},
	}
	process[stepBackBegun] = swapSteps(process)

	binary := machine{
		stepPrepared:        {""},
		stepBinaryReplacing: {stepPrepared},
		stepBinaryReplaced:  {stepBinaryReplacing},
		stepSkill:           {stepBinaryReplaced},
		stepBackBegun:       {stepBinaryReplacing, stepBinaryReplaced, stepSkill},
		stepBackBinary:      {stepBackBegun},
		stepBackDone:        {"", stepPrepared, stepBackBinary},
	}
	return map[string]machine{kindProcess: process, kindBinary: binary}
}()

// allows says whether next may be recorded after last in a run of kind.
func allows(kind, last, next string) bool {
	m, ok := machines[kind]
	return ok && slices.Contains(m[next], last)
}

// last is the step a run is at: the last it recorded, or "".
func (s *State) last() string {
	if len(s.Steps) == 0 {
		return ""
	}
	return s.Steps[len(s.Steps)-1].Name
}

// followsTable says whether a state's steps are a walk of its kind's table.
func (s *State) followsTable() error {
	last := ""
	for _, st := range s.Steps {
		if !allows(s.Kind, last, st.Name) {
			return fmt.Errorf("its step %q does not follow %q", st.Name, last)
		}
		last = st.Name
	}
	return nil
}

// badStep is told of a step about to be recorded off the table: a fault in
// the command, which the tests turn into a failure. The step is never
// written; job.step then ends the run stuck (#34).
var badStep = func(kind, last, next string) {}
