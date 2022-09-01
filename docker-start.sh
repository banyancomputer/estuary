#/bin/bash

#This is the script that is run when the docker container is started.
# Please see the Dockerfile for more information on the context of this script.

# This is the WORKDIR of the docker container.
WORKDIR=/app

FULLNODE_API=$FULLNODE_API
ESTUARY_HOST=$ESTUARY_HOST
ESTUARY_PORT=$ESTUARY_PORT
ESTUARY_WWW_HOST=$ESTUARY_WWW_HOST
ESTUARY_WWW_PORT=$ESTUARY_WWW_PORT

if [ -z "$FULLNODE_API" ]; then
    echo "FULLNODE_API is empty, use default value"
    FULLNODE_API="wss://api.chain.love"
fi

echo "HOSTNAME: $ESTUARY_HOSTNAME:$ESTUARY_PORT"
echo "FULLNODE_API: $FULLNODE_API"

# Setup a new node and record the Estuary Token
mkdir -p $WORKDIR/data
mkdir -p $WORKDIR/private
ESTUARY_TOKEN=$($WORKDIR/estuary setup --username=admin | grep Token | cut -d ' ' -f 3)
echo $ESTUARY_TOKEN > $WORKDIR/private/token
echo "Estuary Admin Key: $ESTUARY_TOKEN"

# This is needed to make sure we dont get 'too many open files' errors
ulimit -n 10000

# Start the Estuary node
$WORKDIR/estuary $ESTUARY_HOSTNAME