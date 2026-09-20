#!/bin/sh
# Start script for the colocated demo image (deploy/demo.Dockerfile).
#
# Postgres and the service share one Cloud Run container. Everything is
# ephemeral: the database lives on the container filesystem, so a restart,
# a new revision, or a scale-to-zero wipes it. That is the documented
# trade-off of this image — a live demo with no managed database behind it.
set -eu

export PGDATA="${PGDATA:-/var/lib/postgresql/data}"
DEMO_DB="${DEMO_DB:-keel}"
DEMO_USER="${DEMO_USER:-keel}"

if [ ! -s "$PGDATA/PG_VERSION" ]; then
	# First boot on this container: create the cluster and the role/db.
	# The password is random per boot and never leaves the container.
	export DEMO_PASSWORD="$(openssl rand -hex 16)"
	initdb -D "$PGDATA" -U postgres --auth=trust >/dev/null
	printf "host all all 127.0.0.1/32 trust\n" >> "$PGDATA/pg_hba.conf"
	pg_ctl -D "$PGDATA" -o "-c listen_addresses='127.0.0.1'" -l /tmp/pg.log start
	until pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; do sleep 0.2; done
	psql -h 127.0.0.1 -U postgres -c "CREATE USER $DEMO_USER WITH PASSWORD '$DEMO_PASSWORD';" >/dev/null
	psql -h 127.0.0.1 -U postgres -c "CREATE DATABASE $DEMO_DB OWNER $DEMO_USER;" >/dev/null
	# Persist the password next to the cluster so a container *restart* (same
	# filesystem) reconnects; a fresh container reinitializes above instead.
	printf '%s' "$DEMO_PASSWORD" > "$PGDATA/.demo-password"
else
	export DEMO_PASSWORD="$(cat "$PGDATA/.demo-password")"
	pg_ctl -D "$PGDATA" -o "-c listen_addresses='127.0.0.1'" -l /tmp/pg.log start
	until pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; do sleep 0.2; done
fi

export DATABASE_URL="postgres://${DEMO_USER}:${DEMO_PASSWORD}@127.0.0.1:5432/${DEMO_DB}?sslmode=disable"
# Cloud Run injects PORT; default keeps `docker run` working locally.
export PORT="${PORT:-8080}"

exec /app/service
