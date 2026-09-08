#!/bin/sh
# check-i18n-cjk.sh — guard against CJK characters leaking back into the
# frontend after the i18n migration.
#
# Everything user-facing must come from the resource trees under
# frontend/src/i18n/locales/. A leftover Chinese literal in a component means
# that string is frozen in one language (and unreviewable by translators).
#
# Whitelist (protocol literals — translating these would BREAK features):
#   - src/utils/logLines.ts      THINKING_PREFIX coalesces the SSE heartbeat
#                                "🤔 模型思考中… (N tokens)" lines; the prefix
#                                must match the backend's Chinese phase text.
#   - src/utils/phaseGroups.ts   groups phase lines by their backend-emitted
#                                Chinese prefixes.
#   - src/api/client.ts          DefaultModelLabel ('默认模型') mirrors the
#                                backend handler.DefaultModelLabel and is
#                                compared with `===`.
#   - src/components/CodingChat.tsx
#                                '用户: ' / 'AI: ' composer prefixes that are
#                                fed back to the LLM as speaker markers.
#   - any line carrying the `// i18n: protocol literal` marker comment.
#
# Implementation notes: written for BusyBox grep — no `grep -P`. The CJK range
# is expressed as a byte-class `[一-鿿]` (U+4E00–U+9FFF) which busybox matches
# as an ordinary bracket expression on the UTF-8 bytes. Only frontend/src is
# scanned; the resource trees themselves are obviously all CJK.

set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$ROOT/frontend/src"

fail=0
checked=0

# is_whitelisted <file> <line> — exits 0 when the line is allowed.
is_whitelisted() {
	_file="$1"
	_line="$2"
	case "$_line" in
		# Explicit opt-out marker, one per line that must stay literal.
		*"// i18n: protocol literal"*) return 0 ;;
	esac
	case "$_file" in
		*/i18n/constants.ts)
			# nativeLabel is deliberately the language's own name (中文/English):
			# language names are never translated.
			case "$_line" in
				*nativeLabel*) return 0 ;;
			esac
			;;
		*/utils/logLines.ts | */utils/phaseGroups.ts)
			return 0
			;;
		*/api/client.ts)
			case "$_line" in
				*DefaultModelLabel*) return 0 ;;
			esac
			;;
		*/components/CodingChat.tsx)
			case "$_line" in
				*"用户: "* | *"AI: "*) return 0 ;;
			esac
			;;
	esac
	return 1
}

# scan <file>
scan() {
	_f="$1"
	_ln=0
	while IFS= read -r _line; do
		_ln=$((_ln + 1))
		case "$_line" in
			*[一-鿿]*)
				if is_whitelisted "$_f" "$_line"; then
					continue
				fi
				if [ "$fail" -eq 0 ]; then
					echo "CJK literals found outside i18n resource trees:" >&2
				fi
				echo "  ${_f#$ROOT/}:$_ln: $_line" >&2
				fail=1
				;;
		esac
	done < "$_f"
	checked=$((checked + 1))
}

# The resource trees are the translations themselves — skip them.
for f in $(find "$SRC" -type f \( -name '*.ts' -o -name '*.tsx' \) | sort); do
	case "$f" in
		*/i18n/locales/*) continue ;;
	esac
	scan "$f"
done

if [ "$fail" -ne 0 ]; then
	echo >&2
	echo "Move these strings into frontend/src/i18n/locales/modules/ and render" >&2
	echo "them with t(). Protocol literals that must stay Chinese carry the" >&2
	echo "// i18n: protocol literal marker." >&2
	exit 1
fi

echo "check-i18n-cjk: OK ($checked files scanned)"
exit 0
