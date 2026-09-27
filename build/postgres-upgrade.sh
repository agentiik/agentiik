#!/bin/sh
# The entry point of ghcr.io/agentiik/postgres-upgrade: it brings a PostgreSQL data directory written
# by an older major version to the newest one this image carries, with pg_upgrade, and keeps the old
# directory beside it as the way back. The Compose file of agentiik/deploy runs it before PostgreSQL
# at every docker compose up, so that a release moving PostgreSQL to a new major version asks nothing
# of the person upgrading.
#
#   postgres-upgrade [DIRECTORY]
#
# DIRECTORY is the cluster, /data/postgres unless named, and the container is given the directory
# holding it, since the old cluster is renamed beside it: postgres becomes postgres-17 and the new
# cluster, built as postgres-18.partial, takes the name postgres. By what DIRECTORY holds:
#
#   nothing, or no PG_VERSION     a new installation, which PostgreSQL's own image initialises: nothing
#   the newest major version      nothing
#   an older one this image has   pg_upgrade --check, pg_upgrade in copy mode, the new cluster started
#                                 once with the old one's configuration, then the two renamed
#   anything else                 refused
#
# A failure leaves DIRECTORY as it was, removes what was built beside it, and says why in a sentence.
# pg_upgrade copies rather than links, so the old cluster is never written to but by the servers
# pg_upgrade starts on it, and starts as the previous release left it.
set -eu

dir=${1:-/data/postgres}
dir=${dir%/}
parent=$(dirname "$dir")
name=$(basename "$dir")

# The account the official image runs PostgreSQL as, uid 70 in Alpine as there, and the superuser it
# makes at initdb, which pg_upgrade connects as and the new cluster has to be created with. The
# Compose file names neither, so the official image's default holds; POSTGRES_USER is read here
# under the name that image reads it.
account=postgres
user=${POSTGRES_USER:-postgres}

say() { printf 'postgres-upgrade: %s\n' "$*"; }
fail() {
	printf 'postgres-upgrade: %s\n' "$*" >&2
	exit 1
}
as_postgres() { su-exec "$account" "$@"; }

# The major versions this image carries, each in /usr/libexec/postgresqlNN as Alpine installs it. The
# newest is the one it upgrades to, so that carrying 19 beside 18 is the whole of the next upgrade.
majors=$(for bin in /usr/libexec/postgresql[0-9]*; do
	if [ -x "$bin/postgres" ]; then echo "${bin##*postgresql}"; fi
done | sort -n)
new=$(echo "$majors" | tail -n 1)
if [ -z "$new" ]; then fail "this image carries no PostgreSQL, so it was built wrong"; fi
newbin=/usr/libexec/postgresql$new

partial=$parent/$name-$new.partial # being built; whatever an earlier run left of it is discarded
ready=$parent/$name-$new.ready     # built and proved, and about to take the name of DIRECTORY
work=/tmp/postgres-upgrade         # pg_upgrade's socket and logs, and the servers' pg_hba.conf
oldbin=

# On any exit but success: a server started on the new cluster is stopped, what pg_upgrade said is
# printed, and the new cluster is removed. The old one is never touched here, and a server pg_upgrade
# left on it, which it stops itself, is stopped cleanly rather than left to a container being killed.
cleanup() {
	status=$?
	trap - EXIT
	if [ "$status" -ne 0 ]; then
		if [ -e "$partial/postmaster.pid" ]; then
			as_postgres "$newbin/pg_ctl" -D "$partial" -m immediate -w stop >/dev/null 2>&1 || true
		fi
		if [ -n "$oldbin" ] && ours "$dir/postmaster.pid"; then
			as_postgres "$oldbin/pg_ctl" -D "$dir" -m fast -w stop >/dev/null 2>&1 || true
		fi
		for log in "$partial"/pg_upgrade_output.d/*/*.txt "$partial"/pg_upgrade_output.d/*/log/*.log "$work"/*.log; do
			if [ -s "$log" ]; then
				echo "--- ${log#"$parent"/}" >&2
				tail -n 40 "$log" >&2
			fi
		done
		rm -rf "$partial"
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

# ours says whether a postmaster.pid is one a server started here wrote, which gives pg_upgrade's port
# and this image's socket directory. A container of this image has its own process namespace, so
# such a server died with the container that started it, and no other container can be running it.
ours() {
	[ -f "$1" ] && [ "$(sed -n 4p "$1")" = 50432 ] && [ "$(sed -n 5p "$1")" = "$work" ]
}

# An earlier run stopped between proving the new cluster and renaming it. Where the old one is still
# in place, it is the one in use, and the upgrade starts again from it rather than trust a copy that
# anything since may have made stale; where it was already renamed, the new one takes its place.
if [ -e "$ready" ]; then
	if [ -s "$dir/PG_VERSION" ]; then
		say "$name-$new.ready was left by an upgrade stopped before $name was renamed: removing it, and upgrading $name again"
		rm -rf "$ready"
	elif [ ! -e "$dir" ] || rmdir "$dir" 2>/dev/null; then
		mv "$ready" "$dir"
		say "an earlier upgrade was stopped as it renamed the clusters: $name-$new.ready is now $name, on PostgreSQL $new"
		exit 0
	else
		fail "$name-$new.ready is an upgraded cluster waiting to take the place of $name, which holds files but no cluster: nothing was changed; move one of them away"
	fi
fi
if [ -e "$partial" ]; then
	say "removing $name-$new.partial, left by an upgrade that did not finish"
	rm -rf "$partial"
fi

if [ ! -s "$dir/PG_VERSION" ]; then
	say "$name holds no cluster yet, which PostgreSQL $new initialises: nothing to upgrade"
	exit 0
fi
old=$(cat "$dir/PG_VERSION")
case $old in
'' | *[!0-9]*) fail "$name/PG_VERSION reads \"$old\", which is no major version of PostgreSQL: nothing was changed" ;;
esac
if [ "$old" -eq "$new" ]; then
	say "$name is on PostgreSQL $new already: nothing to upgrade"
	exit 0
fi
if [ "$old" -gt "$new" ]; then
	fail "$name was written by PostgreSQL $old, newer than $new, the newest this image carries: it is this image that is too old, and nothing was changed"
fi
if [ ! -x "/usr/libexec/postgresql$old/postgres" ]; then
	fail "$name was written by PostgreSQL $old, and this image carries $(echo $majors | sed 's/ /, /g') alone: upgrade through a release whose image carries $old first; nothing was changed"
fi
oldbin=/usr/libexec/postgresql$old

# The cluster the official image made is owned by its postgres, 70, and PostgreSQL refuses a data
# directory its own account does not own.
owner=$(stat -c %u "$dir")
if [ "$owner" != "$(id -u "$account")" ]; then
	fail "$name is owned by uid $owner, where PostgreSQL's official image owns its data as uid $(id -u "$account"): nothing was changed"
fi

# Renaming is atomic only within one file system, and mv would copy a directory across two and then
# delete it: a cluster that is a mount of its own is refused rather than moved so.
if [ "$(stat -c %d "$dir")" != "$(stat -c %d "$parent")" ]; then
	fail "$name is mounted on its own, and the old cluster cannot be kept beside it: give this container the directory holding $name; nothing was changed"
fi

# A postmaster.pid is a server running on the cluster, or one that stopped without shutting down. A
# server in another container, such as PostgreSQL's own, cannot be told from a dead one from here,
# since its process is in another namespace, and pg_upgrade would take it for dead and start a second
# server on the same files: refused. A server started here and killed with its container is recovered
# as pg_upgrade would, by starting it once so that it replays its log, and stopping it.
if [ -e "$dir/postmaster.pid" ]; then
	if ! ours "$dir/postmaster.pid"; then
		fail "$name holds a postmaster.pid: PostgreSQL $old is running on it, or stopped without shutting down, and an upgrade now could lose what it had not written. Nothing was changed: let it shut down, starting it once more with the previous compose.yaml if it crashed, then run docker compose up -d again"
	fi
	say "$name holds the postmaster.pid of a server an earlier upgrade started and did not stop: starting it once to recover, and stopping it"
	mkdir -p -m 700 "$work" && chown "$account:$account" "$work"
	printf 'local all all trust\n' >"$work/pg_hba.conf"
	as_postgres "$oldbin/pg_ctl" -D "$dir" -w -t 300 -l "$work/recovery.log" \
		-o "-p 50432 -c listen_addresses='' -c unix_socket_directories='$work' -c hba_file='$work/pg_hba.conf'" start >/dev/null ||
		fail "PostgreSQL $old did not start on $name to recover it, as its log below says: nothing was changed"
	as_postgres "$oldbin/pg_ctl" -D "$dir" -m fast -w -t 300 stop >/dev/null
fi

# Copy mode needs room for a second cluster beside the first, and a disk filled halfway through fills
# it for every other service on the host too: checked first, with room for the new cluster's own WAL.
size=$(du -sk "$dir" | cut -f1)
free=$(df -Pk "$parent" | awk 'NR == 2 { print $4 }')
if [ "$free" -lt $((size + 131072)) ]; then
	fail "upgrading $name copies its $((size / 1024)) MiB beside it, and $((free / 1024)) MiB are free there: nothing was changed"
fi

# What pg_upgrade cannot change once initdb has written it, and requires to match: the checksums
# and the WAL segment size, read from the old cluster. Encoding and locale it does not require:
# since PostgreSQL 16 it gives the new template0 the old one's and recreates every other database
# with its own, and LANG, set as the official image sets it, gives initdb the same defaults anyway.
control=$(LC_ALL=C su-exec "$account" "$oldbin/pg_controldata" "$dir") || fail "pg_controldata $old could not read $name: nothing was changed"
checksums=$(echo "$control" | sed -n 's/^Data page checksum version: *//p')
segment=$(echo "$control" | sed -n 's/^Bytes per WAL segment: *//p')
case $checksums in
0) checksums=--no-data-checksums ;;
[1-9]*) checksums=--data-checksums ;;
*) fail "pg_controldata $old says nothing of $name's data checksums: nothing was changed" ;;
esac
case $segment in
'' | *[!0-9]*) fail "pg_controldata $old says nothing of $name's WAL segment size: nothing was changed" ;;
esac

say "upgrading $name from PostgreSQL $old to $new, in $name-$new.partial beside it"
mkdir -p -m 700 "$work" && chown "$account:$account" "$work"
printf 'local all all trust\n' >"$work/pg_hba.conf"
mkdir -m 700 "$partial" && chown "$account:$account" "$partial"
as_postgres "$newbin/initdb" -D "$partial" --username="$user" --auth=trust \
	--wal-segsize=$((segment / 1048576)) "$checksums" >"$work/initdb.log" ||
	fail "initdb $new could not create the new cluster, as its log below says: nothing was changed"

# pg_upgrade connects over the socket in $work, to servers told to read $work/pg_hba.conf, so that
# neither cluster's own pg_hba.conf, whatever the person made of it, stands in its way. Its --check
# changes nothing; the run after it copies. What it says is printed where it fails: where it
# succeeds, its advice to gather statistics is followed below, and its script to delete the old
# cluster is left in this container, which is removed.
cd "$work"
set -- --old-bindir="$oldbin" --new-bindir="$newbin" --old-datadir="$dir" --new-datadir="$partial" \
	--username="$user" --socketdir="$work" --copy \
	--old-options="-c hba_file=$work/pg_hba.conf" --new-options="-c hba_file=$work/pg_hba.conf"
as_postgres "$newbin/pg_upgrade" --check "$@" >"$work/pg_upgrade-check.log" ||
	fail "pg_upgrade --check found $name cannot be upgraded to PostgreSQL $new as it is, as it says below: nothing was changed"
as_postgres "$newbin/pg_upgrade" "$@" >"$work/pg_upgrade.log" ||
	fail "pg_upgrade could not upgrade $name to PostgreSQL $new, as it says below: nothing was changed"

# pg_upgrade leaves initdb's configuration, which is not what the official image wrote: that listens
# on every address and lets its clients in by POSTGRES_HOST_AUTH_METHOD, and a person may have
# changed it since. The old cluster's files are taken whole.
for file in postgresql.conf postgresql.auto.conf pg_hba.conf pg_ident.conf; do
	if [ -e "$dir/$file" ]; then cp -p "$dir/$file" "$partial/$file"; fi
done

# The new cluster started once as PostgreSQL's image will start it, on its own configuration, here on
# the socket alone, holding every database the old one holds: one directory under base/ each, named
# by an OID pg_upgrade keeps. The statistics pg_upgrade does not carry over are gathered while
# nothing else runs, as it advises, rather than left to the first queries.
query() { as_postgres "$newbin/psql" -h "$work" -p 50432 -U "$user" -d template1 -XAtqc "$1"; }
before=$(ls "$dir/base" | grep -E '^[0-9]+$' | sort -n | tr '\n' ' ')
as_postgres "$newbin/pg_ctl" -D "$partial" -w -t 300 -l "$work/new.log" \
	-o "-p 50432 -c listen_addresses='' -c unix_socket_directories='$work' -c hba_file='$work/pg_hba.conf'" start >/dev/null ||
	fail "PostgreSQL $new did not start on the upgraded cluster with $name's configuration, as its log below says: nothing was changed"
after=$(query "select string_agg(oid::text, ' ' order by oid) || ' ' from pg_database") || after=
if [ "$before" != "$after" ]; then
	fail "the upgraded cluster holds the databases $after, where $name holds $before: nothing was changed"
fi
databases=$(query "select string_agg(datname, ', ' order by datname) from pg_database")
as_postgres "$newbin/vacuumdb" -h "$work" -p 50432 -U "$user" --all --analyze-only --missing-stats-only --quiet ||
	fail "vacuumdb could not gather the statistics of the upgraded cluster: nothing was changed"
as_postgres "$newbin/pg_ctl" -D "$partial" -m fast -w -t 300 stop >/dev/null

# The two renames, the new cluster first marked ready so that a run stopped between them knows what
# it finds. A name the old cluster should take that is already taken, by one kept at an earlier
# upgrade the person brought back, gets the time beside it rather than overwrite anything.
kept=$name-$old
if [ -e "$parent/$kept" ]; then kept=$name-$old-$(date -u +%Y%m%dT%H%M%SZ); fi
mv "$partial" "$ready"
if ! mv "$dir" "$parent/$kept"; then
	rm -rf "$ready"
	fail "$name could not be renamed $kept: nothing was changed"
fi
if ! mv "$ready" "$dir"; then
	mv "$parent/$kept" "$dir" || true
	fail "the upgraded cluster could not be renamed $name, and $name is back as it was"
fi
say "$name is on PostgreSQL $new, with the databases $databases; the PostgreSQL $old cluster is kept beside it as $kept, as the previous release left it: the way back"
