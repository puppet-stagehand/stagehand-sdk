#!/bin/sh
# Bolt fixture task with a sensitive parameter delivered on stdin as JSON. It
# prints only the length of the token, never the value, so a leak found later
# in Bolt's output or log is Bolt's doing and not this task's.
input=$(cat)
token=$(printf '%s' "$input" | sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
printf '{"token_length": %s}\n' "${#token}"
