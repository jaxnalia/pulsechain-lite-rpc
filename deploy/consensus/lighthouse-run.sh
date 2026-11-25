#!/bin/sh
# Runs lighthouse-pulse's beacon node with a bounded database.
#
# lighthouse keeps every block since its checkpoint plus a ~17-week
# historical backfill (several GB), and this release cannot disable the
# backfill. An RPC node needs none of it, so with
#   LH_FRESH_START=true  every start begins from a fresh checkpoint sync
#   LH_DB_BUDGET_MB=N    the database is re-checkpointed whenever it
#                        outgrows N MB (checked every LH_CHECK_SECONDS)
# Both are off (0/false) for the experimental validator modes.
#
# lighthouse-pulse v2.5.1 panics on some discv5 records circulating on the
# network, which shuts the beacon node down. The addresses of connected
# outbound peers are saved every 10 s; when lighthouse exits on its own it is
# restarted here on its existing database (no re-checkpoint) with discovery
# disabled, dialing those peers. Once fewer than LH_MIN_PEERS remain, it is
# restarted with discovery on to find new ones.
set -u

DATADIR=/blockchain/consensus/lighthouse
PEERS=$DATADIR/known-peers.txt
FRESH=${LH_FRESH_START:-false}
BUDGET_MB=${LH_DB_BUDGET_MB:-0}
CHECK=${LH_CHECK_SECONDS:-300}
MIN_PEERS=${LH_MIN_PEERS:-3}

# api PATH prints the body of GET http://127.0.0.1:3500PATH (the image has no
# curl or wget).
api() {
  perl -MIO::Socket::INET -e '
    alarm 10;
    my $s = IO::Socket::INET->new(PeerAddr => "127.0.0.1:3500", Timeout => 5) or exit 1;
    print $s "GET $ARGV[0] HTTP/1.0\r\nHost: localhost\r\n\r\n";
    local $/; my $r = <$s> // ""; $r =~ s/^.*?\r\n\r\n//s; print $r;' "$1" 2>/dev/null
}

# save_peers merges the connected outbound peers into $PEERS (newest first,
# at most 64; "<peer id> <multiaddr>" per line). The file is left unchanged
# when no peer is connected.
save_peers() {
  api "/eth/v1/node/peers?state=connected&direction=outbound" | perl -e '
    my $f = shift; local $/; my $j = <STDIN> // "";
    my (@out, %seen);
    while ($j =~ /\{[^{}]*\}/g) {
      my $o = $&;
      my ($id) = $o =~ /"peer_id":"([^"]+)"/;
      my ($a) = $o =~ /"last_seen_p2p_address":"([^"]+)"/;
      next unless $id && $a && !$seen{$id}++;
      $a .= "/p2p/$id" unless $a =~ m{/p2p/};
      push @out, "$id $a";
    }
    exit 0 unless @out;
    if (open my $in, "<", $f) {
      while (<$in>) { chomp; my ($id) = split / /; push @out, $_ if $id && !$seen{$id}++ }
    }
    splice @out, 64 if @out > 64;
    open my $w, ">", "$f.tmp" or exit 1;
    print $w "$_\n" for @out;
    close $w; rename "$f.tmp", $f;' "$PEERS"
}

peer_count() {
  api /eth/v1/node/peer_count | sed -n 's/.*"connected":"\([0-9]*\)".*/\1/p'
}

purge=""
[ "$FRESH" = true ] && purge="--purge-db"
discovery=on
[ -s "$PEERS" ] && discovery=off
pid=""
trap '[ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null; [ -n "$pid" ] && wait "$pid"; exit 0' TERM INT

while :; do
  [ -n "$purge" ] && echo "lighthouse-run: starting from a fresh checkpoint sync (--purge-db)"
  disc=""
  if [ "$discovery" = off ]; then
    disc="--disable-discovery --libp2p-addresses=$(cut -d' ' -f2 "$PEERS" | paste -sd, -)"
    echo "lighthouse-run: discovery off, dialing $(wc -l < "$PEERS") known peers"
  fi
  # shellcheck disable=SC2086 # word lists
  lighthouse bn \
    --network=pulsechain \
    --datadir="$DATADIR" \
    --execution-endpoint=http://execution:8551 \
    --execution-jwt=/blockchain/jwt.hex \
    --checkpoint-sync-url=https://checkpoint.pulsechain.com \
    --http --http-address=0.0.0.0 --http-port=3500 \
    --port=13000 \
    --target-peers="${CL_TARGET_PEERS:-50}" \
    --prune-payloads=true \
    --logfile-max-number=0 \
    --suggested-fee-recipient="${FEE_RECIPIENT:-0x0000000000000000000000000000000000000000}" \
    $purge $disc ${LIGHTHOUSE_EXTRA_FLAGS:-} &
  pid=$!
  purge=""
  over=""
  refresh=""
  waited=0
  up=0
  while kill -0 "$pid" 2>/dev/null; do
    sleep 10 &
    wait $!
    waited=$((waited + 10))
    up=$((up + 10))
    save_peers
    if [ "$discovery" = off ] && [ "$up" -ge 120 ] && [ $((up % 60)) -eq 0 ]; then
      n=$(peer_count)
      if [ "${n:-0}" -lt "$MIN_PEERS" ]; then
        echo "lighthouse-run: ${n:-0} peers left; restarting with discovery to find more"
        kill -TERM "$pid"
        wait "$pid"
        refresh=1
        break
      fi
    fi
    [ "$waited" -ge "$CHECK" ] || continue
    waited=0
    [ "$BUDGET_MB" -gt 0 ] 2>/dev/null || continue
    used=$(du -sm "$DATADIR/beacon" 2>/dev/null | cut -f1)
    if [ "${used:-0}" -gt "$BUDGET_MB" ]; then
      echo "lighthouse-run: database ${used} MB exceeds LH_DB_BUDGET_MB=${BUDGET_MB}; restarting from a fresh checkpoint"
      kill -TERM "$pid"
      wait "$pid"
      over=1
      break
    fi
  done
  if [ -n "$over" ]; then
    purge="--purge-db"
    continue
  fi
  if [ -n "$refresh" ]; then
    discovery=on
    continue
  fi
  wait "$pid"
  rc=$?
  discovery=on
  [ -s "$PEERS" ] && discovery=off
  echo "lighthouse-run: lighthouse exited (code $rc); restarting on the existing database"
  sleep 5 &
  wait $!
done
