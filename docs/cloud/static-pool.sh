#!/bin/sh
# Lease runners you already have, one job owner at a time.
#
#   static-pool.sh acquire POOL_FILE
#   static-pool.sh release POOL_FILE
#
# POOL_FILE lists one runner URL per line, such as http://gb10:7443. Each
# machine must already run errand. A lease claims the first free URL; the
# claim lives in POOL_FILE.d/ until release. See docs/CLOUD.md.
set -eu

verb=$1
pool=$2
claims="$pool.d"
mkdir -p "$claims"

case "$verb" in
acquire)
	while IFS= read -r url; do
		[ -n "$url" ] || continue
		slot="$claims/$(printf '%s' "$url" | cksum | cut -d' ' -f1)"
		# A symlink naming the lease is created atomically, together with
		# its owner, so two leases never claim the same machine and no claim
		# is ever left without the lease that release looks for.
		if ln -s "$ERRAND_LEASE_ID" "$slot" 2>/dev/null; then
			echo "claimed $url" >&2
			printf '{"url":"%s","slot":"%s"}\n' "$url" "$slot"
			exit 0
		fi
	done <"$pool"
	echo "every machine in $pool is leased" >&2
	exit 1
	;;
release)
	# Release must be safe to repeat and must work when acquire was cut
	# short, so find the claim by lease ID rather than trusting the state.
	for slot in "$claims"/*; do
		[ -L "$slot" ] || continue
		if [ "$(readlink "$slot")" = "$ERRAND_LEASE_ID" ]; then
			rm -f "$slot"
			echo "released $slot" >&2
		fi
	done
	exit 0
	;;
*)
	echo "usage: $0 acquire|release POOL_FILE" >&2
	exit 2
	;;
esac
