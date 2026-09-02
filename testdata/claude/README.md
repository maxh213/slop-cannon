# Claude Code stream-json fixtures

Real output of Claude Code **2.1.258** (`claude --version`), captured on
2026-09-02 with `claude -p --output-format stream-json --verbose --model
claude-haiku-4-5-20251001` and tiny prompts. Each `.jsonl` file is the
subprocess's complete stdout, one JSON object per line; a `.stderr` file
holds stderr when there was any. Machine-specific paths, the login email and
the organisation id were replaced (`/work/wt`, `/home/user`,
`user@example.com`, all-zero org id); nothing else was edited, including the
plain-text line the CLI printed after the result in `text-reply.jsonl`.

Runs were made with the `CLAUDECODE` and `CLAUDE_CODE_*` variables of the
capturing session removed from the environment, which is what the adapter
does for every child.

| File | Command (after the common flags) | What it shows |
|---|---|---|
| `text-reply.jsonl` | `--max-turns 1 "Reply with exactly the word PONG"` | init, `rate_limit_event`, `thinking_tokens`, a thinking block, one text block, `result` (`subtype: success`, `total_cost_usd`, `usage`, `num_turns`), then a stray non-JSON stdout line |
| `tool-use-read.jsonl` | `--max-turns 3 --permission-mode dontAsk --tools Read,Glob,Grep --append-system-prompt "…" "Use the Read tool to read hello.txt …"` | `tool_use` (Read) and `user`/`tool_result` lines, two assistant messages, `num_turns: 2` |
| `api-error-unknown-model.jsonl` + `.stderr` | `--model claude-no-such-model-999 --max-turns 1 "…PONG"` | an API failure: synthetic assistant text, `result` with `subtype: success` but `is_error: true`, `api_error_status: 404`, `terminal_reason: api_error`; exit status 1; the `[claude-code:unrecognized_model]` tag on stderr |
| `error-max-turns.jsonl` | prompt on **stdin**, `--max-turns 1 --permission-mode dontAsk` asking for a Read | `result` with `subtype: error_max_turns`, `errors: [...]`, no `result` field; exit status 1 |
| `tool-disallowed.jsonl` | `--max-turns 2 --permission-mode dontAsk --disallowedTools Write,Edit,Bash` asking for a Write | `ToolSearch` then `Write`; the `tool_result` with `is_error: true` ("No such tool available: Write"); ends in `error_max_turns` |
| `shell-denied.jsonl` | prompt on stdin, `--max-turns 4 --permission-mode dontAsk --allowedTools 'Bash(ls:*),Bash(cat:*)' --disallowedTools 'Edit,Write,NotebookEdit'` asking for `ls` then `touch zzz.txt` | one allowed and one denied Bash call under `dontAsk`, `permission_denials` populated in `result`; `zzz.txt` was not created |
| `auth-status.json` | `claude auth status --json` | the auth probe's shape (`loggedIn`, `authMethod`, `email`, `subscriptionType`) |
| `version.txt` | `claude --version` | `2.1.258 (Claude Code)` |

Regenerate with the commands above from any directory containing a
`hello.txt`; keep `--model claude-haiku-4-5-20251001 --max-turns N` so a
capture costs a few cents at most, and re-run the sanitiser replacements
before committing.
