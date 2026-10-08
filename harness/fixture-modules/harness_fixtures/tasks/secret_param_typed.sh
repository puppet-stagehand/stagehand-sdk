#!/bin/sh
# Bolt fixture task whose token parameter is typed Sensitive[String]. Bolt
# rejects a plain JSON string for that type, so the harness never gets this far;
# the body only exists so the task is well formed. It never prints the token.
input=$(cat)
token=$(printf '%s' "$input" | sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
printf '{"token_length": %s}\n' "${#token}"
