package nibash

import "testing"

func TestAFailureTheExitStatusDoesNotTellIsNamedWhereItCouldGoUnseen(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	for _, c := range []struct{ command, want string }{
		// Its standard error went nowhere, and the call went on.
		{"sh -c 'echo out; exit 3' 2>/dev/null; echo done", "out\ndone\n[nibash: sh -c 'echo out; exit 3' exited 3, its stderr discarded]"},
		{"sh -c 'exit 3' >/dev/null 2>&1; echo done", "done\n[nibash: sh -c 'exit 3' exited 3, its stderr discarded]"},
		// || true only silences it.
		{"sh -c 'exit 7' 2>/dev/null || true", "[nibash: sh -c 'exit 7' exited 7, its stderr discarded]"},
		// An && chain skipped the rest, and the call went on.
		{"sh -c 'exit 4' && echo never && echo nor; echo after", "after\n[nibash: sh -c 'exit 4' exited 4, so echo never && echo nor did not run]"},
		// A loop names a command once.
		{"for i in 1 2; do sh -c 'exit 2' 2>/dev/null; done; echo end", "end\n[nibash: sh -c 'exit 2' exited 2 (2 times), its stderr discarded]"},
		// Anything else after ||, or a command reading $?, handles the failure.
		{"sh -c 'exit 6' 2>/dev/null || echo handled", "handled"},
		{"sh -c 'exit 8' 2>/dev/null; echo status=$?", "status=8"},
		{"if sh -c 'exit 5' 2>/dev/null; then echo yes; fi; echo checked", "checked"},
		// A failure whose message showed needs no note, nor does grep's answer that nothing matched.
		{"sh -c 'echo oops >&2; exit 3'; echo done", "oops\ndone"},
		{"grep -q zzz a.txt 2>/dev/null; echo searched", "searched"},
		// The closing line names the failure the call exits with, among several commands.
		{"echo a && sh -c 'exit 5' && echo b", "a\n[exit 5: sh -c 'exit 5']"},
		{"sh -c 'exit 9'", "[exit 9]"},
		// No program sees the step nibash gives it.
		{"printenv NIBASH_STEP; echo $?", "1"},
		// A stage before a pipeline's last fails where the pipeline's status cannot say so; one SIGPIPE ended as head stopped reading did not.
		{"sh -c 'echo FAIL x; exit 1' | grep FAIL", "FAIL x\n[nibash: sh -c 'echo FAIL x; exit 1' exited 1 before | grep FAIL]"},
		{"seq 1 200000 | head -1", "1"},
		{"if sh -c 'exit 3' | grep -q x; then echo y; fi; echo n", "n"},
	} {
		expect(t, f.run(c.command), c.want)
	}
}
