#!/usr/bin/env bash

fetch_cached() {
	local name="$1" url="$2" target="$CACHE_DIR/$1" temporary
	if [[ ! -s "$target" ]]; then
		printf 'Downloading Alpine %s %s into %s\n' "$ALPINE_VERSION" "$name" "$CACHE_DIR" >&2
		temporary="$(mktemp "$CACHE_DIR/.${name}.XXXXXX")"
		if ! curl --fail --location --retry 3 --output "$temporary" "$url"; then
			rm -f -- "$temporary"
			return 1
		fi
		mv -f -- "$temporary" "$target"
	fi
}
