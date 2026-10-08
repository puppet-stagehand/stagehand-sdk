#!/bin/sh
# Bolt fixture task: echoes its "message" parameter back as JSON. Bolt passes
# parameters as PT_<name> environment variables (input_method: environment).
# Later harness plans use this to prove exit codes, JSON shape and target
# handling against a real Bolt without touching a real node.
printf '{"message": "%s"}\n' "${PT_message}"
