# Cursor Agent fixtures

Captured on 2026-09-29 from `cursor-agent` `2026.09.28-64d2043` (logged in),
each in a throwaway git repository with an `a.txt` containing `hi` or `hello`.
Paths were rewritten to `/tmp/sc-fixture` and the account e-mail to
`user@example.com`. `B` stands for
`cursor-agent -p --output-format stream-json --force --trust --sandbox disabled`.

| File | Command |
|---|---|
| `tools-reply.jsonl` | `echo "Run the shell command 'cat a.txt', then create a file b.txt containing the word done, then reply with exactly: OK" \| B --model auto` |
| `shell-failure.jsonl` | `echo "Run the shell command 'ls /definitely/not/here' and then reply with exactly: DONE" \| B --model auto` |
| `read-grep.jsonl` | `echo "Read the file a.txt with your file-reading tool, search for the word hi with your grep/search tool, then reply with exactly: READ-OK" \| B --model auto` |
| `long-shell.jsonl` | `echo "Run exactly this shell command and report its full output: sleep 50 && echo FINISHED-LONG. Then reply with the output you saw." \| B --model auto` (the model picked a 60 s timeout itself) |
| `background-shell.jsonl` | `echo "Run this shell command with the shell tool's timeout set to 5000 ms …: sleep 40 && echo LATE > marker.txt && echo WROTE. Then tell me whether marker.txt exists and its contents." \| B --model auto` (the command is backgrounded after 5 s; print mode waits for it and wakes the model) |
| `unknown-model.stderr` | `echo "say hi" \| B --model no-such-model-xyz` (exit 1, empty stdout) |
| `logged-out.stderr` | `echo "say hi" \| HOME=<empty dir> XDG_CONFIG_HOME=<empty dir>/.config cursor-agent -p --output-format stream-json --force --trust --model auto` (exit 1, empty stdout) |
| `status-logged-in.txt` | `cursor-agent status` |
| `status-logged-out.txt` | `HOME=<empty dir> XDG_CONFIG_HOME=<empty dir>/.config cursor-agent status` (exit 0) |
| `version.txt` | `cursor-agent --version` |

No failing `result` line (`is_error: true`) was observed; the unit tests
build one by hand.
