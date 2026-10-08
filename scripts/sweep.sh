#!/bin/sh
# Pre-publication sweep — HARNESS-SPEC.md §13.
#
# The mechanical guard (cli/guard_test.go) covers cli/** and desktop/**. This
# script is the same rule over the rest of the workspace: documents, the proxy,
# and the gateway sources. The gateway tree is *expected* to match heavily — its
# job is to name and route to vendors (SPEC.md decision #14). The harness
# surfaces and the public docs are expected to match nothing.
#
# Exit 1 if any harness surface (cli, desktop) or the public design document
# (cli/README.md) names a provider or model. Matches elsewhere are reported as
# context, not failures.
set -u

root="$(cd "$(dirname "$0")/../.." && pwd)"
pattern='(?i)(deep ?seek|open ?router|anthropic|openai|qwen|gpt-?[0-9o]|mimo|claude|sonnet|opus|haiku|gemini|llama|mistral|grok|kimi|moonshot|glm-[0-9]|command-r|vertex|bedrock|azure)'

fails=0
while IFS= read -r f; do
	rel=${f#"$root"/}
	hits=$(grep -incE "$pattern" "$f" 2>/dev/null) || hits=0
	[ "$hits" -gt 0 ] || continue
	case "$rel" in
		gateway/*|proxy/*|HARNESS-SPEC.md)
			echo "context  $rel: $hits line(s) name a provider or model (expected; not a failure)"
			;;
		cli/guard_test.go|cli/scripts/sweep.sh)
			# Contain the patterns by construction.
			;;
		cli/*|desktop/*|OAUTH-PLAN.md)
			echo "FAIL     $rel: $hits line(s) name a provider or model — must not ship"
			fails=$((fails + 1))
			;;
		scripts/claude-censi.sh|scripts/harness-censi.sh|SPEC.md)
			# Internal-only: dev launchers and the gateway spec are not published.
			echo "internal $rel: $hits line(s) (internal-only surface; excluded from publication)"
			;;
		*)
			echo "review   $rel: $hits line(s) — classify before publishing"
			fails=$((fails + 1))
			;;
	esac
done <<EOF
$(find "$root" \
	\( -path "$root/gateway/node_modules" -o -path "$root/gateway/vendor" \
		-o -path "$root/desktop/build" -o -path "$root/desktop/frontend/dist" \
		-o -path "$root/.git" \) -prune -o \
	-type f \( -name '*.go' -o -name '*.md' -o -name '*.php' -o -name '*.json' \
		-o -name '*.yaml' -o -name '*.yml' -o -name '*.sh' -o -name '*.js' \
		-o -name '*.vue' -o -name '*.ts' \) -print 2>/dev/null)
EOF

if [ "$fails" -gt 0 ]; then
	echo
	echo "$fails file(s) fail the sweep. Do not publish."
	exit 1
fi
echo
echo "sweep clean: no harness surface names a provider or model."
