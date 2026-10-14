#!/usr/bin/env bash
# The fixture for the opt-in STEPS_TEST_DO_* tests: do:// only launches, so the tests make their own droplets, and what fakes cannot show is real cloud-init timing and that DigitalOcean's images honor ssh_keys/ssh_authorized_keys for root; this makes the one thing those droplets need, a Cloud Firewall admitting port 22 from YOUR address only to the `steps` tag every do:// droplet carries. Needs doctl (`doctl auth init`) and DIGITALOCEAN_ACCESS_TOKEN for steps; a test droplet costs under a cent.
set -eEuo pipefail

NAME=steps-test
REGION="${STEPS_TEST_DO_REGION:-nyc3}"

say() { printf '%s\n' "$*" >&2; }
die() { printf 'do-fixture: %s\n' "$*" >&2; exit 1; }

command -v doctl >/dev/null 2>&1 || die "doctl is required"

# The steps-host tag value: steps lowercases the hostname and maps anything outside [a-z0-9_-] to a dash.
host_tag() {
	printf 'steps-host:%s' "$(hostname | tr 'A-Z' 'a-z' | sed 's/[^a-z0-9_-]/-/g' | cut -c1-63)"
}

firewall_id() {
	doctl compute firewall list --format ID,Name --no-header | awk -v n="$NAME" '$2 == n { print $1 }'
}

print_env() {
	cat <<EOF
export STEPS_TEST_DO_REGION=$REGION
# and DIGITALOCEAN_ACCESS_TOKEN, which steps reads; this script does not print a secret
EOF
}

up() {
	if [ -z "$(firewall_id)" ]; then
		ip=$(curl -fsS https://ifconfig.me)
		say "creating firewall $NAME: tcp/22 from $ip/32 to droplets tagged steps"
		doctl compute firewall create --name "$NAME" \
			--inbound-rules "protocol:tcp,ports:22,address:$ip/32" \
			--outbound-rules "protocol:tcp,ports:all,address:0.0.0.0/0,address:::/0 protocol:udp,ports:all,address:0.0.0.0/0,address:::/0 protocol:icmp,address:0.0.0.0/0,address:::/0" \
			--tag-names steps >/dev/null
	fi

	print_env
}

down() {
	tag=$(host_tag)

	# Only this host's: a steps web elsewhere may be mid-job on its own droplets.
	for id in $(doctl compute droplet list --tag-name "$tag" --format ID --no-header); do
		say "deleting droplet $id ($tag)"
		doctl compute droplet delete --force "$id"
	done

	fw=$(firewall_id)
	if [ -n "$fw" ]; then
		say "deleting firewall $NAME"
		doctl compute firewall delete --force "$fw"
	fi
}

case "${1:-}" in
up) up ;;
env) print_env ;;
down) down ;;
*) die "usage: $0 up (create the firewall, print the env) | env (re-print it) | down (delete the firewall and every droplet this host's steps left)" ;;
esac
