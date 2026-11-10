#!/usr/bin/env bash
# Runs the supabase-js compatibility suite against a running IvaraBase.
# Uses the CLI login (ivarabase login) or IVARA_URL / IVARA_TOKEN (admin token).
#   make compat            # creates/updates the compat_test project, then runs the suite
#   KEEP=1 make compat     # keep the project afterwards (default: delete it)
set -euo pipefail
cd "$(dirname "$0")/.."
B="$PWD/bin/ivarabase"
[ -x "$B" ] || GOTOOLCHAIN=${GOTOOLCHAIN:-local} go build -o bin/ivarabase ./cmd/ivarabase
[ -d tests/compat/node_modules ] || (cd tests/compat && npm install --no-audit --no-fund --loglevel=error)

echo "==> setting up the compat_test project from tests/compat/project/ivarabase.yaml"
(cd tests/compat/project && "$B" apply --yes >/dev/null)

API=$("$B" projects api --project compat_test)
export SUPABASE_URL=$(echo "$API" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).url))')
export SUPABASE_ANON_KEY=$(echo "$API" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).anon_key))')
export SUPABASE_SERVICE_ROLE_KEY=$(echo "$API" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).service_role_key))')
# talk to the server we were pointed at (works without DNS for the project subdomain)
if [ -n "${IVARA_URL:-}" ]; then export SUPABASE_URL="${IVARA_URL%/}/proj/compat_test"; fi
echo "==> running supabase-js against $SUPABASE_URL"

status=0
node tests/compat/compat.mjs || status=$?

# console output of the multi function must be captured (stdout and stderr)
if "$B" functions console --project compat_test --name multi | grep -q "multi called for ivara" &&
   "$B" functions console --project compat_test --name multi | grep -q "multi stderr line"; then
  echo "PASS  functions console.log / console.error captured"
else
  echo "FAIL  functions console.log / console.error captured"; status=1
fi
if [ -z "${KEEP:-}" ]; then
  "$B" projects delete --project compat_test --confirm >/dev/null && echo "==> removed compat_test (KEEP=1 keeps it)"
fi
exit $status
