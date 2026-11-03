#!/bin/sh
# Create the server's first account by signing in the way the CLI does, then
# print a personal access token: the only output on stdout.
set -eu
apk add -q --no-cache curl jq openssl >&2
S=http://netbird-server
EMAIL=admin@example.com
PASS='Test-Passw0rd-123!'

tries=0
until curl -fs "$S/oauth2/.well-known/openid-configuration" >/dev/null; do
    tries=$((tries + 1))
    [ "$tries" -lt 60 ] || { echo "NetBird server did not start" >&2; exit 1; }
    sleep 1
done
curl -fsS -X POST "$S/api/setup" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\",\"name\":\"Admin\"}" >/dev/null

VERIFIER=test-verifier-test-verifier-test-verifier-1234
CHALLENGE=$(printf %s "$VERIFIER" | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d '=')
REDIRECT=http%3A%2F%2Flocalhost%3A53000%2F
JAR=/tmp/jar
FORM=$(curl -fsSL -c $JAR -b $JAR -o /tmp/login.html -w '%{url_effective}' \
    "$S/oauth2/auth?client_id=netbird-cli&redirect_uri=$REDIRECT&response_type=code&scope=openid+profile+email+offline_access&state=x&code_challenge_method=S256&code_challenge=$CHALLENGE")
# A redirect to the CLI's localhost callback carries the code; don't follow it.
LOCATION=$(curl -s -o /dev/null -c $JAR -b $JAR -w '%{redirect_url}' -X POST "$FORM" \
    --data-urlencode "login=$EMAIL" --data-urlencode "password=$PASS")
while ! printf %s "$LOCATION" | grep -q 'code='; do
    LOCATION=$(curl -s -o /dev/null -c $JAR -b $JAR -w '%{redirect_url}' "$S${LOCATION#"$S"}")
done
CODE=$(printf %s "$LOCATION" | sed -n 's/.*[?&]code=\([^&]*\).*/\1/p')
BEARER=$(curl -fsS "$S/oauth2/token" -d grant_type=authorization_code -d client_id=netbird-cli \
    -d "code=$CODE" -d "code_verifier=$VERIFIER" -d 'redirect_uri=http://localhost:53000/' \
    | jq -re .access_token)

USER=$(curl -fsS "$S/api/users/current" -H "Authorization: Bearer $BEARER" | jq -re .id)
curl -fsS -X POST "$S/api/users/$USER/tokens" -H "Authorization: Bearer $BEARER" -H 'Content-Type: application/json' \
    -d '{"name":"e2e","expires_in":1}' | jq -re .plain_token
