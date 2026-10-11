package cli

import "fmt"

const pauseJournalKey = "daemon:paused"

// cmdPause implements both `pause` and `resume`: it writes or
// deletes the pause journal key the daemon checks before every
// sync round. It works whether or not the daemon is running.
func cmdPause(e *env, _ []string, pause bool) int {
	_, st, err := e.openStore()
	if err != nil {
		return e.fail("open store: %v", err)
	}
	defer st.Close()
	if pause {
		if err := st.JournalSet(pauseJournalKey, []byte("1")); err != nil {
			return e.fail("pause: %v", err)
		}
		fmt.Fprintln(e.stdout, "paused: sync rounds are suspended (daemon picks this up within one round)")
		return 0
	}
	if err := st.JournalDelete(pauseJournalKey); err != nil {
		return e.fail("resume: %v", err)
	}
	fmt.Fprintln(e.stdout, "resumed: sync rounds will run again")
	return 0
}
